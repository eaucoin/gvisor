// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/subcommands"
	"github.com/klauspost/compress/zstd"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/runsc/version"
)

// engine is what a container engine puts in a checkpoint archive besides the
// checkpoint, and how checkpointctl describes the container from it.
type engine struct {
	annotations map[string]string
	config      map[string]string
	want        containerInfo
}

// engines are the archives of containerd (Kubernetes' kubelet checkpoint API),
// CRI-O and Podman, as checkpointctl reads them.
var engines = map[string]engine{
	"containerd": {
		annotations: map[string]string{
			"io.kubernetes.cri.container-name":    "counter",
			"io.kubernetes.cri.sandbox-namespace": "default",
			"io.kubernetes.cri.sandbox-name":      "counter-pod",
		},
		config: map[string]string{"id": "0123456789abcdef", "name": "counter", "runtime": "io.containerd.runsc.v1"},
		want:   containerInfo{ID: "0123456789abcdef", Name: "counter", Engine: "containerd", Runtime: "io.containerd.runsc.v1", Namespace: "default", Pod: "counter-pod"},
	},
	"CRI-O": {
		annotations: map[string]string{
			"io.container.manager":         "cri-o",
			"io.kubernetes.cri-o.Metadata": `{"name":"counter","attempt":1}`,
			"io.kubernetes.pod.namespace":  "default",
			"io.kubernetes.pod.name":       "counter-pod",
		},
		config: map[string]string{"id": "0123456789abcdef", "name": "k8s_counter_counter-pod_default", "runtime": "runsc"},
		want:   containerInfo{ID: "0123456789abcdef", Name: "counter", Engine: "CRI-O", Runtime: "runsc", Namespace: "default", Pod: "counter-pod"},
	},
	"Podman": {
		annotations: map[string]string{"io.container.manager": "libpod"},
		config:      map[string]string{"id": "0123456789abcdef", "name": "counter", "runtime": "runsc"},
		want:        containerInfo{ID: "0123456789abcdef", Name: "counter", Engine: "Podman", Runtime: "runsc"},
	},
}

// writeArchive writes at path a checkpoint archive of a container of engine e
// whose checkpoint is the image directory image, compressed with compression
// ("", "gzip" or "zstd"). The entries' names start with prefix ("" or "./",
// as engines write them). extra holds more files, by name.
func writeArchive(t *testing.T, path, image string, e engine, compression, prefix string, extra map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var w io.WriteCloser = f
	switch compression {
	case "gzip":
		w = gzip.NewWriter(f)
	case "zstd":
		if w, err = zstd.NewWriter(f); err != nil {
			t.Fatal(err)
		}
	}
	tw := tar.NewWriter(w)
	add := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: prefix + name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	addJSON := func(name string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		add(name, data)
	}
	addJSON(archiveSpecDump, &specs.Spec{Version: "1.0.2", Annotations: e.annotations})
	addJSON(archiveConfigDump, e.config)
	if err := filepath.WalkDir(image, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(image, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join(archiveCheckpointDir, rel))
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: prefix + name + "/", Typeflag: tar.TypeDir, Mode: 0700})
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Name: prefix + name, Typeflag: tar.TypeSymlink, Linkname: target})
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			add(name, data)
			return nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	for name, data := range extra {
		add(name, []byte(data))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if w != io.WriteCloser(f) {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// inspectArchive runs "image inspect --json" on archive, and returns what it
// printed with ROOT for root and VERSION for the runsc version.
func inspectArchive(t *testing.T, root, archive string, args ...string) (*imageInfo, string) {
	t.Helper()
	status, out := run(t, new(inspect), append(append([]string{"--json"}, args...), archive)...)
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect %s: %v", archive, status)
	}
	var info imageInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("inspect printed %q: %v", out, err)
	}
	out = strings.ReplaceAll(out, root, "ROOT")
	out = strings.ReplaceAll(out, fmt.Sprintf("%q", version.Version()), `"VERSION"`)
	return &info, out
}

func TestInspectArchive(t *testing.T) {
	root, template, delta := testImages(t)
	deltaDir := filepath.Join(root, "delta")
	archive := filepath.Join(root, "checkpoint.tar")
	writeArchive(t, archive, deltaDir, engines["containerd"], "", "", nil)
	info, out := inspectArchive(t, root, archive, "--layer-path", filepath.Join(root, "template"))
	golden := filepath.Join("testdata", "inspect_archive.json")
	if *update {
		if err := os.WriteFile(golden, []byte(out), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("inspect --json printed:\n%s\nwant:\n%s", out, want)
	}
	// The layers/ directory of the image in the archive is a symbolic link,
	// which is not extracted: the template is found in the layer path.
	if info.Digest != delta.Digest.String() || len(info.Layers) != 2 || info.Layers[1].Path != filepath.Join(root, "template", checkpointfiles.PagesFileName) {
		t.Errorf("inspect printed image %s, layers %+v; want image %v with layer %v", info.Digest, info.Layers, delta.Digest, template.Digest)
	}

	for name, e := range engines {
		for _, compression := range []string{"", "gzip", "zstd"} {
			for _, prefix := range []string{"", "./"} {
				t.Run(fmt.Sprintf("%s %q %q", name, compression, prefix), func(t *testing.T) {
					archive := filepath.Join(t.TempDir(), "checkpoint.tar")
					writeArchive(t, archive, deltaDir, e, compression, prefix, nil)
					info, _ := inspectArchive(t, root, archive)
					if info.Archive == nil || info.Archive.Container != e.want {
						t.Errorf("inspect described the archive as %+v, want container %+v", info.Archive, e.want)
					}
					if info.Digest != delta.Digest.String() {
						t.Errorf("inspect printed image %s, want %v", info.Digest, delta.Digest)
					}
				})
			}
		}
	}

	t.Run("text", func(t *testing.T) {
		status, out := run(t, new(inspect), archive)
		if status != subcommands.ExitSuccess {
			t.Fatalf("inspect %s: %v", archive, status)
		}
		for _, want := range []string{
			"Archive:        " + archive + "\n",
			"Container:      counter\n",
			"Pod:            default/counter-pod\n",
			"Engine:         containerd\n",
			"Runtime:        io.containerd.runsc.v1\n",
			"16384  " + archive + ":checkpoint/pages.img\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("inspect printed:\n%s\nwhich lacks %q", out, want)
			}
		}
	})
}

