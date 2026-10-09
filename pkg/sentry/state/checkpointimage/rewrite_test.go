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
	n := uint64(len(mem))
	mf := &pb.MemoryFileMetadataProto{
		Version: MemoryFileMetadataVersion,
		Chunks:  []*pb.ChunkInfoProto{{}},
		MemAcct: []*pb.MemAcctRangeProto{{Start: 0, End: n * PageSize, KnownCommitted: true}},
	}
	ip := &pb.ImageProto{Layers: []*pb.LayerProto{{}}, PageHash: PageHashXXH64}
	var layers [][]byte
	var pm *MemoryFileImage
	var parentMem [][]byte
	if parent != nil {
		pm = parent.img.MemoryFileImage(0)
		parentMem = parent.memory(t)
		for i, l := range parent.img.Layers() {
			if i == 0 {
				l.Digest = parent.img.Digest
			}
			ip.Layers = append(ip.Layers, &pb.LayerProto{Digest: bytes.Clone(l.Digest[:]), PagesSize: l.PagesSize})
			layers = append(layers, parent.layers[i])
		}
	}
	var pages []byte
	for i, pg := range mem {
		off := uint64(i) * PageSize
		mf.PageHashes = binary.LittleEndian.AppendUint64(mf.PageHashes, xxhash.Sum64(pg))
		if bytes.Equal(pg, make([]byte, PageSize)) {
			continue
		}
		if parent != nil && i < len(parentMem) && bytes.Equal(pg, parentMem[i]) {
			e, _ := pm.ExtentAt(off)
			mf.Extents = appendExtent(mf.Extents, off, off+PageSize, e.Layer+1, e.OffsetOf(off))
			continue
		}
		mf.Extents = appendExtent(mf.Extents, off, off+PageSize, 0, uint64(len(pages)))
		pages = append(pages, pg...)
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
	byDigest := make(map[Digest][]byte)
	if parent != nil {
		for i, l := range parent.img.Layers() {
			if i == 0 {
				l.Digest = parent.img.Digest
			}
			byDigest[l.Digest] = layers[i]
		}
	}
	di := &dataImage{img: img, layers: [][]byte{pages}}
	for _, l := range img.Layers()[1:] {
		di.layers = append(di.layers, byDigest[l.Digest])
	}
	return di
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
	out := make([][]byte, len(mem))
	for i := range mem {
		out[i] = bytes.Clone(mem[i])
	}
	for ; k > 0; k-- {
		rng.Read(out[rng.Intn(len(out))])
	}
	return out
}

// chain returns a chain of three images: a full image, a delta of it, and a
// delta of the delta, and the memory of the last.
func chain(t *testing.T) ([]*dataImage, [][]byte) {
	rng := rand.New(rand.NewSource(1))
	mem := randomMemory(rng, 64)
	a := makeDataImage(t, mem, nil)
	mem = mutate(rng, mem, 16)
	b := makeDataImage(t, mem, a)
	mem = mutate(rng, mem, 16)
	c := makeDataImage(t, mem, b)
	return []*dataImage{a, b, c}, mem
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
	images, mem := chain(t)
	c := images[2]
	if n := len(c.img.Layers()); n != 3 {
		t.Fatalf("the chain's last image has %d layers, want 3", n)
	}
	flat := rewrite(t, c, RewriteOpts{})
	if n := len(flat.img.Layers()); n != 1 {
		t.Errorf("flattened image has %d layers", n)
	}
	checkMemory(t, flat, mem)
	// The pages file holds the non-zero pages, in MemoryFile order.
	var want []byte
	for _, pg := range mem {
		if !bytes.Equal(pg, make([]byte, PageSize)) {
			want = append(want, pg...)
		}
	}
	if !bytes.Equal(flat.layers[0], want) {
		t.Errorf("flattened pages file of %d bytes is not the non-zero pages in order (%d bytes)", len(flat.layers[0]), len(want))
	}
}

func TestCompactKeepLayer(t *testing.T) {
	images, mem := chain(t)
	a, c := images[0], images[2]
	compacted := rewrite(t, c, RewriteOpts{Keep: map[Digest]struct{}{a.img.Digest: {}}})
	layers := compacted.img.Layers()
	if len(layers) != 2 || layers[1].Digest != a.img.Digest {
		t.Fatalf("layers = %+v, want [self, %v]", layers, a.img.Digest)
	}
	compacted.layers = append(compacted.layers, a.layers[0])
	checkMemory(t, compacted, mem)
	// What stays in a is not copied.
	var inA uint64
	for _, e := range compacted.img.MemoryFiles[0].Extents {
		if e.Layer == 1 {
			inA += e.End - e.Start
		}
	}
	if inA == 0 || uint64(len(compacted.layers[0]))+inA != uint64(len(flatPages(mem))) {
		t.Errorf("%d bytes kept in the template and %d copied, for %d bytes of data", inA, len(compacted.layers[0]), len(flatPages(mem)))
	}
}

func flatPages(mem [][]byte) []byte {
	var b []byte
	for _, pg := range mem {
		if !bytes.Equal(pg, make([]byte, PageSize)) {
			b = append(b, pg...)
		}
	}
	return b
}

func TestWorkingSetFirst(t *testing.T) {
	images, mem := chain(t)
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
	images, _ := chain(t)
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
	// The data of another image of the same size is not the image's.
	swapped := &dataImage{img: c.img, layers: [][]byte{c.layers[0], c.layers[2], c.layers[1]}}
	if len(c.layers[1]) == len(c.layers[2]) {
		if err := VerifyPages(swapped.img, swapped.readers()); !errors.Is(err, ErrPageMismatch) {
			t.Errorf("VerifyPages with swapped layers = %v, want ErrPageMismatch", err)
		}
	}
}
