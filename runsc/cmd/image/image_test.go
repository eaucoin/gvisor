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
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/google/subcommands"
	"golang.org/x/sys/unix"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/version"
)

var update = flag.Bool("update", false, "update the golden files in testdata/")

const page = checkpointimage.PageSize

// writeStateFile writes a state file with metadata md and no data.
func writeStateFile(t *testing.T, path string, md map[string]string) {
	t.Helper()
	data, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.WriteString("gVisorSF")
	binary.Write(&b, binary.BigEndian, uint64(len(data)))
	b.Write(data)
	if err := os.WriteFile(path, b.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeImage writes, in the image directory dir, an image of a MemoryFile of
// n pages, the first of which is zero and the others random. If parent is not
// nil, the image's pages [0, shared) refer to parent's, which must hold the
// same memory.
func writeImage(t *testing.T, dir string, seed int64, n, shared uint64, parent *checkpointimage.Image) *checkpointimage.Image {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	mf := &pb.MemoryFileMetadataProto{
		Version: checkpointimage.MemoryFileMetadataVersion,
		Chunks:  []*pb.ChunkInfoProto{{}},
		MemAcct: []*pb.MemAcctRangeProto{{Start: 0, End: n * page, KnownCommitted: true}},
	}
	ip := &pb.ImageProto{
		Layers:   []*pb.LayerProto{{}},
		PageHash: checkpointimage.PageHashXXH64,
		WorkingSet: &pb.WorkingSetProto{
			Version:  checkpointimage.WorkingSetVersion,
			Unit:     page,
			WindowNs: 3e9,
			Extents:  []*pb.FileRangeProto{{Start: 2 * page, End: 4 * page}, {Start: page, End: 2 * page}},
		},
	}
	var pm *checkpointimage.MemoryFileImage
	if parent != nil {
		pm = parent.MemoryFileImage(0)
		ip.Layers = append(ip.Layers, &pb.LayerProto{Digest: bytes.Clone(parent.Digest[:]), PagesSize: parent.Layers()[0].PagesSize})
	}
	var pages []byte
	for i := uint64(0); i < n; i++ {
		off := i * page
		pg := make([]byte, page)
		if i != 0 {
			rng.Read(pg)
		}
		if i < shared {
			h, _ := pm.HashAt(off)
			mf.PageHashes = binary.LittleEndian.AppendUint64(mf.PageHashes, h)
			if e, ok := pm.ExtentAt(off); ok {
				mf.Extents = appendExtent(mf.Extents, off, 1, e.OffsetOf(off))
			}
			continue
		}
		mf.PageHashes = binary.LittleEndian.AppendUint64(mf.PageHashes, xxhash.Sum64(pg))
		if i == 0 {
			continue
		}
		mf.Extents = appendExtent(mf.Extents, off, 0, uint64(len(pages)))
		pages = append(pages, pg...)
	}
	meta, err := os.Create(filepath.Join(dir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	w := checkpointimage.NewWriter(meta, ip)
	if err := checkpointimage.WriteRecord(w, mf); err != nil {
		t.Fatal(err)
	}
	img, err := w.Finish(uint64(len(pages)))
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointfiles.PagesFileName), pages, 0644); err != nil {
		t.Fatal(err)
	}
	writeStateFile(t, filepath.Join(dir, checkpointfiles.StateFileName), map[string]string{
		boot.VersionKey:                   version.Version(),
		checkpointimage.FormatMetadataKey: checkpointimage.FormatVersion(),
		boot.ContainerSpecsKey:            "{...}",
		"timestamp":                       "2026-10-08 12:00:00",
		"_internal":                       "x",
	})
	return img
}

// appendExtent appends the page at off, in layer at offset, to extents, as
// checkpointimage writers do: merged with the last extent if contiguous.
func appendExtent(extents []*pb.ExtentProto, off uint64, layer uint32, offset uint64) []*pb.ExtentProto {
	if n := len(extents); n != 0 {
		if last := extents[n-1]; last.End == off && last.Layer == layer && last.Offset+(last.End-last.Start) == offset {
			last.End += page
			return extents
		}
	}
	return append(extents, &pb.ExtentProto{Start: off, End: off + page, Layer: layer, Offset: offset})
}

// testImages writes a template image and an image whose first pages are in the
// template, in its layers/ directory.
func testImages(t *testing.T) (root string, template, delta *checkpointimage.Image) {
	root = t.TempDir()
	template = writeImage(t, filepath.Join(root, "template"), 1, 8, 0, nil)
	deltaDir := filepath.Join(root, "delta")
	delta = writeImage(t, deltaDir, 1, 12, 8, template)
	if err := os.MkdirAll(filepath.Join(deltaDir, checkpointimage.LayersDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "template"), filepath.Join(deltaDir, checkpointimage.LayersDir, template.Digest.String())); err != nil {
		t.Fatal(err)
	}
	return root, template, delta
}

// run runs cmd with args, and returns its exit status and output.
func run(t *testing.T, cmd subcommands.Command, args ...string) (subcommands.ExitStatus, string) {
	t.Helper()
	fs := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
	cmd.SetFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %q: %v", args, err)
	}
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	status := cmd.Execute(context.Background(), fs)
	w.Close()
	os.Stdout = stdout
	return status, <-out
}

func TestInspectJSON(t *testing.T) {
	root, _, _ := testImages(t)
	status, out := run(t, new(inspect), "--json", filepath.Join(root, "delta"))
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect: %v", status)
	}
	out = strings.ReplaceAll(out, root, "ROOT")
	// The version depends on how the test is built.
	out = strings.ReplaceAll(out, fmt.Sprintf("%q", version.Version()), `"VERSION"`)
	golden := filepath.Join("testdata", "inspect.json")
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
}

