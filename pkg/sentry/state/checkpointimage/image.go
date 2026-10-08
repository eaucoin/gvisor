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
	"crypto/sha256"
	"encoding/binary"
	"sort"

	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
)

// WorkingSetVersion is the WorkingSetProto.version of this format.
const WorkingSetVersion = 1

// Image is a parsed and validated pages metadata file.
type Image struct {
	// Header is the file's header.
	Header Header

	// Digest is the image's identity, the SHA-256 of the file's header, body
	// and trailer.
	Digest Digest

	// Proto is the image-level metadata.
	Proto *pb.ImageProto

	// MemoryFiles holds the metadata of each MemoryFile: the application
	// MemoryFile's, then the private MemoryFiles' in
	// Proto.PrivateMemoryFiles order.
	MemoryFiles []*pb.MemoryFileMetadataProto

	// records is the part of the body that follows the ImageProto.
	records []byte

	// If hashesRead is not nil, ReadHashesAsync is reading the page hashes
	// of MemoryFiles, and closes it when done.
	hashesRead chan struct{}
}

// MemoryFileRecords returns a reader of the MemoryFile metadata records that
// follow the ImageProto in the body, in the form that pgalloc.MemoryFile.LoadFrom
// reads.
func (img *Image) MemoryFileRecords() *bytes.Reader {
	return bytes.NewReader(img.records)
}

// Layer is the identity of one layer of an image.
type Layer struct {
	// Digest is the layer's digest; it is zero for layer 0, the image itself.
	Digest Digest

	// PagesSize is the size of the layer's pages file.
	PagesSize uint64
}

// Layers returns img's layers.
func (img *Image) Layers() []Layer {
	return layersOf(img.Proto)
}

func layersOf(ip *pb.ImageProto) []Layer {
	ls := make([]Layer, len(ip.GetLayers()))
	for i, l := range ip.GetLayers() {
		ls[i].Digest, _ = digestFromBytes(l.GetDigest())
		ls[i].PagesSize = l.GetPagesSize()
	}
	return ls
}

// CommittedBytes returns the number of known-committed bytes of the i-th
// MemoryFile.
func (img *Image) CommittedBytes(i int) uint64 {
	var n uint64
	for _, ma := range img.MemoryFiles[i].GetMemAcct() {
		if ma.GetKnownCommitted() {
			n += ma.GetEnd() - ma.GetStart()
		}
	}
	return n
}

// MemoryFileImage returns the in-memory form of the i-th MemoryFile's
// extents and page hashes. If ReadHashesAsync is reading the page hashes, it
// waits for it; the MemoryFileImage has no page hashes if they were not read.
func (img *Image) MemoryFileImage(i int) *MemoryFileImage {
	if img.hashesRead != nil {
		<-img.hashesRead
	}
	return NewMemoryFileImage(img.MemoryFiles[i])
}

// Extent maps the MemoryFile offsets [Start, End) to the pages file of a
// layer, starting at Offset.
type Extent struct {
	Start  uint64
	End    uint64
	Layer  uint32
	Offset uint64
}

// Length returns the length of e in bytes.
func (e Extent) Length() uint64 {
	return e.End - e.Start
}

// OffsetOf returns the pages file offset of the MemoryFile offset off.
//
// Preconditions: e.Start <= off < e.End.
func (e Extent) OffsetOf(off uint64) uint64 {
	return e.Offset + (off - e.Start)
}

// MemoryFileImage is one MemoryFile's part of an image: where the data of
// each of its known-committed pages is, and the hash of each.
type MemoryFileImage struct {
	// Extents are sorted by Start and do not overlap. Known-committed pages
	// without an extent are zero.
	Extents []Extent

	// committed are the known-committed ranges, merged, sorted and
	// non-overlapping.
	committed []committedRange

	// hashes holds the page hashes of the known-committed pages, in
	// MemoryFile offset order; it is nil if the image has no page hashes.
	hashes []byte
}

type committedRange struct {
	start uint64
	end   uint64
	// page is the index of the range's first page among known-committed
	// pages.
	page uint64
}

// NewMemoryFileImage returns the in-memory form of mf.
//
// Preconditions: mf has passed ValidateMemoryFile.
func NewMemoryFileImage(mf *pb.MemoryFileMetadataProto) *MemoryFileImage {
	m := &MemoryFileImage{
		Extents:   make([]Extent, len(mf.GetExtents())),
		committed: committedRanges(mf),
		hashes:    mf.GetPageHashes(),
	}
	for i, e := range mf.GetExtents() {
		m.Extents[i] = Extent{
			Start:  e.GetStart(),
			End:    e.GetEnd(),
			Layer:  e.GetLayer(),
			Offset: e.GetOffset(),
		}
	}
	return m
}

