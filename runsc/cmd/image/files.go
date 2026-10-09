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
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/state/statefile"
)

// imageDir is an image directory that has been read.
type imageDir struct {
	path string
	img  *checkpointimage.Image

	// archive is the checkpoint archive that the image was extracted from, or
	// nil if it was not.
	archive *archive

	// layerDirs holds the directory of each layer i >= 1 at index i-1, or ""
	// if it was not found.
	layerDirs []string

	// state is the metadata of the state file, and stateSize its size.
	state     map[string]string
	stateSize int64
}

// openImage opens the image at path: an image directory, or a checkpoint
// archive of a container engine, which is extracted into a temporary directory
// (see extractArchive), with the data of its pages files only if withPages is
// true. It looks for the image's layers in its layers/ directory and in
// layerPaths. The caller must call close when done with the image.
func openImage(path string, layerPaths []string, withPages bool) (d *imageDir, close func(), err error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if st.IsDir() {
		d, err := openImageDir(path, layerPaths)
		return d, func() {}, err
	}
	a, err := extractArchive(path, withPages)
	if err != nil {
		return nil, nil, err
	}
	if d, err = openImageDir(a.imageDir(), layerPaths); err != nil {
		a.remove()
		return nil, nil, err
	}
	d.archive = a
	return d, a.remove, nil
}

// displayPath returns how to show p, a path of d or of its layers, to the
// user.
func (d *imageDir) displayPath(p string) string {
	if d.archive == nil {
		return p
	}
	return d.archive.displayPath(p)
}

// openImageDir reads the image in dir, and looks for its layers in
// dir/layers and in layerPaths.
func openImageDir(dir string, layerPaths []string) (*imageDir, error) {
	img, err := checkpointimage.ReadMetadataFile(filepath.Join(dir, checkpointfiles.PagesMetadataFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s has no %s: it is not an uncompressed checkpoint image", dir, checkpointfiles.PagesMetadataFileName)
	}
	if err != nil {
		return nil, err
	}
	d := &imageDir{path: dir, img: img}
	if d.layerDirs, err = checkpointimage.LocateLayers(img, dir, layerPaths); err != nil {
		return nil, err
	}
	sf, err := os.Open(filepath.Join(dir, checkpointfiles.StateFileName))
	if err != nil {
		return nil, err
	}
	defer sf.Close()
	if d.state, err = statefile.MetadataUnsafe(sf); err != nil {
		return nil, fmt.Errorf("reading %s: %w", sf.Name(), err)
	}
	st, err := sf.Stat()
	if err != nil {
		return nil, err
	}
	d.stateSize = st.Size()
	return d, nil
}

// pagesPath returns the path of the pages file of layer i, or "" if the layer
// was not found.
func (d *imageDir) pagesPath(i int) string {
	if i == 0 {
		return filepath.Join(d.path, checkpointfiles.PagesFileName)
	}
	if d.layerDirs[i-1] == "" {
		return ""
	}
	return filepath.Join(d.layerDirs[i-1], checkpointfiles.PagesFileName)
}

// openLayers opens the pages file of every layer. The caller must close
// them.
func (d *imageDir) openLayers() ([]*os.File, error) {
	var files []*os.File
	for i, l := range d.img.Layers() {
		path := d.pagesPath(i)
		if path == "" {
			closeAll(files)
			return nil, fmt.Errorf("layer %d (%v): %w in %s or the layer paths", i, l.Digest, checkpointimage.ErrLayerNotFound, filepath.Join(d.path, checkpointimage.LayersDir))
		}
		f, err := os.Open(path)
		if err != nil {
			closeAll(files)
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

func readersOf(files []*os.File) []io.ReaderAt {
	rs := make([]io.ReaderAt, len(files))
	for i, f := range files {
		rs[i] = f
	}
	return rs
}

// writeImageDir creates the image directory out, a new image whose state file
// is a copy of src's and whose pages file and pages metadata file write
// writes. Every file and out itself are synced before writeImageDir returns;
// if it fails, it removes out.
func writeImageDir(out string, src *imageDir, write func(pages, meta io.Writer) (*checkpointimage.Image, error)) (img *checkpointimage.Image, retErr error) {
	if err := os.Mkdir(out, 0755); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			os.RemoveAll(out)
		}
	}()
	pages, err := os.OpenFile(filepath.Join(out, checkpointfiles.PagesFileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer pages.Close()
	meta, err := os.OpenFile(filepath.Join(out, checkpointfiles.PagesMetadataFileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer meta.Close()
	pagesBuf := bufio.NewWriterSize(pages, 1<<20)
	metaBuf := bufio.NewWriter(meta)
	img, err = write(pagesBuf, metaBuf)
	if err != nil {
		return nil, err
	}
	for _, b := range []*bufio.Writer{pagesBuf, metaBuf} {
		if err := b.Flush(); err != nil {
			return nil, err
		}
	}
	if err := copyFile(filepath.Join(out, checkpointfiles.StateFileName), filepath.Join(src.path, checkpointfiles.StateFileName)); err != nil {
		return nil, err
	}
	for _, f := range []*os.File{pages, meta} {
		if err := f.Sync(); err != nil {
			return nil, err
		}
	}
	return img, syncDir(out)
}

// copyFile copies the file src to the new file dst, and syncs it.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
