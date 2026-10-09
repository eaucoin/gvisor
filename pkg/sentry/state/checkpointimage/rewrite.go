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
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"

	"google.golang.org/protobuf/proto"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
)

// RewriteOpts are options to Rewrite.
type RewriteOpts struct {
	// Keep holds the digests of layers of the image whose data stays where it
	// is: the rewritten image refers to them. The data in every other layer
	// is copied into the rewritten image's pages file.
	Keep map[Digest]struct{}

	// If Onto is not nil, pages that are the same, at the same offset of the
	// same MemoryFile, in Onto as in the image refer to Onto's data rather
	// than being copied: the rewritten image is rebased onto Onto, which
	// becomes one of its layers. OntoLayers are the pages files of Onto's
	// layers, OntoLayers[0] being Onto's own.
	Onto       *Image
	OntoLayers []io.ReaderAt

	// If WorkingSetFirst is true, the data that Rewrite copies of the pages
	// in the image's working set comes first in the new pages file, in the
	// working set's order, so that loading the pages file sequentially loads
	// the working set first. The rest follows in MemoryFile order.
	WorkingSetFirst bool
}

// Rewrite writes an image with the same memory as img, whose layers' pages
// files are layers (layers[0] is img's own), as a new image: its pages file
// to pages and its pages metadata file to meta. Data is copied from the
// layers in MemoryFile order (unless opts.WorkingSetFirst), so the new pages
// file holds only data that the new image refers to, laid out sequentially.
//
// Rewrite returns the new image. Its pages file and pages metadata file are
// complete when it returns, but not synced.
func Rewrite(img *Image, layers []io.ReaderAt, opts RewriteOpts, pages, meta io.Writer) (*Image, error) {
	srcLayers := img.Layers()
	if len(layers) != len(srcLayers) {
		return nil, fmt.Errorf("%d pages files for an image of %d layers", len(layers), len(srcLayers))
	}

	// The new image's layers: itself, then every layer it may refer to.
	// Finish drops those that it ends up not referring to.
	ip := proto.Clone(img.Proto).(*pb.ImageProto)
	ip.Layers = []*pb.LayerProto{{}}
	byDigest := make(map[Digest]uint32)
	addLayer := func(d Digest, pagesSize uint64) uint32 {
		if i, ok := byDigest[d]; ok {
			return i
		}
		i := uint32(len(ip.Layers))
		byDigest[d] = i
		ip.Layers = append(ip.Layers, &pb.LayerProto{Digest: bytes.Clone(d[:]), PagesSize: pagesSize})
		return i
	}
	// kept[i] is the new index of img's layer i if it is kept.
	kept := make(map[uint32]uint32)
	for i, l := range srcLayers[1:] {
		if _, ok := opts.Keep[l.Digest]; ok {
			kept[uint32(i+1)] = addLayer(l.Digest, l.PagesSize)
		}
	}
	if len(kept) != len(opts.Keep) {
		for d := range opts.Keep {
			if _, ok := byDigest[d]; !ok {
				return nil, fmt.Errorf("%v is not a layer of image %v", d, img.Digest)
			}
		}
	}
	// onto[i] is the new index of Onto's layer i.
	var onto []uint32
	if opts.Onto != nil {
		if err := checkRebase(img, opts.Onto, len(opts.OntoLayers)); err != nil {
			return nil, err
		}
		for i, l := range opts.Onto.Layers() {
			if i == 0 {
				l.Digest = opts.Onto.Digest
			}
			onto = append(onto, addLayer(l.Digest, l.PagesSize))
		}
	}

	w := NewWriter(meta, ip)
	c := copier{layers: layers, pages: pages}
	page := make([]byte, PageSize)
	ontoPage := make([]byte, PageSize)
	for i, mf := range img.MemoryFiles {
		m := img.MemoryFileImage(i)
		var ontoCursor Cursor
		if opts.Onto != nil {
			ontoCursor = opts.Onto.MemoryFileImage(i).Cursor()
		}
		// refs are the extents of the new image that refer to other layers;
		// copies are the ranges whose data is copied into its pages file.
		// Both are in MemoryFile order.
		var refs []*pb.ExtentProto
		var copies []Extent
		for _, e := range m.Extents {
			if l, ok := kept[e.Layer]; ok {
				refs = append(refs, &pb.ExtentProto{Start: e.Start, End: e.End, Layer: l, Offset: e.Offset})
				continue
			}
			if opts.Onto == nil {
				copies = append(copies, e)
				continue
			}
			// Rebasing: page by page, refer to Onto's data where it is the
			// same, and copy runs of other pages.
			runStart := e.Start
			for off := e.Start; off < e.End; off += PageSize {
				oe, ok, _, _ := ontoCursor.Lookup(off)
				if !ok {
					continue
				}
				if err := readPage(layers[e.Layer], page, e.OffsetOf(off)); err != nil {
					return nil, fmt.Errorf("layer %d: %w", e.Layer, err)
				}
				if err := readPage(opts.OntoLayers[oe.Layer], ontoPage, oe.OffsetOf(off)); err != nil {
					return nil, fmt.Errorf("layer %d of the image to rebase onto: %w", oe.Layer, err)
				}
				if !bytes.Equal(page, ontoPage) {
					continue
				}
				if runStart < off {
					copies = append(copies, e.sub(runStart, off))
				}
				refs = append(refs, &pb.ExtentProto{Start: off, End: off + PageSize, Layer: onto[oe.Layer], Offset: oe.OffsetOf(off)})
				runStart = off + PageSize
			}
			if runStart < e.End {
				copies = append(copies, e.sub(runStart, e.End))
			}
		}
		if i == 0 && opts.WorkingSetFirst {
			copies = workingSetFirst(copies, img.Proto.GetWorkingSet())
		}
		for _, e := range copies {
			off, err := c.copy(e.Layer, e.Offset, e.Length())
			if err != nil {
				return nil, err
			}
			refs = append(refs, &pb.ExtentProto{Start: e.Start, End: e.End, Layer: 0, Offset: off})
		}
		slices.SortFunc(refs, func(a, b *pb.ExtentProto) int {
			return cmp.Compare(a.Start, b.Start)
		})
		out := proto.Clone(mf).(*pb.MemoryFileMetadataProto)
		out.Extents = nil
		for _, e := range refs {
			out.Extents = appendExtent(out.Extents, e.Start, e.End, e.Layer, e.Offset)
		}
		if err := WriteRecord(w, out); err != nil {
			return nil, err
		}
	}
	return w.Finish(c.size)
}