// committedRanges returns the known-committed ranges of mf, merged.
//
// Preconditions: mf.MemAcct is sorted and non-overlapping.
func committedRanges(mf *pb.MemoryFileMetadataProto) []committedRange {
	var crs []committedRange
	var pages uint64
	for _, ma := range mf.GetMemAcct() {
		if !ma.GetKnownCommitted() {
			continue
		}
		if n := len(crs); n != 0 && crs[n-1].end == ma.GetStart() {
			crs[n-1].end = ma.GetEnd()
		} else {
			crs = append(crs, committedRange{start: ma.GetStart(), end: ma.GetEnd(), page: pages})
		}
		pages += (ma.GetEnd() - ma.GetStart()) / PageSize
	}
	return crs
}

// HasHashes returns true if m has the page hash of every known-committed page:
// if it has page hashes, or no known-committed page. A MemoryFile without
// known-committed pages has no page hash to record, even in an image with page
// hashes, and needs none to be the base of a delta.
func (m *MemoryFileImage) HasHashes() bool {
	return m.hashes != nil || len(m.committed) == 0
}

// HashAt returns the hash of the page at MemoryFile offset off, and true, if
// that page is known-committed in m and m has page hashes.
func (m *MemoryFileImage) HashAt(off uint64) (uint64, bool) {
	if m.hashes == nil {
		return 0, false
	}
	i := sort.Search(len(m.committed), func(i int) bool { return m.committed[i].end > off })
	if i == len(m.committed) || m.committed[i].start > off {
		return 0, false
	}
	return m.hashAt(i, off), true
}

// Preconditions: m.committed[i] contains off; m.hashes != nil.
func (m *MemoryFileImage) hashAt(i int, off uint64) uint64 {
	cr := &m.committed[i]
	page := cr.page + (off-cr.start)/PageSize
	return binary.LittleEndian.Uint64(m.hashes[page*8:])
}

// ExtentAt returns the extent that contains MemoryFile offset off, and true,
// if there is one.
func (m *MemoryFileImage) ExtentAt(off uint64) (Extent, bool) {
	i := sort.Search(len(m.Extents), func(i int) bool { return m.Extents[i].End > off })
	if i == len(m.Extents) || m.Extents[i].Start > off {
		return Extent{}, false
	}
	return m.Extents[i], true
}

// Cursor looks up pages of a MemoryFileImage in increasing offset order, in
// amortized constant time per page.
type Cursor struct {
	m  *MemoryFileImage
	ei int
	ci int
}

// Cursor returns a Cursor positioned before m's first page.
func (m *MemoryFileImage) Cursor() Cursor {
	return Cursor{m: m}
}

// Lookup returns what m records for the page at MemoryFile offset off: the
// extent that holds its data, if any, and its hash, if it is known-committed
// in m and m has page hashes.
//
// Preconditions: off is not less than the offset of any previous call to
// Lookup on c.
func (c *Cursor) Lookup(off uint64) (e Extent, hasExtent bool, hash uint64, hasHash bool) {
	m := c.m
	for c.ei < len(m.Extents) && m.Extents[c.ei].End <= off {
		c.ei++
	}
	if c.ei < len(m.Extents) && m.Extents[c.ei].Start <= off {
		e, hasExtent = m.Extents[c.ei], true
	}
	for c.ci < len(m.committed) && m.committed[c.ci].end <= off {
		c.ci++
	}
	if m.hashes != nil && c.ci < len(m.committed) && m.committed[c.ci].start <= off {
		hash, hasHash = m.hashAt(c.ci, off), true
	}
	return
}

