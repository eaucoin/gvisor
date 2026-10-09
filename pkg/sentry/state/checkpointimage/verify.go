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
	"errors"
	"fmt"
	"io"

	"github.com/cespare/xxhash/v2"
)

// ErrPageMismatch is wrapped by the errors of VerifyPages that report a page
// whose contents do not match its hash.
var ErrPageMismatch = errors.New("page contents do not match the image")

// zeroPageHash is the XXH64 of a zero-filled page.
var zeroPageHash = xxhash.Sum64(make([]byte, PageSize))

// VerifyPages checks every known-committed page of img against its page
// hash: pages with an extent are read from the pages file of their layer,
// layers[i] being layer i's; pages without one must have the hash of a zero
// page. It returns an error wrapping ErrPageMismatch for the first page that
// does not match, and one wrapping ErrFormat if a pages file is too short.
func VerifyPages(img *Image, layers []io.ReaderAt) error {
	if len(layers) != len(img.Proto.GetLayers()) {
		return fmt.Errorf("%d pages files for an image of %d layers", len(layers), len(img.Proto.GetLayers()))
	}
	if img.Proto.GetPageHash() != PageHashXXH64 {
		return fmt.Errorf("the image has no page hashes")
	}
	buf := make([]byte, copyBufferSize)
	for i := range img.MemoryFiles {
		m := img.MemoryFileImage(i)
		c := m.Cursor()
		for _, cr := range m.committed {
			for off := cr.start; off < cr.end; {
				e, hasExtent, h, _ := c.Lookup(off)
				if !hasExtent {
					if h != zeroPageHash {
						return fmt.Errorf("%w: MemoryFile %d, page %#x has no data but the hash of a non-zero page", ErrPageMismatch, i, off)
					}
					off += PageSize
					continue
				}
				// Read up to a buffer of the extent and check its pages.
				n := min(e.End-off, cr.end-off, uint64(len(buf)))
				if _, err := layers[e.Layer].ReadAt(buf[:n], int64(e.OffsetOf(off))); err != nil {
					return fmt.Errorf("MemoryFile %d, pages %#x-%#x in layer %d: %w", i, off, off+n, e.Layer, readError(err, n, e.OffsetOf(off)))
				}
				for p := uint64(0); p < n; p += PageSize {
					_, _, h, _ := c.Lookup(off + p)
					if got := xxhash.Sum64(buf[p : p+PageSize]); got != h {
						return fmt.Errorf("%w: MemoryFile %d, page %#x in layer %d at %#x has hash %#016x, the image records %#016x", ErrPageMismatch, i, off+p, e.Layer, e.OffsetOf(off+p), got, h)
					}
				}
				off += n
			}
		}
	}
	return nil
}
