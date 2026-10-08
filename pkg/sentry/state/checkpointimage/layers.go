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

package checkpointimage

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
)

// LayersDir is the directory of an image in which it may keep the images it
// has as layers, each in a subdirectory named by its digest:
// <image>/layers/<digest>/. Remote images (in a checkpoint gofer's store) are
// laid out the same way, as objects named "layers/<digest>/...".
const LayersDir = "layers"

// LayerPath returns the path, relative to an image directory, of the file
// named name of the layer with digest d.
func LayerPath(d Digest, name string) string {
	return path.Join(LayersDir, d.String(), name)
}

// FileDigest returns the digest of the pages metadata file at path, the
// SHA-256 of its header, body and trailer, checking only its header.
func FileDigest(path string) (Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Digest{}, err
	}
	defer f.Close()
	h := sha256.New()
	var hdrBuf [HeaderSize]byte
	if _, err := io.ReadFull(f, hdrBuf[:]); err != nil {
		return Digest{}, fmt.Errorf("reading %s: %w", path, err)
	}
	hdr, err := ParseHeader(hdrBuf[:])
	if err != nil {
		return Digest{}, fmt.Errorf("%s: %w", path, err)
	}
	h.Write(hdrBuf[:])
	if n, err := io.CopyN(h, f, int64(hdr.Length+TrailerSize)); err != nil {
		if err == io.EOF {
			return Digest{}, fmt.Errorf("%s: %w: the file ends after %d bytes", path, ErrFormat, HeaderSize+n)
		}
		return Digest{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var d Digest
	copy(d[:], h.Sum(nil))
	return d, nil
}

// ErrLayerNotFound is wrapped by the errors of FindLayers that report a layer
// it found nowhere.
var ErrLayerNotFound = errors.New("layer not found")

// FindLayers returns the directories of the images that are layers 1 to n of
// img, whose own directory is imageDir. It looks for the layer with digest d
// in:
//
//   - imageDir/layers/d;
//   - for each directory p of searchPaths, in order: p itself, if it is an
//     image directory (holds a pages metadata file), and p/d.
//
// A candidate is a layer only if the digest of its pages metadata file is d;
// its pages file must then have the size that img records for it.
func FindLayers(img *Image, imageDir string, searchPaths []string) ([]string, error) {
	layers := img.Layers()
	dirs := make([]string, len(layers)-1)
	// Image directories in searchPaths, by digest; computed once, since a
	// directory may hold any of the layers.
	searchDigests := make(map[string]Digest)
	for i, l := range layers[1:] {
		candidates := []string{filepath.Join(imageDir, LayersDir, l.Digest.String())}
		for _, p := range searchPaths {
			candidates = append(candidates, p, filepath.Join(p, l.Digest.String()))
		}
		for _, dir := range candidates {
			metaPath := filepath.Join(dir, checkpointfiles.PagesMetadataFileName)
			d, ok := searchDigests[dir]
			if !ok {
				var err error
				d, err = FileDigest(metaPath)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("layer %d (%v): %w", i+1, l.Digest, err)
				}
				searchDigests[dir] = d
			}
			if d != l.Digest {
				continue
			}
			pagesPath := filepath.Join(dir, checkpointfiles.PagesFileName)
			st, err := os.Stat(pagesPath)
			if err != nil {
				return nil, fmt.Errorf("layer %d (%v): %w", i+1, l.Digest, err)
			}
			if uint64(st.Size()) != l.PagesSize {
				return nil, fmt.Errorf("layer %d (%v): %w: %s is %d bytes, the image expects %d", i+1, l.Digest, ErrFormat, pagesPath, st.Size(), l.PagesSize)
			}
			dirs[i] = dir
			break
		}
		if dirs[i] == "" {
			return nil, fmt.Errorf("layer %d (%v): %w in %s or the layer paths %q", i+1, l.Digest, ErrLayerNotFound, filepath.Join(imageDir, LayersDir), searchPaths)
		}
	}
	return dirs, nil
}