func TestVerifyArchive(t *testing.T) {
	root, _, _ := testImages(t)
	templateDir := filepath.Join(root, "template")
	archive := filepath.Join(root, "checkpoint.tar.gz")
	writeArchive(t, archive, filepath.Join(root, "delta"), engines["CRI-O"], "gzip", "", nil)
	if status, out := run(t, new(verify), "--pages", "--host", "--layer-path", templateDir, archive); status != subcommands.ExitSuccess {
		t.Errorf("verify --pages --host: %v\n%s", status, out)
	} else if want := fmt.Sprintf("Checkpoint of CRI-O container \"counter\" in %s\n", archive); !strings.HasPrefix(out, want) {
		t.Errorf("verify printed %q, want it to start with %q", out, want)
	}
	if status, out := run(t, new(verify), archive); status != exitLayerNotFound {
		t.Errorf("verify without the layer path: %v, want %v\n%s", status, exitLayerNotFound, out)
	}

	// A flipped byte in the archive's pages file is found when pages are
	// read; verify reads no page data otherwise.
	flipByte(t, filepath.Join(root, "delta", checkpointfiles.PagesFileName), 7)
	writeArchive(t, archive, filepath.Join(root, "delta"), engines["CRI-O"], "gzip", "", nil)
	if status, out := run(t, new(verify), "--layer-path", templateDir, archive); status != subcommands.ExitSuccess {
		t.Errorf("verify: %v\n%s", status, out)
	}
	if status, out := run(t, new(verify), "--pages", "--layer-path", templateDir, archive); status != exitInvalid {
		t.Errorf("verify --pages of a flipped byte: %v, want %v\n%s", status, exitInvalid, out)
	}
}

func TestLayersArchive(t *testing.T) {
	root, template, _ := testImages(t)
	archive := filepath.Join(root, "checkpoint.tar.zst")
	writeArchive(t, archive, filepath.Join(root, "delta"), engines["Podman"], "zstd", "./", nil)
	if status, out := run(t, new(layers), archive); status != subcommands.ExitSuccess || out != template.Digest.String()+"\n" {
		t.Errorf("layers = %v, %q; want %q", status, out, template.Digest.String()+"\n")
	}
}

func TestArchiveRejects(t *testing.T) {
	root, _, _ := testImages(t)
	deltaDir := filepath.Join(root, "delta")
	escape := fmt.Sprintf("escape-%d", os.Getpid())
	for _, tc := range []struct {
		name  string
		write func(path string)
	}{
		{
			name: "CRIU checkpoint",
			write: func(path string) {
				writeArchive(t, path, t.TempDir(), engines["Podman"], "", "", map[string]string{"checkpoint/pages-1.img": "data"})
			},
		},
		{
			name: "no spec.dump",
			write: func(path string) {
				writeArchive(t, path, deltaDir, engines["Podman"], "", "", nil)
				// Rewrite it without its first entry, spec.dump.
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				tr := tar.NewReader(strings.NewReader(string(data)))
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				tw := tar.NewWriter(f)
				for {
					hdr, err := tr.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if hdr.Name == archiveSpecDump {
						continue
					}
					if err := tw.WriteHeader(hdr); err != nil {
						t.Fatal(err)
					}
					if _, err := io.Copy(tw, tr); err != nil {
						t.Fatal(err)
					}
				}
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "bzip2",
			write: func(path string) {
				if err := os.WriteFile(path, []byte("BZh91AY&SY"), 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "checkpoint.tar")
			tc.write(archive)
			if status, out := run(t, new(inspect), archive); status != subcommands.ExitFailure {
				t.Errorf("inspect: %v, want %v\n%s", status, subcommands.ExitFailure, out)
			}
		})
	}

	// Entries outside checkpoint/ are not extracted, even if their names
	// lead into it, nor out of the extraction directory.
	archive := filepath.Join(root, "checkpoint.tar")
	writeArchive(t, archive, deltaDir, engines["Podman"], "", "", map[string]string{"checkpoint/../../" + escape: "data"})
	if status, out := run(t, new(inspect), "--layer-path", filepath.Join(root, "template"), archive); status != subcommands.ExitSuccess {
		t.Errorf("inspect: %v\n%s", status, out)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), escape)); !os.IsNotExist(err) {
		os.Remove(filepath.Join(os.TempDir(), escape))
		t.Errorf("an archive entry was extracted outside the extraction directory: %v", err)
	}
}
