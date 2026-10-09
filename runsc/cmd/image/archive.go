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
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
)

// Container engines (containerd, CRI-O, Podman) export the checkpoint of a
// container as a tar archive, possibly compressed, which checkpointctl
// (github.com/checkpoint-restore/checkpointctl) reads: the runtime's
// checkpoint in its checkpoint/ directory, the container's OCI spec in
// spec.dump, and the engine's description of the container in config.dump.
// When the runtime is runsc, checkpoint/ is a runsc checkpoint image.
const (
	archiveCheckpointDir = "checkpoint"
	archiveSpecDump      = "spec.dump"
	archiveConfigDump    = "config.dump"
)

// containerInfo describes the container whose checkpoint an archive holds, as
// checkpointctl does.
type containerInfo struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Engine    string `json:"engine"`
	Runtime   string `json:"runtime,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Pod       string `json:"pod,omitempty"`
}

// archive is a checkpoint archive whose image has been extracted.
type archive struct {
	// path is the archive's path.
	path string

	// dir is the temporary directory that the archive was extracted into.
	dir string

	// container describes the checkpointed container.
	container containerInfo
}

// imageDir returns the directory of the archive's image.
func (a *archive) imageDir() string {
	return filepath.Join(a.dir, archiveCheckpointDir)
}

// displayPath returns how to show p, a path of the extracted archive, to the
// user: as the path in the archive.
func (a *archive) displayPath(p string) string {
	if rel, err := filepath.Rel(a.dir, p); err == nil && filepath.IsLocal(rel) {
		return a.path + ":" + rel
	}
	return p
}

// remove removes the extracted archive.
func (a *archive) remove() {
	os.RemoveAll(a.dir)
}

// extractArchive extracts the checkpoint archive at path into a temporary
// directory: its checkpoint/ directory, spec.dump and config.dump. Regular
// files and directories are extracted; other entries, such as symbolic links,
// are not. The data of pages files is extracted only if withPages is true;
// otherwise they are extracted as sparse files of their size, which is all that
// commands that do not read pages look at.
func extractArchive(archivePath string, withPages bool) (*archive, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, closeReader, err := decompress(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", archivePath, err)
	}
	defer closeReader()

	dir, err := os.MkdirTemp("", "runsc-image-")
	if err != nil {
		return nil, err
	}
	a := &archive{path: archivePath, dir: dir}
	extracted := false
	defer func() {
		if !extracted {
			a.remove()
		}
	}()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", archivePath, err)
		}
		// Cleaned, the names of the entries to extract cannot lead out of
		// dir.
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name != archiveSpecDump && name != archiveConfigDump && name != archiveCheckpointDir && !strings.HasPrefix(name, archiveCheckpointDir+"/") {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return nil, err
			}
			data := io.Reader(tr)
			if !withPages && filepath.Base(name) == checkpointfiles.PagesFileName {
				data = nil
			}
			if err := extractFile(dst, data, hdr.Size); err != nil {
				return nil, fmt.Errorf("extracting %s from %s: %w", hdr.Name, archivePath, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(a.imageDir(), checkpointfiles.PagesMetadataFileName)); err != nil {
		return nil, fmt.Errorf("%s has no %s/%s: it is not a checkpoint archive of an uncompressed runsc checkpoint", archivePath, archiveCheckpointDir, checkpointfiles.PagesMetadataFileName)
	}
	if a.container, err = readContainerInfo(dir); err != nil {
		return nil, fmt.Errorf("%s: %w", archivePath, err)
	}
	extracted = true
	return a, nil
}

// extractFile creates the file dst with the size bytes of data, or, if data is
// nil, as a sparse file of that size.
func extractFile(dst string, data io.Reader, size int64) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if data == nil {
		return f.Truncate(size)
	}
	if _, err := io.CopyN(f, data, size); err != nil {
		return err
	}
	return nil
}

// Magic numbers of the compression formats of checkpoint archives.
var (
	gzipMagic  = []byte{0x1f, 0x8b}
	zstdMagic  = []byte{0x28, 0xb5, 0x2f, 0xfd}
	bzip2Magic = []byte("BZh")
	xzMagic    = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}
)

// decompress returns a reader of the tar archive that f holds, uncompressed,
// gzip- or zstd-compressed, and a function that releases it. An uncompressed
// archive is read from f itself, so that the tar reader seeks over the data
// of the entries that are not extracted rather than reading it.
func decompress(f *os.File) (io.Reader, func(), error) {
	magic := make([]byte, len(xzMagic))
	n, err := f.ReadAt(magic, 0)
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	magic = magic[:n]
	switch {
	case bytes.HasPrefix(magic, gzipMagic):
		r, err := gzip.NewReader(bufio.NewReader(f))
		if err != nil {
			return nil, nil, err
		}
		return r, func() { r.Close() }, nil
	case bytes.HasPrefix(magic, zstdMagic):
		d, err := zstd.NewReader(bufio.NewReader(f))
		if err != nil {
			return nil, nil, err
		}
		return d, d.Close, nil
	case bytes.HasPrefix(magic, bzip2Magic), bytes.HasPrefix(magic, xzMagic):
		return nil, nil, errors.New("bzip2 and xz archives are not supported: decompress the archive first")
	default:
		return f, func() {}, nil
	}
}

// readContainerInfo reads what spec.dump and config.dump, in the extracted
// archive dir, say of the container, as checkpointctl does
// (internal/container.go): the engine is told by the spec's
// io.container.manager annotation, and each engine records the container's
// name in its own place.
func readContainerInfo(dir string) (containerInfo, error) {
	var spec specs.Spec
	if err := readJSON(dir, archiveSpecDump, &spec); err != nil {
		return containerInfo{}, err
	}
	// The part of Podman's container configuration that checkpointctl reads;
	// containerd writes the same fields.
	var config struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Runtime string `json:"runtime"`
	}
	if err := readJSON(dir, archiveConfigDump, &config); err != nil {
		return containerInfo{}, err
	}
	ci := containerInfo{ID: config.ID, Runtime: config.Runtime}
	annotations := spec.Annotations
	switch annotations["io.container.manager"] {
	case "libpod":
		ci.Engine = "Podman"
		ci.Name = config.Name
	case "cri-o":
		ci.Engine = "CRI-O"
		var md struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(annotations["io.kubernetes.cri-o.Metadata"]), &md); err != nil {
			return containerInfo{}, fmt.Errorf("reading the io.kubernetes.cri-o.Metadata annotation: %w", err)
		}
		ci.Name = md.Name
		ci.Namespace = annotations["io.kubernetes.pod.namespace"]
		ci.Pod = annotations["io.kubernetes.pod.name"]
	default:
		ci.Engine = "containerd"
		ci.Name = annotations["io.kubernetes.cri.container-name"]
		ci.Namespace = annotations["io.kubernetes.cri.sandbox-namespace"]
		ci.Pod = annotations["io.kubernetes.cri.sandbox-name"]
	}
	return ci, nil
}

// readJSON reads the JSON file name of the extracted archive dir into v.
func readJSON(dir, name string, v any) error {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the archive has no %s", name)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