// ValidateImage checks the image-level metadata ip.
func ValidateImage(ip *pb.ImageProto) error {
	layers := ip.GetLayers()
	if len(layers) == 0 {
		return formatErrorf("no layers")
	}
	if len(layers[0].GetDigest()) != 0 {
		return formatErrorf("layer 0 has a digest")
	}
	seen := make(map[Digest]struct{}, len(layers)-1)
	for i, l := range layers[1:] {
		d, ok := digestFromBytes(l.GetDigest())
		if !ok {
			return formatErrorf("layer %d: digest of %d bytes", i+1, len(l.GetDigest()))
		}
		if _, dup := seen[d]; dup {
			return formatErrorf("layer %d: duplicate digest %v", i+1, d)
		}
		seen[d] = struct{}{}
		if l.GetPagesSize()%PageSize != 0 {
			return formatErrorf("layer %d: pages size %d is not page-aligned", i+1, l.GetPagesSize())
		}
	}
	if layers[0].GetPagesSize()%PageSize != 0 {
		return formatErrorf("layer 0: pages size %d is not page-aligned", layers[0].GetPagesSize())
	}
	switch ip.GetPageHash() {
	case 0:
		if n := len(ip.GetPageHashesDigest()); n != 0 {
			return formatErrorf("%d bytes of page hashes digest in an image without page hashes", n)
		}
	case PageHashXXH64:
		if n := len(ip.GetPageHashesDigest()); n != sha256.Size {
			return formatErrorf("page hashes digest is %d bytes, want %d", n, sha256.Size)
		}
	default:
		return formatErrorf("unknown page hash %d", ip.GetPageHash())
	}
	if ws := ip.GetWorkingSet(); ws != nil {
		if ws.GetVersion() != WorkingSetVersion {
			return formatErrorf("working set version %d is not supported by this build (%d)", ws.GetVersion(), WorkingSetVersion)
		}
		if u := ws.GetUnit(); u < PageSize || u&(u-1) != 0 {
			return formatErrorf("working set unit %d is not a power of two of at least %d", u, PageSize)
		}
		for _, fr := range ws.GetExtents() {
			if !wellFormed(fr.GetStart(), fr.GetEnd()) {
				return formatErrorf("working set extent [%#x, %#x) is not a non-empty page-aligned range", fr.GetStart(), fr.GetEnd())
			}
		}
	}
	return nil
}

// ValidateMemoryFile checks the metadata mf of a MemoryFile of the image
// whose image-level metadata is ip. If inline is true, the MemoryFile's pages
// follow its metadata in the same stream instead of being in pages files, so
// mf must have no extents.
//
// Preconditions: ip has passed ValidateImage.
func ValidateMemoryFile(mf *pb.MemoryFileMetadataProto, ip *pb.ImageProto, inline bool) error {
	if v := mf.GetVersion(); v != MemoryFileMetadataVersion {
		return formatErrorf("MemoryFile metadata version %d is not supported by this build (%d)", v, MemoryFileMetadataVersion)
	}
	var end uint64
	for _, ma := range mf.GetMemAcct() {
		if !wellFormed(ma.GetStart(), ma.GetEnd()) || ma.GetStart() < end {
			return formatErrorf("memory accounting range [%#x, %#x) is empty, unaligned, unsorted or overlapping", ma.GetStart(), ma.GetEnd())
		}
		end = ma.GetEnd()
	}

	extents := mf.GetExtents()
	if inline && len(extents) != 0 {
		return formatErrorf("%d extents in a MemoryFile saved inline", len(extents))
	}
	layers := ip.GetLayers()
	committed := committedRanges(mf)
	ci := 0
	end = 0
	for _, e := range extents {
		start, eend, off := e.GetStart(), e.GetEnd(), e.GetOffset()
		if !wellFormed(start, eend) || start < end {
			return formatErrorf("extent [%#x, %#x) is empty, unaligned, unsorted or overlapping", start, eend)
		}
		end = eend
		if off%PageSize != 0 {
			return formatErrorf("extent [%#x, %#x): offset %#x is not page-aligned", start, eend, off)
		}
		if e.GetLayer() >= uint32(len(layers)) {
			return formatErrorf("extent [%#x, %#x): layer %d of %d", start, eend, e.GetLayer(), len(layers))
		}
		if size := layers[e.GetLayer()].GetPagesSize(); off > size || eend-start > size-off {
			return formatErrorf("extent [%#x, %#x): offset %#x is beyond the %d bytes of layer %d", start, eend, off, size, e.GetLayer())
		}
		for ci < len(committed) && committed[ci].end <= start {
			ci++
		}
		if ci == len(committed) || committed[ci].start > start || committed[ci].end < eend {
			return formatErrorf("extent [%#x, %#x) is not within known-committed memory", start, eend)
		}
	}

	if n := len(mf.GetPageHashes()); n != 0 {
		return formatErrorf("%d bytes of page hashes in the body", n)
	}
	return nil
}

func wellFormed(start, end uint64) bool {
	return start < end && start%PageSize == 0 && end%PageSize == 0
}