// workingSetFirst returns the ranges of copies, ranges of a MemoryFile in
// MemoryFile order, reordered so that those in the working set ws come first,
// in the working set's order, then the others in MemoryFile order.
func workingSetFirst(copies []Extent, ws *pb.WorkingSetProto) []Extent {
	// find returns the index of the range of copies that contains off, if any.
	find := func(off uint64) (int, bool) {
		i, _ := slices.BinarySearchFunc(copies, off, func(e Extent, off uint64) int {
			if e.End <= off {
				return -1
			}
			if e.Start > off {
				return 1
			}
			return 0
		})
		return i, i < len(copies) && copies[i].Start <= off
	}
	var ordered []Extent
	// add appends the page at off of copies[i] to ordered, merging it with
	// the last range if they are contiguous in both the MemoryFile and the
	// source pages file.
	add := func(i int, off uint64) {
		pg := copies[i].sub(off, off+PageSize)
		if n := len(ordered); n != 0 {
			last := &ordered[n-1]
			if last.End == pg.Start && last.Layer == pg.Layer && last.Offset+last.Length() == pg.Offset {
				last.End = pg.End
				return
			}
		}
		ordered = append(ordered, pg)
	}
	placed := make(map[uint64]struct{})
	for _, fr := range ws.GetExtents() {
		for off := fr.GetStart(); off < fr.GetEnd(); off += PageSize {
			i, ok := find(off)
			if !ok {
				// Not copied: zero, or in a layer that the new image refers to.
				continue
			}
			if _, ok := placed[off]; ok {
				continue
			}
			placed[off] = struct{}{}
			add(i, off)
		}
	}
	if len(placed) == 0 {
		return copies
	}
	for i, e := range copies {
		for off := e.Start; off < e.End; off += PageSize {
			if _, ok := placed[off]; !ok {
				add(i, off)
			}
		}
	}
	return ordered
}