func TestInspectText(t *testing.T) {
	root, template, delta := testImages(t)
	status, out := run(t, new(inspect), filepath.Join(root, "delta"))
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect: %v", status)
	}
	for _, want := range []string{
		"Image:          " + delta.Digest.String() + "\n",
		"Format:         2.0\n",
		"image_format:   2.0\n",
		"  1  " + template.Digest.String() + "  ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect printed:\n%s\nwhich lacks %q", out, want)
		}
	}
}

// TestInspectParent checks that inspect shows the parent that an incremental
// checkpoint's state file names.
func TestInspectParent(t *testing.T) {
	root, template, _ := testImages(t)
	deltaDir := filepath.Join(root, "delta")
	writeStateFile(t, filepath.Join(deltaDir, checkpointfiles.StateFileName), map[string]string{
		boot.VersionKey:                   version.Version(),
		checkpointimage.FormatMetadataKey: checkpointimage.FormatVersion(),
		checkpointimage.ParentMetadataKey: template.Digest.String(),
	})
	status, out := run(t, new(inspect), deltaDir)
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect: %v", status)
	}
	if want := "parent_id:      " + template.Digest.String() + "\n"; !strings.Contains(out, want) {
		t.Errorf("inspect printed:\n%s\nwhich lacks %q", out, want)
	}
	status, out = run(t, new(inspect), "--json", deltaDir)
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect --json: %v", status)
	}
	var info imageInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("inspect --json printed %q: %v", out, err)
	}
	if got, want := info.State.Metadata[checkpointimage.ParentMetadataKey], template.Digest.String(); got != want {
		t.Errorf("inspect --json: parent %q, want %q", got, want)
	}
}

func TestVerify(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, root string, template *checkpointimage.Image)
		args    []string
		want    subcommands.ExitStatus
	}{
		{name: "intact", args: []string{"--pages", "--host"}, want: subcommands.ExitSuccess},
		{
			name: "flipped byte in a layer",
			corrupt: func(t *testing.T, root string, _ *checkpointimage.Image) {
				flipByte(t, filepath.Join(root, "template", checkpointfiles.PagesFileName), page+7)
			},
			args: []string{"--pages"},
			want: exitInvalid,
		},
		{
			name: "flipped byte in the pages metadata",
			corrupt: func(t *testing.T, root string, _ *checkpointimage.Image) {
				flipByte(t, filepath.Join(root, "delta", checkpointfiles.PagesMetadataFileName), 40)
			},
			want: exitInvalid,
		},
		{
			name: "truncated layer",
			corrupt: func(t *testing.T, root string, _ *checkpointimage.Image) {
				if err := os.Truncate(filepath.Join(root, "template", checkpointfiles.PagesFileName), page); err != nil {
					t.Fatal(err)
				}
			},
			want: exitInvalid,
		},
		{
			name: "wrong layer",
			corrupt: func(t *testing.T, root string, template *checkpointimage.Image) {
				// Replace the template with another image.
				os.RemoveAll(filepath.Join(root, "template"))
				writeImage(t, filepath.Join(root, "template"), 2, 8, 0, nil)
			},
			want: exitLayerNotFound,
		},
		{
			name: "other runsc",
			corrupt: func(t *testing.T, root string, _ *checkpointimage.Image) {
				writeStateFile(t, filepath.Join(root, "delta", checkpointfiles.StateFileName), map[string]string{boot.VersionKey: "other"})
			},
			args: []string{"--host"},
			want: exitNotRestorable,
		},
		{
			name: "missing CPU feature",
			corrupt: func(t *testing.T, root string, _ *checkpointimage.Image) {
				writeStateFile(t, filepath.Join(root, "delta", checkpointfiles.StateFileName), map[string]string{
					boot.VersionKey:     version.Version(),
					boot.CPUFeaturesKey: "no-such-feature",
				})
			},
			args: []string{"--host"},
			want: exitNotRestorable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, template, _ := testImages(t)
			if tc.corrupt != nil {
				tc.corrupt(t, root, template)
			}
			args := append(tc.args, filepath.Join(root, "delta"))
			if status, out := run(t, new(verify), args...); status != tc.want {
				t.Errorf("verify %q = %v, want %v; output:\n%s", args, status, tc.want, out)
			}
		})
	}
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

