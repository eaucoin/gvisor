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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/cespare/xxhash/v2"
	"google.golang.org/protobuf/proto"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
)

// dataImage is an image with its data: the pages file of each of its layers.
type dataImage struct {
	img    *Image
	layers [][]byte
}

func (di *dataImage) readers() []io.ReaderAt {
	rs := make([]io.ReaderAt, len(di.layers))
	for i, l := range di.layers {
		rs[i] = bytes.NewReader(l)
	}
	return rs
}

// memory returns the contents of the known-committed pages of di's
// application MemoryFile, read through its extents.
func (di *dataImage) memory(t *testing.T) [][]byte {
	t.Helper()
	m := di.img.MemoryFileImage(0)
	var mem [][]byte
	for _, cr := range m.committed {
		for off := cr.start; off < cr.end; off += PageSize {
			pg := make([]byte, PageSize)
			if e, ok := m.ExtentAt(off); ok {
				src := di.layers[e.Layer]
				copy(pg, src[e.OffsetOf(off):e.OffsetOf(off)+PageSize])
			}
			mem = append(mem, pg)
		}
	}
	return mem
}

// makeDataImage returns an image of a MemoryFile whose known-committed pages
// are [0, len(mem)) with contents mem. If parent is not nil, the image is a
// delta of it: pages with the same contents in parent refer to parent's data.
func makeDataImage(t *testing.T, mem [][]byte, parent *dataImage) *dataImage {
	t.Helper()
	var parentMem [][]byte
	if parent != nil {
		parentMem = parent.memory(t)
	}
	var pages []byte
	written := make(map[int]uint64)
	for i, pg := range mem {
		if isZero(pg) || (i < len(parentMem) && bytes.Equal(pg, parentMem[i])) {
			continue
		}
		written[i] = uint64(len(pages))
		pages = append(pages, pg...)
	}
	return finishDataImage(t, mem, pages, written, parent)
}

// finishDataImage returns an image of a MemoryFile whose known-committed pages
// are [0, len(mem)) with contents mem, a delta of parent if parent is not nil,
// whose pages file is pages. written maps the index of every page whose data
// is in pages to its offset there. Other non-zero pages refer to parent's
// data, which must be the same.
func finishDataImage(t *testing.T, mem [][]byte, pages []byte, written map[int]uint64, parent *dataImage) *dataImage {
	t.Helper()
	mf := &pb.MemoryFileMetadataProto{
		Version: MemoryFileMetadataVersion,
		Chunks:  []*pb.ChunkInfoProto{{}},
		MemAcct: []*pb.MemAcctRangeProto{{Start: 0, End: uint64(len(mem)) * PageSize, KnownCommitted: true}},
	}
	ip := &pb.ImageProto{Layers: []*pb.LayerProto{{}}, PageHash: PageHashXXH64}
	// byDigest holds the pages file of each of parent's layers.
	byDigest := make(map[Digest][]byte)
	var pm *MemoryFileImage
	if parent != nil {
		pm = parent.img.MemoryFileImage(0)
		for i, l := range parent.img.Layers() {
			if i == 0 {
				l.Digest = parent.img.Digest
			}
			ip.Layers = append(ip.Layers, &pb.LayerProto{Digest: bytes.Clone(l.Digest[:]), PagesSize: l.PagesSize})
			byDigest[l.Digest] = parent.layers[i]
		}
	}
	for i, pg := range mem {
		off := uint64(i) * PageSize
		mf.PageHashes = binary.LittleEndian.AppendUint64(mf.PageHashes, xxhash.Sum64(pg))
		if o, ok := written[i]; ok {
			mf.Extents = appendExtent(mf.Extents, off, off+PageSize, 0, o)
			continue
		}
		if isZero(pg) {
			continue
		}
		e, ok := pm.ExtentAt(off)
		if !ok {
			t.Fatalf("page %d is neither written nor in the parent", i)
		}
		mf.Extents = appendExtent(mf.Extents, off, off+PageSize, e.Layer+1, e.OffsetOf(off))
	}
	var meta bytes.Buffer
	w := NewWriter(&meta, ip)
	if err := WriteRecord(w, mf); err != nil {
		t.Fatal(err)
	}
	img, err := w.Finish(uint64(len(pages)))
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// Keep the pages files of the layers that Finish kept.
	di := &dataImage{img: img, layers: [][]byte{pages}}
	for _, l := range img.Layers()[1:] {
		di.layers = append(di.layers, byDigest[l.Digest])
	}
	return di
}