// checkRebase checks that img can be rebased onto onto, whose layers have
// ontoLayers pages files.
func checkRebase(img, onto *Image, ontoLayers int) error {
	if n := len(onto.Proto.GetLayers()); ontoLayers != n {
		return fmt.Errorf("%d pages files for an image of %d layers to rebase onto", ontoLayers, n)
	}
	if len(onto.MemoryFiles) != len(img.MemoryFiles) {
		return fmt.Errorf("cannot rebase an image of %d MemoryFiles onto one of %d", len(img.MemoryFiles), len(onto.MemoryFiles))
	}
	for i, owner := range img.Proto.GetPrivateMemoryFiles() {
		if o := onto.Proto.GetPrivateMemoryFiles()[i]; !proto.Equal(owner, o) {
			return fmt.Errorf("cannot rebase private MemoryFile %d (%v) onto %v", i+1, owner, o)
		}
	}
	return nil
}

// readPage reads the page at offset off of a pages file into pg.
func readPage(r io.ReaderAt, pg []byte, off uint64) error {
	if _, err := r.ReadAt(pg, int64(off)); err != nil {
		return readError(err, PageSize, off)
	}
	return nil
}

// readError returns the error of a read of n bytes at offset off of a pages
// file: a pages file too short for its image is invalid.
func readError(err error, n, off uint64) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: the pages file ends before byte %#x", ErrFormat, off+n)
	}
	return fmt.Errorf("reading %d bytes at %#x: %w", n, off, err)
}

// sub returns the part of e that maps [start, end).
//
// Preconditions: e.Start <= start < end <= e.End.
func (e Extent) sub(start, end uint64) Extent {
	return Extent{Start: start, End: end, Layer: e.Layer, Offset: e.OffsetOf(start)}
}

// appendExtent appends the extent {start, end, layer, off} to extents,
// merging it with the last one if they are contiguous.
func appendExtent(extents []*pb.ExtentProto, start, end uint64, layer uint32, off uint64) []*pb.ExtentProto {
	if n := len(extents); n != 0 {
		last := extents[n-1]
		if last.End == start && last.Layer == layer && last.Offset+(last.End-last.Start) == off {
			last.End = end
			return extents
		}
	}
	return append(extents, &pb.ExtentProto{Start: start, End: end, Layer: layer, Offset: off})
}

// copyBufferSize is the size of the reads of a copier.
const copyBufferSize = 1 << 20

// copier copies data from pages files of layers to a pages file.
type copier struct {
	layers []io.ReaderAt
	pages  io.Writer
	// size is the number of bytes written to pages.
	size uint64
	buf  []byte
}

// copy copies length bytes at offset off of layer's pages file to the end of
// the pages file, and returns the offset at which it wrote them.
func (c *copier) copy(layer uint32, off, length uint64) (uint64, error) {
	if c.buf == nil {
		c.buf = make([]byte, copyBufferSize)
	}
	start := c.size
	for length != 0 {
		n := min(length, uint64(len(c.buf)))
		if _, err := c.layers[layer].ReadAt(c.buf[:n], int64(off)); err != nil {
			return 0, fmt.Errorf("layer %d: %w", layer, readError(err, n, off))
		}
		if _, err := c.pages.Write(c.buf[:n]); err != nil {
			return 0, err
		}
		c.size += n
		off += n
		length -= n
	}
	return start, nil
}