func TestFlattenCompact(t *testing.T) {
	root, template, delta := testImages(t)
	deltaDir := filepath.Join(root, "delta")
	before, err := os.ReadFile(filepath.Join(deltaDir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}

	flat := filepath.Join(root, "flat")
	if status, out := run(t, new(flatten), "--output", flat, deltaDir); status != subcommands.ExitSuccess {
		t.Fatalf("flatten: %v\n%s", status, out)
	}
	img, err := checkpointimage.ReadMetadataFile(filepath.Join(flat, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(img.Layers()); n != 1 {
		t.Errorf("flattened image has %d layers", n)
	}
	if status, out := run(t, new(verify), "--pages", flat); status != subcommands.ExitSuccess {
		t.Errorf("verify of the flattened image: %v\n%s", status, out)
	}
	// The source image is untouched, and so is an existing output.
	after, err := os.ReadFile(filepath.Join(deltaDir, checkpointfiles.PagesMetadataFileName))
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("flatten changed the source image")
	}
	if status, _ := run(t, new(flatten), "--output", flat, deltaDir); status == subcommands.ExitSuccess {
		t.Errorf("flatten into an existing directory succeeded")
	}

	// With --working-set-first, the pages file starts with the working set's
	// pages, in its order.
	ws := filepath.Join(root, "ws")
	if status, out := run(t, new(flatten), "--output", ws, "--working-set-first", deltaDir); status != subcommands.ExitSuccess {
		t.Fatalf("flatten --working-set-first: %v\n%s", status, out)
	}
	if status, out := run(t, new(verify), "--pages", ws); status != subcommands.ExitSuccess {
		t.Errorf("verify of the image flattened working set first: %v\n%s", status, out)
	}
	img, err = checkpointimage.ReadMetadataFile(filepath.Join(ws, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	m := img.MemoryFileImage(0)
	var next uint64
	placed := make(map[uint64]bool)
	for _, fr := range img.Proto.GetWorkingSet().GetExtents() {
		for off := fr.GetStart(); off < fr.GetEnd(); off += page {
			e, ok := m.ExtentAt(off)
			if !ok || placed[off] {
				continue
			}
			placed[off] = true
			if got := e.OffsetOf(off); got != next {
				t.Errorf("working set page %#x is at %#x of the pages file, want %#x", off, got, next)
			}
			next += page
		}
	}
	if next == 0 {
		t.Errorf("no page of the working set has data")
	}

	kept := filepath.Join(root, "kept")
	if status, out := run(t, new(compact), "--output", kept, "--keep-layer", template.Digest.String(), deltaDir); status != subcommands.ExitSuccess {
		t.Fatalf("compact: %v\n%s", status, out)
	}
	img, err = checkpointimage.ReadMetadataFile(filepath.Join(kept, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if layers := img.Layers(); len(layers) != 2 || layers[1].Digest != template.Digest || layers[0].PagesSize != delta.Layers()[0].PagesSize {
		t.Errorf("compacted image layers = %+v", layers)
	}
	if status, out := run(t, new(verify), "--pages", "--layer-path", filepath.Join(root, "template"), kept); status != subcommands.ExitSuccess {
		t.Errorf("verify of the compacted image: %v\n%s", status, out)
	}

	// Rebasing the flattened image onto the template gives back the delta.
	rebased := filepath.Join(root, "rebased")
	if status, out := run(t, new(rebase), "--output", rebased, "--onto", filepath.Join(root, "template"), flat); status != subcommands.ExitSuccess {
		t.Fatalf("rebase: %v\n%s", status, out)
	}
	img, err = checkpointimage.ReadMetadataFile(filepath.Join(rebased, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != delta.Digest {
		t.Errorf("rebased image %v, want the delta %v", img.Digest, delta.Digest)
	}
}

func TestLayers(t *testing.T) {
	root, template, _ := testImages(t)
	status, out := run(t, new(layers), filepath.Join(root, "delta"))
	if status != subcommands.ExitSuccess || out != template.Digest.String()+"\n" {
		t.Errorf("layers = %v, %q; want %q", status, out, template.Digest.String()+"\n")
	}
}

// TestExitStatus checks that "runsc image" reports the exit status of its
// subcommand through the wait status, which runsc exits with, rather than
// failing: runsc exits with a status of its own when a command fails.
func TestExitStatus(t *testing.T) {
	root, _, _ := testImages(t)
	delta := filepath.Join(root, "delta")
	if err := os.RemoveAll(filepath.Join(delta, checkpointimage.LayersDir)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want subcommands.ExitStatus
	}{
		{args: []string{"layers", delta}, want: subcommands.ExitSuccess},
		{args: []string{"verify", delta}, want: exitLayerNotFound},
		{args: []string{"inspect"}, want: subcommands.ExitUsageError},
	} {
		fs := flag.NewFlagSet("image", flag.ContinueOnError)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		var ws unix.WaitStatus
		if status := new(Image).Execute(context.Background(), fs, &config.Config{}, &ws); status != subcommands.ExitSuccess {
			t.Errorf("image %q returned %v, want %v", tc.args, status, subcommands.ExitSuccess)
		}
		if !ws.Exited() || ws.ExitStatus() != int(tc.want) {
			t.Errorf("image %q: wait status %#x, want exit status %d", tc.args, ws, tc.want)
		}
	}
}