func isZero(pg []byte) bool {
	return bytes.Equal(pg, make([]byte, PageSize))
}

func cloneMemory(mem [][]byte) [][]byte {
	out := make([][]byte, len(mem))
	for i := range mem {
		out[i] = bytes.Clone(mem[i])
	}
	return out
}

// randomMemory returns n pages, every fourth of which is zero.
func randomMemory(rng *rand.Rand, n int) [][]byte {
	mem := make([][]byte, n)
	for i := range mem {
		mem[i] = make([]byte, PageSize)
		if i%4 != 0 {
			rng.Read(mem[i])
		}
	}
	return mem
}

// mutate returns a copy of mem with k random pages rewritten.
func mutate(rng *rand.Rand, mem [][]byte, k int) [][]byte {
	out := cloneMemory(mem)
	for ; k > 0; k-- {
		rng.Read(out[rng.Intn(len(out))])
	}
	return out
}

// chainDepths are the lengths of the chains that rewrites are tested on: a
// short one, and the longest that experiments restored.
var chainDepths = []int{3, 15}

// chain returns a chain of depth images of a MemoryFile of 64 pages, and the
// memory of the last: a full image, then deltas, the g-th of which rewrites
// the pages whose index is g modulo depth, so that every image of the chain
// holds some of the last one's pages.
func chain(t *testing.T, depth int) ([]*dataImage, [][]byte) {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(depth)))
	mem := randomMemory(rng, 64)
	images := []*dataImage{makeDataImage(t, mem, nil)}
	for g := 1; g < depth; g++ {
		mem = cloneMemory(mem)
		for i := g; i < len(mem); i += depth {
			rng.Read(mem[i])
		}
		images = append(images, makeDataImage(t, mem, images[g-1]))
	}
	if n := len(images[depth-1].img.Layers()); n != depth {
		t.Fatalf("the last image of a chain of %d has %d layers", depth, n)
	}
	return images, mem
}

// precopyImage returns an image saved as pre-copy saves one, a delta of
// parent, and its memory. The first round writes the pages that differ from
// parent's; each later round rewrites some pages and writes them again, at the
// end of the pages file, so that the pages file holds earlier copies of pages
// that no extent refers to.
func precopyImage(t *testing.T, rng *rand.Rand, parent *dataImage, rounds int) (*dataImage, [][]byte) {
	t.Helper()
	parentMem := parent.memory(t)
	mem := mutate(rng, parentMem, len(parentMem)/4)
	var pages []byte
	written := make(map[int]uint64)
	write := func(i int) {
		written[i] = uint64(len(pages))
		pages = append(pages, mem[i]...)
	}
	for i := range mem {
		if !bytes.Equal(mem[i], parentMem[i]) {
			write(i)
		}
	}
	for r := 1; r < rounds; r++ {
		for k := 0; k < len(mem)/8; k++ {
			i := rng.Intn(len(mem))
			rng.Read(mem[i])
			write(i)
		}
	}
	return finishDataImage(t, mem, pages, written, parent), mem
}

// referencedData returns the data that di's extents refer to in its own pages
// file, in MemoryFile order.
func referencedData(di *dataImage) []byte {
	var b []byte
	for _, e := range di.img.MemoryFileImage(0).Extents {
		if e.Layer == 0 {
			b = append(b, di.layers[0][e.Offset:e.Offset+e.Length()]...)
		}
	}
	return b
}

func rewrite(t *testing.T, di *dataImage, opts RewriteOpts) *dataImage {
	t.Helper()
	var pages, meta bytes.Buffer
	img, err := Rewrite(di.img, di.readers(), opts, &pages, &meta)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if read, err := ReadMetadata(bytes.NewReader(meta.Bytes())); err != nil || read.Digest != img.Digest {
		t.Fatalf("ReadMetadata of the rewritten image: %v, %v", read, err)
	}
	return &dataImage{img: img, layers: [][]byte{pages.Bytes()}}
}

func checkMemory(t *testing.T, di *dataImage, want [][]byte) {
	t.Helper()
	got := di.memory(t)
	if len(got) != len(want) {
		t.Fatalf("%d pages, want %d", len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("page %d differs", i)
		}
	}
	if err := VerifyPages(di.img, di.readers()); err != nil {
		t.Errorf("VerifyPages: %v", err)
	}
}

func TestFlatten(t *testing.T) {
	for _, depth := range chainDepths {
		t.Run(fmt.Sprintf("depth %d", depth), func(t *testing.T) {
			images, mem := chain(t, depth)
			last := images[depth-1]
			checkMemory(t, last, mem)
			flat := rewrite(t, last, RewriteOpts{})
			if n := len(flat.img.Layers()); n != 1 {
				t.Errorf("flattened image has %d layers", n)
			}
			checkMemory(t, flat, mem)
			// The pages file holds the non-zero pages, in MemoryFile order.
			if want := flatPages(mem); !bytes.Equal(flat.layers[0], want) {
				t.Errorf("flattened pages file of %d bytes is not the non-zero pages in order (%d bytes)", len(flat.layers[0]), len(want))
			}
		})
	}
}

func TestCompactKeepLayer(t *testing.T) {
	for _, depth := range chainDepths {
		t.Run(fmt.Sprintf("depth %d", depth), func(t *testing.T) {
			images, mem := chain(t, depth)
			template, last := images[0], images[depth-1]
			compacted := rewrite(t, last, RewriteOpts{Keep: map[Digest]struct{}{template.img.Digest: {}}})
			layers := compacted.img.Layers()
			if len(layers) != 2 || layers[1].Digest != template.img.Digest {
				t.Fatalf("layers = %+v, want [self, %v]", layers, template.img.Digest)
			}
			compacted.layers = append(compacted.layers, template.layers[0])
			checkMemory(t, compacted, mem)
			// The pages that the template holds stay there, and the others,
			// and only they, are copied, in MemoryFile order.
			var inTemplate uint64
			var copied []byte
			for _, e := range last.img.MemoryFileImage(0).Extents {
				if last.img.Layers()[e.Layer].Digest == template.img.Digest {
					inTemplate += e.Length()
					continue
				}
				copied = append(copied, last.layers[e.Layer][e.Offset:e.Offset+e.Length()]...)
			}
			if inTemplate == 0 || !bytes.Equal(compacted.layers[0], copied) {
				t.Errorf("%d bytes kept in the template and %d copied, want %d copied", inTemplate, len(compacted.layers[0]), len(copied))
			}
		})
	}
}

// TestCompactPrecopy compacts and flattens an image saved by pre-copy rounds,
// whose pages file holds copies of pages that it does not refer to: the
// rewritten image's pages file holds exactly the data the image refers to, and
// the rewritten image has the same memory.
func TestCompactPrecopy(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	template := makeDataImage(t, randomMemory(rng, 64), nil)
	pre, mem := precopyImage(t, rng, template, 4)
	checkMemory(t, pre, mem)
	referenced := referencedData(pre)
	if len(referenced) >= len(pre.layers[0]) {
		t.Fatalf("the pre-copy image refers to %d bytes of its pages file of %d bytes", len(referenced), len(pre.layers[0]))
	}

	compacted := rewrite(t, pre, RewriteOpts{Keep: map[Digest]struct{}{template.img.Digest: {}}})
	if layers := compacted.img.Layers(); len(layers) != 2 || layers[1].Digest != template.img.Digest {
		t.Fatalf("layers = %+v, want [self, %v]", layers, template.img.Digest)
	}
	compacted.layers = append(compacted.layers, template.layers[0])
	checkMemory(t, compacted, mem)
	if !bytes.Equal(compacted.layers[0], referenced) {
		t.Errorf("compacted pages file of %d bytes is not the %d bytes that the image refers to, in order", len(compacted.layers[0]), len(referenced))
	}

	flat := rewrite(t, pre, RewriteOpts{})
	checkMemory(t, flat, mem)
	if want := flatPages(mem); !bytes.Equal(flat.layers[0], want) {
		t.Errorf("flattened pages file of %d bytes is not the non-zero pages in order (%d bytes)", len(flat.layers[0]), len(want))
	}
}

func flatPages(mem [][]byte) []byte {
	var b []byte
	for _, pg := range mem {
		if !isZero(pg) {
			b = append(b, pg...)
		}
	}
	return b
}

func TestWorkingSetFirst(t *testing.T) {
	images, mem := chain(t, 3)
	c := *images[2]
	img := *c.img
	img.Proto = proto.Clone(img.Proto).(*pb.ImageProto)
	page := func(start, end uint64) *pb.FileRangeProto {
		return &pb.FileRangeProto{Start: start * PageSize, End: end * PageSize}
	}
	// Pages 41 to 43, partly twice, then 5, 0 and 100 (not in the
	// MemoryFile). Zero pages have no data to place.
	img.Proto.WorkingSet = &pb.WorkingSetProto{
		Version: WorkingSetVersion,
		Unit:    PageSize,
		Extents: []*pb.FileRangeProto{page(41, 43), page(42, 44), page(5, 6), page(0, 1), page(100, 101)},
	}
	c.img = &img
	compacted := rewrite(t, &c, RewriteOpts{WorkingSetFirst: true})
	checkMemory(t, compacted, mem)
	if !proto.Equal(compacted.img.Proto.GetWorkingSet(), img.Proto.WorkingSet) {
		t.Errorf("working set = %v, want %v", compacted.img.Proto.GetWorkingSet(), img.Proto.WorkingSet)
	}
	// The pages file holds the working set's non-zero pages in its order,
	// then the other non-zero pages in MemoryFile order.
	zero := make([]byte, PageSize)
	var want []byte
	placed := make(map[int]bool)
	for _, i := range []int{41, 42, 42, 43, 5, 0, 100} {
		if i < len(mem) && !placed[i] && !bytes.Equal(mem[i], zero) {
			placed[i] = true
			want = append(want, mem[i]...)
		}
	}
	if len(placed) < 4 {
		t.Fatalf("only %d pages of the working set have data", len(placed))
	}
	for i, pg := range mem {
		if !placed[i] && !bytes.Equal(pg, zero) {
			want = append(want, pg...)
		}
	}
	if !bytes.Equal(compacted.layers[0], want) {
		t.Errorf("pages file of %d bytes is not the working set's pages, then the others in order (%d bytes)", len(compacted.layers[0]), len(want))
	}
}

// TestRebase rebases a full image onto an image of the memory it started
// from.
func TestRebase(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	templateMem := randomMemory(rng, 64)
	template := makeDataImage(t, templateMem, nil)
	mem := mutate(rng, templateMem, 8)
	full := makeDataImage(t, mem, nil)

	rebased := rewrite(t, full, RewriteOpts{Onto: template.img, OntoLayers: template.readers()})
	layers := rebased.img.Layers()
	if len(layers) != 2 || layers[1].Digest != template.img.Digest {
		t.Fatalf("layers = %+v, want [self, %v]", layers, template.img.Digest)
	}
	rebased.layers = append(rebased.layers, template.layers[0])
	checkMemory(t, rebased, mem)
	if got, max := len(rebased.layers[0]), 8*PageSize; got > max {
		t.Errorf("rebased image copied %d bytes, want at most %d", got, max)
	}

	t.Run("different MemoryFiles", func(t *testing.T) {
		other := &Image{Digest: template.img.Digest, Proto: template.img.Proto, MemoryFiles: append(template.img.MemoryFiles, template.img.MemoryFiles[0])}
		var pages, meta bytes.Buffer
		if _, err := Rewrite(full.img, full.readers(), RewriteOpts{Onto: other, OntoLayers: template.readers()}, &pages, &meta); err == nil {
			t.Errorf("Rewrite onto an image with another number of MemoryFiles succeeded")
		}
	})
}

func TestVerifyPages(t *testing.T) {
	images, _ := chain(t, 3)
	c := images[2]
	if err := VerifyPages(c.img, c.readers()); err != nil {
		t.Fatalf("VerifyPages: %v", err)
	}
	// Flip a bit in a page of each layer that c refers to.
	for _, e := range c.img.MemoryFileImage(0).Extents {
		bad := &dataImage{img: c.img, layers: append([][]byte(nil), c.layers...)}
		bad.layers[e.Layer] = bytes.Clone(c.layers[e.Layer])
		bad.layers[e.Layer][e.Offset+100] ^= 1
		if err := VerifyPages(bad.img, bad.readers()); !errors.Is(err, ErrPageMismatch) {
			t.Errorf("VerifyPages with a flipped bit in layer %d at %#x = %v, want ErrPageMismatch", e.Layer, e.Offset+100, err)
		}
	}
	truncated := &dataImage{img: c.img, layers: append([][]byte(nil), c.layers...)}
	truncated.layers[1] = nil
	if err := VerifyPages(truncated.img, truncated.readers()); !errors.Is(err, ErrFormat) {
		t.Errorf("VerifyPages with an empty layer = %v, want ErrFormat", err)
	}
	// A pages file of the right size whose pages are not where the image's
	// extents say is not the layer's.
	rotated := &dataImage{img: c.img, layers: append([][]byte(nil), c.layers...)}
	rotated.layers[1] = append(bytes.Clone(c.layers[1][PageSize:]), c.layers[1][:PageSize]...)
	if err := VerifyPages(rotated.img, rotated.readers()); !errors.Is(err, ErrPageMismatch) {
		t.Errorf("VerifyPages with the pages of layer 1 rotated by a page = %v, want ErrPageMismatch", err)
	}
}
