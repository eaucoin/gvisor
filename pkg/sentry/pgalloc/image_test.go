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

//go:build !pagesize_64k

package pgalloc

import (
	"bytes"
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/state/pretty"
	"gvisor.dev/gvisor/pkg/state/wire"
	"gvisor.dev/gvisor/pkg/sync"
)

// fillPage writes to the page at off the pattern of generation gen+1
// (patternPage), so that generation 0 is not the zero page.
func fillPage(f *MemoryFile, off uint64, gen uint64) {
	patternPage(f.pageSlice(off), off, gen+1)
}

// zeroPageAt writes zeroes to the page at off.
func zeroPageAt(f *MemoryFile, off uint64) {
	clear(f.pageSlice(off))
}

// imageTestFile is a MemoryFile and what has been done to it since its last
// save, for a test to save it as a delta.
type imageTestFile struct {
	f        *MemoryFile
	fr       memmap.FileRange
	modified map[uint64]bool
}

// newImageTestFile returns a MemoryFile with an allocation of n pages, every
// third of which is zero and the others filled with generation 0.
func newImageTestFile(t *testing.T, n uint64) *imageTestFile {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr, err := f.Allocate(n*page, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	for off := fr.Start; off < fr.End; off += page {
		if (off-fr.Start)/page%3 != 0 {
			fillPage(f, off, 0)
		}
	}
	return &imageTestFile{f: f, fr: fr, modified: make(map[uint64]bool)}
}

// write writes generation gen to page i of the allocation.
func (tf *imageTestFile) write(i, gen uint64) {
	off := tf.fr.Start + i*page
	fillPage(tf.f, off, gen)
	tf.modified[off] = true
}

// zero writes zeroes to page i of the allocation.
func (tf *imageTestFile) zero(i uint64) {
	off := tf.fr.Start + i*page
	zeroPageAt(tf.f, off)
	tf.modified[off] = true
}

// rewrite writes the current contents of page i of the allocation back to it:
// it is written, but unchanged.
func (tf *imageTestFile) rewrite(i uint64) {
	off := tf.fr.Start + i*page
	pg := tf.f.pageSlice(off)
	copy(pg, bytes.Clone(pg))
	tf.modified[off] = true
}

func (tf *imageTestFile) clean(off uint64) bool {
	return !tf.modified[off]
}

// layerDigests returns the digests of ti's layers, ti's own included.
func (ti *testImage) layerDigests() []checkpointimage.Digest {
	var ds []checkpointimage.Digest
	for i, l := range ti.img.Layers() {
		if i == 0 {
			l.Digest = ti.img.Digest
		}
		ds = append(ds, l.Digest)
	}
	return ds
}

// mfImage returns the main MemoryFile's part of image.
func (ti *testImage) mfImage() *checkpointimage.MemoryFileImage {
	return ti.img.MemoryFileImage(0)
}

// saveTestImage saves f as an image. If parent is not nil, it saves a delta of
// parent whose clean pages are those for which clean returns true, parent's
// layers becoming the following layers of the image.
func saveTestImage(t *testing.T, f *MemoryFile, parent *testImage, clean func(uint64) bool) *testImage {
	t.Helper()
	return saveTestImageOpts(t, f, parent, SaveOpts{Dirty: dirtySetOf(f, clean), PageHashes: true})
}

// dirtySetOf returns the set of the pages of f for which clean returns false,
// or nil if clean is nil.
func dirtySetOf(f *MemoryFile, clean func(uint64) bool) *DirtySet {
	if clean == nil {
		return nil
	}
	size := uint64(len(f.chunksLoad())) * chunkSize
	s := &DirtySet{words: make([]uint64, size/hostarch.PageSize/64)}
	for off := uint64(0); off < size; off += hostarch.PageSize {
		if !clean(off) {
			s.add(memmap.FileRange{Start: off, End: off + hostarch.PageSize})
		}
	}
	return s
}

// saveTestImageOpts is saveTestImage with the given SaveOpts, whose Base,
// BaseLayers and PagesFile it sets.
func saveTestImageOpts(t *testing.T, f *MemoryFile, parent *testImage, opts SaveOpts) *testImage {
	t.Helper()
	return newTestPagesWriter(t).save(t, f, parent, opts)
}

// testPagesWriter is async page saving to a pages file in memory.
type testPagesWriter struct {
	apfs *AsyncPagesFileSave
	buf  bytes.Buffer
	done chan error
}

func newTestPagesWriter(t *testing.T) *testPagesWriter {
	t.Helper()
	pw := &testPagesWriter{done: make(chan error, 1)}
	apfs, err := StartAsyncPagesFileSave(stateio.NewIOWriter(&pw.buf, 64<<10, 16, 4), func(err error) { pw.done <- err })
	if err != nil {
		t.Fatalf("StartAsyncPagesFileSave: %v", err)
	}
	pw.apfs = apfs
	return pw
}

// save saves f as an image whose pages file pw writes, as saveTestImageOpts
// does, and completes pw.
func (pw *testPagesWriter) save(t *testing.T, f *MemoryFile, parent *testImage, opts SaveOpts) *testImage {
	t.Helper()
	ip := &pgallocpb.ImageProto{
		Layers:   []*pgallocpb.LayerProto{{}},
		PageHash: checkpointimage.PageHashXXH64,
	}
	if parent != nil {
		opts.Base = parent.mfImage()
		for i, d := range parent.layerDigests() {
			ip.Layers = append(ip.Layers, &pgallocpb.LayerProto{Digest: bytes.Clone(d[:]), PagesSize: parent.img.Layers()[i].PagesSize})
			opts.BaseLayers = append(opts.BaseLayers, uint32(1+i))
		}
	}

	opts.PagesFile = pw.apfs
	var metaBuf bytes.Buffer
	w := checkpointimage.NewWriter(&metaBuf, ip)
	saveErr := f.SaveTo(context.Background(), w, &opts)
	pw.apfs.MemoryFilesDone()
	if err := <-pw.done; err != nil {
		t.Fatalf("async page saving: %v", err)
	}
	if saveErr != nil {
		t.Fatalf("SaveTo: %v", saveErr)
	}
	img, err := w.Finish(pw.apfs.PagesFileOffset())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ti := &testImage{meta: metaBuf.Bytes(), pages: pw.buf.Bytes(), img: img}
	// Find the pages files of the layers that the image kept.
	ti.layers = [][]byte{ti.pages}
	if parent != nil {
		byDigest := make(map[checkpointimage.Digest][]byte)
		for i, d := range parent.layerDigests() {
			byDigest[d] = parent.layers[i]
		}
		for _, l := range img.Layers()[1:] {
			ti.layers = append(ti.layers, byDigest[l.Digest])
		}
	}
	// What is written is what ReadMetadata reads.
	read, err := checkpointimage.ReadMetadata(bytes.NewReader(ti.meta))
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if read.Digest != img.Digest {
		t.Fatalf("ReadMetadata digest %v, Finish digest %v", read.Digest, img.Digest)
	}
	return ti
}

// loadTestImage loads ti into a new MemoryFile, reading each layer's pages
// file from readers[i] if given, and returns it without waiting for loading
// to complete.
func loadTestImage(t *testing.T, ti *testImage, readers ...stateio.AsyncReader) *MemoryFile {
	t.Helper()
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	opts := LoadOpts{Image: ti.img.Proto}
	for i, layer := range ti.layers {
		var ar stateio.AsyncReader
		if i < len(readers) {
			ar = readers[i]
		} else {
			ar = stateio.NewIOReader(bytes.NewReader(layer), 64<<10, 16, 4)
		}
		done := make(chan struct{})
		apfl, err := StartAsyncPagesFileLoad(ar, func(err error) {
			if err != nil {
				t.Errorf("async page loading from layer %d: %v", i, err)
			}
			close(done)
		}, nil, AsyncPagesFileLoadOpts{})
		if err != nil {
			t.Fatalf("StartAsyncPagesFileLoad: %v", err)
		}
		// Loading stops before the test ends, so that a test may replace its
		// clock (testPagesFile.useClock).
		t.Cleanup(func() {
			apfl.MemoryFilesDone()
			select {
			case <-done:
			case <-time.After(testWaitTimeout):
				t.Errorf("timed out after %v waiting for async page loading from layer %d to stop", testWaitTimeout, i)
			}
		})
		opts.PagesFiles = append(opts.PagesFiles, apfl)
	}
	if err := f.LoadFrom(context.Background(), ti.img.MemoryFileRecords(), &opts); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return f
}

// checkSameContents checks that got has the same contents as want over the
// allocation fr.
func checkSameContents(t *testing.T, got, want *MemoryFile, fr memmap.FileRange) {
	t.Helper()
	if err := got.AwaitLoadAll(); err != nil {
		t.Fatalf("AwaitLoadAll: %v", err)
	}
	bad := 0
	for off := fr.Start; off < fr.End; off += page {
		if !bytes.Equal(got.pageSlice(off), want.pageSlice(off)) {
			if bad < 10 {
				t.Errorf("page %#x differs: got %x..., want %x...", off, got.pageSlice(off)[:16], want.pageSlice(off)[:16])
			}
			bad++
		}
	}
	if bad != 0 {
		t.Errorf("%d pages differ", bad)
	}
}

// pageLayers returns, for each page of fr, the layer that holds its data in
// m, or -1 if it has none.
func pageLayers(m *checkpointimage.MemoryFileImage, fr memmap.FileRange) map[uint64]int {
	layers := make(map[uint64]int)
	for off := fr.Start; off < fr.End; off += page {
		layers[off] = -1
		if e, ok := m.ExtentAt(off); ok {
			layers[off] = int(e.Layer)
		}
	}
	return layers
}

func TestImageFull(t *testing.T) {
	tf := newImageTestFile(t, 64)
	ti := saveTestImage(t, tf.f, nil, nil)

	mf := ti.img.MemoryFiles[0]
	if got := len(ti.img.Layers()); got != 1 {
		t.Errorf("image has %d layers, want 1", got)
	}
	// Every non-zero page is in the pages file, at the offset its extent
	// records, and has its hash.
	m := ti.mfImage()
	for off := tf.fr.Start; off < tf.fr.End; off += page {
		pg := tf.f.pageSlice(off)
		e, ok := m.ExtentAt(off)
		if bytes.Equal(pg, zeroPageBytes[:]) {
			if ok {
				t.Errorf("zero page %#x has an extent %+v", off, e)
			}
			continue
		}
		if !ok || e.Layer != 0 {
			t.Fatalf("page %#x: extent %+v, %t", off, e, ok)
		}
		if got := ti.pages[e.OffsetOf(off):][:page]; !bytes.Equal(got, pg) {
			t.Errorf("page %#x: pages file has %x..., want %x...", off, got[:16], pg[:16])
		}
		if h, ok := m.HashAt(off); !ok || h != xxhash.Sum64(pg) {
			t.Errorf("page %#x: hash %#x, %t; want %#x", off, h, ok, xxhash.Sum64(pg))
		}
	}
	if len(mf.Extents) == 0 || uint64(len(ti.pages)) != ti.img.Layers()[0].PagesSize {
		t.Errorf("%d extents, pages file of %d bytes, layer 0 of %d bytes", len(mf.Extents), len(ti.pages), ti.img.Layers()[0].PagesSize)
	}

	f2 := loadTestImage(t, ti)
	checkSameContents(t, f2, tf.f, tf.fr)
}

// TestImageDelta saves deltas of a MemoryFile and checks that each, restored
// with its layers, has the memory of the moment it was saved, as a full image
// of the same moment does (Firecracker's test_cmp_full_and_first_diff_mem),
// and that a delta refers to its parent for pages that did not change (CRIU's
// check_parent_pages).
func TestImageDelta(t *testing.T) {
	const pages = 64
	tf := newImageTestFile(t, pages)
	parent := saveTestImage(t, tf.f, nil, nil)

	tf.write(1, 1)  // changed
	tf.write(3, 1)  // was zero
	tf.zero(4)      // becomes zero
	tf.rewrite(5)   // written with the same contents
	tf.write(10, 1) // changed
	delta := saveTestImage(t, tf.f, parent, tf.clean)

	if layers := delta.img.Layers(); len(layers) != 2 || layers[1].Digest != parent.img.Digest {
		t.Fatalf("delta layers = %+v, want [self, %v]", layers, parent.img.Digest)
	}
	got := pageLayers(delta.mfImage(), tf.fr)
	for i := uint64(0); i < pages; i++ {
		off := tf.fr.Start + i*page
		want := 1 // in the parent
		switch {
		case i == 1 || i == 3 || i == 10:
			want = 0 // written by the delta
		case i == 4 || (i%3 == 0 && i != 3):
			want = -1 // zero
		}
		if got[off] != want {
			t.Errorf("page %d is in layer %d, want %d", i, got[off], want)
		}
	}
	if n := uint64(len(delta.pages)); n != 3*page {
		t.Errorf("delta wrote %d bytes, want %d", n, 3*page)
	}

	full := saveTestImage(t, tf.f, nil, nil)
	fromDelta := loadTestImage(t, delta)
	fromFull := loadTestImage(t, full)
	checkSameContents(t, fromDelta, tf.f, tf.fr)
	checkSameContents(t, fromFull, fromDelta, tf.fr)

	// The hashes recorded by the delta are those of the full image.
	dm, fm := delta.mfImage(), full.mfImage()
	for off := tf.fr.Start; off < tf.fr.End; off += page {
		dh, dok := dm.HashAt(off)
		fh, fok := fm.HashAt(off)
		if dok != fok || dh != fh {
			t.Errorf("page %#x: delta hash %#x, %t; full image hash %#x, %t", off, dh, dok, fh, fok)
		}
	}
}

// TestImageDeltaOfEmpty saves a delta of an image of a MemoryFile that had no
// committed page, and so no page hash, when the image was saved, as the
// private MemoryFile of an unused tmpfs mount has.
func TestImageDeltaOfEmpty(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	empty := saveTestImage(t, f, nil, nil)
	if !empty.mfImage().HasHashes() {
		t.Errorf("an image of an empty MemoryFile lacks page hashes")
	}
	fr, err := f.Allocate(4*page, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	for off := fr.Start; off < fr.End; off += page {
		fillPage(f, off, 1)
	}
	delta := saveTestImage(t, f, empty, func(uint64) bool { return false })
	if n := uint64(len(delta.pages)); n != 4*page {
		t.Errorf("delta wrote %d bytes, want %d", n, 4*page)
	}
	checkSameContents(t, loadTestImage(t, delta), f, fr)
}

// TestImageDeltaKeepsCleanPages checks that a delta save does not decommit a
// clean page that is not known to be committed, which it does not read: if
// dirty tracking missed a write to it, the write must survive in the
// MemoryFile.
func TestImageDeltaKeepsCleanPages(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr, err := f.Allocate(4*page, AllocOpts{Kind: usage.Anonymous, Mode: AllocateCallerIndirectCommit})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	parent := saveTestImage(t, f, nil, nil)
	// A write that dirty tracking missed.
	off := fr.Start + page
	fillPage(f, off, 1)
	saveTestImage(t, f, parent, func(uint64) bool { return true })
	want := make([]byte, page)
	patternPage(want, off, 2)
	if !bytes.Equal(f.pageSlice(off), want) {
		t.Errorf("page %#x lost its contents in a delta save: %s", off, describePage(f.pageSlice(off)))
	}
}

// TestImageDeltaByHash saves a delta without SaveOpts.Dirty: every page is
// hashed and compared with the parent's.
func TestImageDeltaByHash(t *testing.T) {
	tf := newImageTestFile(t, 32)
	parent := saveTestImage(t, tf.f, nil, nil)
	tf.write(2, 1)
	tf.write(7, 1)
	delta := saveTestImage(t, tf.f, parent, nil)
	if n := uint64(len(delta.pages)); n != 2*page {
		t.Errorf("delta wrote %d bytes, want %d", n, 2*page)
	}
	checkSameContents(t, loadTestImage(t, delta), tf.f, tf.fr)
}

// TestImageChain saves chains of deltas and restores every link. Each image
// must refer, for every page, to the image of the chain that last wrote it,
// and list exactly those images as its layers, in order of first reference.
func TestImageChain(t *testing.T) {
	for _, depth := range []int{2, 15} {
		t.Run(fmt.Sprintf("depth %d", depth), func(t *testing.T) {
			const pages = 128
			tf := newImageTestFile(t, pages)
			rng := rand.New(rand.NewSource(int64(depth)))
			chain := []*testImage{saveTestImage(t, tf.f, nil, nil)}
			// writer[off] is the index in chain of the image that wrote the
			// page at off last, or -1 if the page is zero.
			writer := make(map[uint64]int)
			for off := tf.fr.Start; off < tf.fr.End; off += page {
				writer[off] = 0
				if (off-tf.fr.Start)/page%3 == 0 {
					writer[off] = -1
				}
			}
			for gen := uint64(1); gen < uint64(depth); gen++ {
				clear(tf.modified)
				for j := 0; j < pages/8; j++ {
					i := uint64(rng.Intn(pages))
					tf.write(i, gen)
					writer[tf.fr.Start+i*page] = int(gen)
				}
				ti := saveTestImage(t, tf.f, chain[len(chain)-1], tf.clean)
				chain = append(chain, ti)
				checkSameContents(t, loadTestImage(t, ti), tf.f, tf.fr)
				checkLayerReferences(t, ti, chain, writer, tf.fr)
			}
		})
	}
}

// checkLayerReferences checks that every page of fr in ti refers to the layer
// that is the image of chain that wrote it last (writer, as an index in chain,
// or -1 for a zero page), and that ti's layers are exactly the images that its
// pages refer to, in order of first reference in fr.
func checkLayerReferences(t *testing.T, ti *testImage, chain []*testImage, writer map[uint64]int, fr memmap.FileRange) {
	t.Helper()
	want := []checkpointimage.Digest{ti.img.Digest}
	listed := map[checkpointimage.Digest]bool{ti.img.Digest: true}
	digests := ti.layerDigests()
	got := pageLayers(ti.mfImage(), fr)
	for off := fr.Start; off < fr.End; off += page {
		w := writer[off]
		if w < 0 {
			if got[off] != -1 {
				t.Errorf("zero page %#x refers to layer %d", off, got[off])
			}
			continue
		}
		d := chain[w].img.Digest
		if !listed[d] {
			listed[d] = true
			want = append(want, d)
		}
		if got[off] < 0 || got[off] >= len(digests) || digests[got[off]] != d {
			t.Errorf("page %#x refers to layer %d, want the image %d of the chain (%v)", off, got[off], w, d)
		}
	}
	if !slices.Equal(digests, want) {
		t.Errorf("layers are %v, want %v", digests, want)
	}
}

// TestImageChainShortens checks that a chain drops the images that no page
// refers to any more.
func TestImageChainShortens(t *testing.T) {
	tf := newImageTestFile(t, 8)
	a := saveTestImage(t, tf.f, nil, nil)
	for i := uint64(0); i < 8; i++ {
		tf.write(i, 1)
	}
	b := saveTestImage(t, tf.f, a, tf.clean)
	clear(tf.modified)
	tf.write(0, 2)
	c := saveTestImage(t, tf.f, b, tf.clean)
	// Every page of a was rewritten in b, so c refers to b only.
	if layers := c.img.Layers(); len(layers) != 2 || layers[1].Digest != b.img.Digest {
		t.Errorf("layers = %+v, want [self, %v]", layers, b.img.Digest)
	}
	checkSameContents(t, loadTestImage(t, c), tf.f, tf.fr)
}

// TestImagePagesContent03 is CRIU's pages_content03: three levels of a chain
// with overlapping changes, each restored.
func TestImagePagesContent03(t *testing.T) {
	tf := newImageTestFile(t, 30)
	ti := saveTestImage(t, tf.f, nil, nil)
	for level, r := range [][2]uint64{{0, 15}, {10, 25}, {5, 30}} {
		clear(tf.modified)
		for i := r[0]; i < r[1]; i++ {
			tf.write(i, uint64(level+1))
		}
		ti = saveTestImage(t, tf.f, ti, tf.clean)
		checkSameContents(t, loadTestImage(t, ti), tf.f, tf.fr)
	}
}

// TestImageCycled restores, modifies and saves the restored MemoryFile as a
// delta of the image it was restored from, repeatedly (Firecracker's
// test_cycled_snapshot_restore).
func TestImageCycled(t *testing.T) {
	tf := newImageTestFile(t, 32)
	ti := saveTestImage(t, tf.f, nil, nil)
	for cycle := uint64(1); cycle <= 5; cycle++ {
		f := loadTestImage(t, ti)
		if err := f.AwaitLoadAll(); err != nil {
			t.Fatalf("AwaitLoadAll: %v", err)
		}
		next := &imageTestFile{f: f, fr: tf.fr, modified: make(map[uint64]bool)}
		next.write(cycle, cycle)
		next.write(cycle+10, cycle)
		ti = saveTestImage(t, f, ti, next.clean)
		checkSameContents(t, loadTestImage(t, ti), f, tf.fr)
	}
}

// TestImageAnyOrder loads an image whose pages file holds its pages in reverse
// order, and checks that background loading reads it sequentially.
func TestImageAnyOrder(t *testing.T) {
	tf := newImageTestFile(t, 32)
	ti := saveTestImage(t, tf.f, nil, nil)

	// Rewrite the pages file with pages in reverse order, one extent each.
	mf := ti.img.MemoryFiles[0]
	m := ti.mfImage()
	var pages []uint64
	for _, e := range m.Extents {
		for off := e.Start; off < e.End; off += page {
			pages = append(pages, off)
		}
	}
	slices.Reverse(pages)
	var pagesFile []byte
	mf.Extents = nil
	for i, off := range pages {
		e, _ := m.ExtentAt(off)
		pagesFile = append(pagesFile, ti.pages[e.OffsetOf(off):][:page]...)
		mf.Extents = append(mf.Extents, &pgallocpb.ExtentProto{Start: off, End: off + page, Offset: uint64(i) * page})
	}
	slices.SortFunc(mf.Extents, func(a, b *pgallocpb.ExtentProto) int { return int(a.Start/page) - int(b.Start/page) })
	var meta bytes.Buffer
	w := checkpointimage.NewWriter(&meta, &pgallocpb.ImageProto{
		Layers:   []*pgallocpb.LayerProto{{}},
		PageHash: checkpointimage.PageHashXXH64,
	})
	if err := checkpointimage.WriteRecord(w, mf); err != nil {
		t.Fatal(err)
	}
	img, err := w.Finish(uint64(len(pagesFile)))
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	reordered := &testImage{img: img, pages: pagesFile, layers: [][]byte{pagesFile}}

	src := &offsetRecorder{r: bytes.NewReader(pagesFile)}
	f2 := loadTestImage(t, reordered, stateio.NewIOReader(src, 64<<10, 16, 1))
	// Let loading complete without awaiting pages, which would read them
	// first.
	for deadline := time.Now().Add(10 * time.Second); f2.IsAsyncLoading(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("loading did not complete")
		}
	}
	src.mu.Lock()
	if !slices.IsSorted(src.offsets) {
		t.Errorf("pages file read at offsets %v, want increasing", src.offsets)
	}
	src.mu.Unlock()
	checkSameContents(t, f2, tf.f, tf.fr)
}

// offsetRecorder records the offsets of reads.
type offsetRecorder struct {
	r       *bytes.Reader
	mu      sync.Mutex
	offsets []int64
}

// ReadAt implements io.ReaderAt.ReadAt.
func (o *offsetRecorder) ReadAt(p []byte, off int64) (int, error) {
	o.mu.Lock()
	o.offsets = append(o.offsets, off)
	o.mu.Unlock()
	return o.r.ReadAt(p, off)
}

// gatedReader blocks reads until its gate is opened.
type gatedReader struct {
	r    *bytes.Reader
	gate chan struct{}
}

// ReadAt implements io.ReaderAt.ReadAt.
func (g *gatedReader) ReadAt(p []byte, off int64) (int, error) {
	<-g.gate
	return g.r.ReadAt(p, off)
}

// TestImageAwaitAcrossLayers checks that an access to a range whose pages are
// in two layers waits for both.
func TestImageAwaitAcrossLayers(t *testing.T) {
	tf := newImageTestFile(t, 8)
	for i := uint64(0); i < 8; i++ {
		tf.write(i, 1)
	}
	parent := saveTestImage(t, tf.f, nil, nil)
	clear(tf.modified)
	for i := uint64(0); i < 8; i += 2 {
		tf.write(i, 2)
	}
	delta := saveTestImage(t, tf.f, parent, tf.clean)

	gates := []*gatedReader{
		{r: bytes.NewReader(delta.layers[0]), gate: make(chan struct{})},
		{r: bytes.NewReader(delta.layers[1]), gate: make(chan struct{})},
	}
	f2 := loadTestImage(t, delta,
		stateio.NewIOReader(gates[0], 64<<10, 16, 4),
		stateio.NewIOReader(gates[1], 64<<10, 16, 4))
	done := make(chan error, 1)
	go func() {
		_, err := f2.MapInternal(tf.fr, hostarch.Read)
		done <- err
	}()
	close(gates[0].gate)
	select {
	case err := <-done:
		t.Fatalf("MapInternal returned (%v) before layer 1 was loaded", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gates[1].gate)
	if err := <-done; err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	checkSameContents(t, f2, tf.f, tf.fr)
}

// TestImageDeltaDuringLoad checks that a delta of the image a MemoryFile is
// still loading from neither waits for loading nor reads the clean pages,
// which loading has not filled yet, and refers them to the image.
func TestImageDeltaDuringLoad(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		t.Run(fmt.Sprintf("ExcludeCommittedZeroPages=%t", exclude), func(t *testing.T) {
			tf := newImageTestFile(t, 8)
			parent := saveTestImage(t, tf.f, nil, nil)

			gate := &gatedReader{r: bytes.NewReader(parent.layers[0]), gate: make(chan struct{})}
			f2 := loadTestImage(t, parent, stateio.NewIOReader(gate, 64<<10, 16, 4))
			var openGate sync.Once
			defer openGate.Do(func() { close(gate.gate) })
			saved := make(chan *testImage, 1)
			go func() {
				saved <- saveTestImageOpts(t, f2, parent, SaveOpts{
					Dirty:                     &DirtySet{},
					ExcludeCommittedZeroPages: exclude,
					PageHashes:                true,
				})
			}()
			var delta *testImage
			select {
			case delta = <-saved:
			case <-time.After(10 * time.Second):
				t.Fatalf("saving a delta of the image being loaded waits for loading")
			}
			openGate.Do(func() { close(gate.gate) })

			if got := len(delta.pages); got != 0 {
				t.Errorf("delta wrote %d bytes of pages, want 0", got)
			}
			parentLayers := pageLayers(parent.mfImage(), tf.fr)
			for off, layer := range pageLayers(delta.mfImage(), tf.fr) {
				want := parentLayers[off]
				if want >= 0 {
					want++
				}
				if layer != want {
					t.Errorf("page %#x: in layer %d of the delta, want %d", off, layer, want)
				}
			}
			checkSameContents(t, loadTestImage(t, delta), tf.f, tf.fr)
			checkSameContents(t, f2, tf.f, tf.fr)
		})
	}
}

// TestImageDeltaDuringLoadReadsDirtyPages checks that a delta of the image a
// MemoryFile is still loading from waits for the pages that are not clean
// although they have not been loaded, which it reads, and saves their
// contents as loaded: a page dirtied by a mark coarser than the writes it
// records, which holds the data loading writes, and a page decommitted while
// it loads, which holds zeros.
func TestImageDeltaDuringLoadReadsDirtyPages(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	const (
		marked      = 1
		decommitted = 2
	)
	tf.zero(decommitted)
	pageAt := func(i uint64) memmap.FileRange {
		return memmap.FileRange{Start: tf.fr.Start + i*page, End: tf.fr.Start + (i+1)*page}
	}

	gate := &gatedReader{r: bytes.NewReader(parent.layers[0]), gate: make(chan struct{})}
	f2 := loadTestImage(t, parent, stateio.NewIOReader(gate, 64<<10, 16, 4))
	var openGate sync.Once
	defer openGate.Do(func() { close(gate.gate) })
	f2.Decommit(pageAt(decommitted))
	saved := make(chan *testImage, 1)
	go func() {
		saved <- saveTestImageOpts(t, f2, parent, SaveOpts{
			Dirty:      dirtySetOf(f2, func(off uint64) bool { return off != pageAt(marked).Start && off != pageAt(decommitted).Start }),
			PageHashes: true,
		})
	}()
	select {
	case <-saved:
		t.Fatalf("saving a delta returned before the pages it reads were loaded")
	case <-time.After(50 * time.Millisecond):
	}
	openGate.Do(func() { close(gate.gate) })
	var delta *testImage
	select {
	case delta = <-saved:
	case <-time.After(10 * time.Second):
		t.Fatalf("saving a delta of the image being loaded did not complete after loading")
	}

	layers := pageLayers(delta.mfImage(), tf.fr)
	if got := layers[pageAt(marked).Start]; got != 1 {
		t.Errorf("marked page: in layer %d of the delta, want 1 (unchanged since the parent, by hash)", got)
	}
	if got := layers[pageAt(decommitted).Start]; got != -1 {
		t.Errorf("decommitted page: in layer %d of the delta, want none (zero)", got)
	}
	checkSameContents(t, loadTestImage(t, delta), tf.f, tf.fr)
	checkSameContents(t, f2, tf.f, tf.fr)
}

// TestImageDeltaDuringLoadWaitedBytes checks that the wait of a delta save for
// the pages that are not clean although they have not been loaded counts the
// bytes it waited for, as the log of the save reports them: those pages, and
// none once loading has completed.
func TestImageDeltaDuringLoadWaitedBytes(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	pageAt := func(i uint64) uint64 { return tf.fr.Start + i*page }
	// Pages 1 and 4 hold data (newImageTestFile), so that the image has them
	// to load.
	clean := func(off uint64) bool { return off != pageAt(1) && off != pageAt(4) }

	gate := &gatedReader{r: bytes.NewReader(parent.layers[0]), gate: make(chan struct{})}
	f2 := loadTestImage(t, parent, stateio.NewIOReader(gate, 64<<10, 16, 4))
	var openGate sync.Once
	defer openGate.Do(func() { close(gate.gate) })
	img := &imageSaver{dirty: dirtySetOf(f2, clean)}
	type result struct {
		waited uint64
		err    error
	}
	awaited := make(chan result, 1)
	go func() {
		waited, err := f2.awaitUncleanLoads(img)
		awaited <- result{waited, err}
	}()
	select {
	case <-awaited:
		t.Fatalf("awaitUncleanLoads returned before the pages it waits for were loaded")
	case <-time.After(50 * time.Millisecond):
	}
	openGate.Do(func() { close(gate.gate) })
	var res result
	select {
	case res = <-awaited:
	case <-time.After(10 * time.Second):
		t.Fatalf("awaitUncleanLoads did not return after loading")
	}
	if res.err != nil {
		t.Fatalf("awaitUncleanLoads: %v", res.err)
	}
	if want := uint64(2 * page); res.waited != want {
		t.Errorf("awaitUncleanLoads waited for %d bytes, want %d", res.waited, want)
	}

	if err := f2.AwaitLoadAll(); err != nil {
		t.Fatalf("AwaitLoadAll: %v", err)
	}
	if waited, err := f2.awaitUncleanLoads(img); err != nil || waited != 0 {
		t.Errorf("awaitUncleanLoads once loaded: waited for %d bytes, error %v; want 0, nil", waited, err)
	}
	checkSameContents(t, f2, tf.f, tf.fr)
}

// TestAwaitLoadsFindsUnloadedPagesUnlocked checks that a wait for pages that
// are all loaded, or were never to be loaded, does not lock the MemoryFile:
// a save's or a pre-copy round's wait finds the pages that remain to be
// loaded among ranges that may be many, as those of a dirty set are, while
// allocations and frees wait for the MemoryFile's lock.
func TestAwaitLoadsFindsUnloadedPagesUnlocked(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	gate := &gatedReader{r: bytes.NewReader(parent.layers[0]), gate: make(chan struct{})}
	f2 := loadTestImage(t, parent, stateio.NewIOReader(gate, 64<<10, 16, 4))
	var openGate sync.Once
	defer openGate.Do(func() { close(gate.gate) })
	loads := f2.asyncPageLoad.Load()
	if loads == nil {
		t.Fatalf("no async page loading")
	}
	// Every third page is zero (newImageTestFile): the image has no data
	// for it, so it is not loaded.
	var zero []memmap.FileRange
	for i := uint64(0); i < 8; i += 3 {
		off := tf.fr.Start + i*page
		zero = append(zero, memmap.FileRange{Start: off, End: off + page})
	}
	f2.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := f2.awaitLoads(loads, zero)
		done <- err
	}()
	select {
	case err := <-done:
		f2.mu.Unlock()
		if err != nil {
			t.Fatalf("awaitLoads: %v", err)
		}
	case <-time.After(10 * time.Second):
		f2.mu.Unlock()
		<-done
		t.Fatalf("awaitLoads of pages that are not loading waited for the MemoryFile's lock")
	}
	openGate.Do(func() { close(gate.gate) })
	if err := f2.AwaitLoadAll(); err != nil {
		t.Fatalf("AwaitLoadAll: %v", err)
	}
}

// TestAwaitLoadsUnloadedIn checks that the pages that awaitLoads finds to
// remain to be loaded among many ranges, in several layers, are those that
// do, as a look-up of each range in each layer finds them.
func TestAwaitLoadsUnloadedIn(t *testing.T) {
	const pages = 512
	tf := newImageTestFile(t, pages)
	parent := saveTestImage(t, tf.f, nil, nil)
	for i := uint64(0); i < pages; i += 5 {
		tf.write(i, 1)
	}
	delta := saveTestImage(t, tf.f, parent, tf.clean)
	var gates []*gatedReader
	var readers []stateio.AsyncReader
	for _, layer := range delta.layers {
		g := &gatedReader{r: bytes.NewReader(layer), gate: make(chan struct{})}
		gates = append(gates, g)
		readers = append(readers, stateio.NewIOReader(g, 64<<10, 16, 4))
	}
	f2 := loadTestImage(t, delta, readers...)
	defer func() {
		for _, g := range gates {
			close(g.gate)
		}
		if err := f2.AwaitLoadAll(); err != nil {
			t.Errorf("AwaitLoadAll: %v", err)
		}
	}()
	loads := f2.asyncPageLoad.Load()
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 100; round++ {
		// Random sorted ranges of the allocation, some of them abutting.
		var frs []memmap.FileRange
		for off := tf.fr.Start; off < tf.fr.End; {
			n := uint64(1+rng.Intn(8)) * page
			if rng.Intn(2) == 0 {
				frs = append(frs, memmap.FileRange{Start: off, End: min(off+n, tf.fr.End)})
			}
			off += n
		}
		var want []memmap.FileRange
		for _, amfl := range loads.amfls {
			if amfl == nil {
				continue
			}
			amfl.pf.mu.Lock()
			for _, fr := range frs {
				for ulseg := amfl.unloaded.LowerBoundSegment(fr.Start); ulseg.Ok() && ulseg.Start() < fr.End; ulseg = ulseg.NextSegment() {
					want = append(want, ulseg.Range().Intersect(fr))
				}
			}
			amfl.pf.mu.Unlock()
		}
		want = mergeFileRanges(want)
		if got := loads.unloadedIn(frs); !slices.Equal(got, want) {
			t.Fatalf("round %d: unloadedIn(%v) = %v, want %v", round, frs, got, want)
		}
	}
}

// BenchmarkAwaitUncleanLoads measures how long a save's wait takes to find
// the pages it must wait for among the pages of a large MemoryFile that
// remain to be loaded, few of them dirty: it is spent with the sandbox
// paused.
func BenchmarkAwaitUncleanLoads(b *testing.B) {
	const pages = 1 << 18 // 1 GiB
	f := newTestMemoryFile(b, testMemoryFileOpts{})
	fr, err := f.Allocate(pages*page, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		b.Fatalf("Allocate: %v", err)
	}
	dirty := dirtySetOf(f, func(off uint64) bool { return (off-fr.Start)/page%4096 != 0 })
	img := &imageSaver{dirty: dirty}
	// Every page of the allocation remains to be loaded, in one layer whose
	// loader never runs.
	amfl := &asyncMemoryFileLoad{f: f, pf: &AsyncPagesFileLoad{}}
	amfl.unloaded.InsertRange(fr, aplUnloadedInfo{})
	loads := &asyncMemoryFileLoads{amfls: []*asyncMemoryFileLoad{amfl}}
	b.ResetTimer()
	for b.Loop() {
		var unclean []memmap.FileRange
		img.forEachUnclean(fr, func(ufr memmap.FileRange) { unclean = append(unclean, ufr) })
		if got := loads.unloadedIn(unclean); len(got) != pages/4096 {
			b.Fatalf("found %d ranges to wait for, want %d", len(got), pages/4096)
		}
	}
}

// TestImageLoadRejects checks that loading fails, before reading any page, if
// the pages files do not match the image's layers.
func TestImageLoadRejects(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	tf.write(1, 1)
	delta := saveTestImage(t, tf.f, parent, tf.clean)

	f := newTestMemoryFile(t, testMemoryFileOpts{})
	ar := stateio.NewIOReader(bytes.NewReader(delta.pages), 64<<10, 16, 4)
	apfl, err := StartAsyncPagesFileLoad(ar, func(error) {}, nil, AsyncPagesFileLoadOpts{})
	if err != nil {
		t.Fatalf("StartAsyncPagesFileLoad: %v", err)
	}
	defer apfl.MemoryFilesDone()
	opts := LoadOpts{Image: delta.img.Proto, PagesFiles: []*AsyncPagesFileLoad{apfl}}
	if err := f.LoadFrom(context.Background(), delta.img.MemoryFileRecords(), &opts); err == nil {
		t.Errorf("LoadFrom with one pages file for two layers succeeded")
	}
}

// TestImageSaveRejects checks the preconditions of saving a delta.
func TestImageSaveRejects(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	for _, tc := range []struct {
		name string
		opts SaveOpts
	}{
		{"no pages file", SaveOpts{Base: parent.mfImage(), BaseLayers: []uint32{1}}},
		{"unmapped layer", SaveOpts{Base: parent.mfImage()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			var pagesBuf bytes.Buffer
			if tc.name != "no pages file" {
				apfs, err := StartAsyncPagesFileSave(stateio.NewIOWriter(&pagesBuf, 64<<10, 16, 4), func(error) {})
				if err != nil {
					t.Fatal(err)
				}
				defer apfs.MemoryFilesDone()
				opts.PagesFile = apfs
			}
			if err := tf.f.SaveTo(context.Background(), &bytes.Buffer{}, &opts); err == nil {
				t.Errorf("SaveTo succeeded")
			}
		})
	}
}

// TestImageInlineFramesMetadata checks that a MemoryFile saved inline, into a
// state file, frames its metadata as it frames its pages: as data that is not
// an object of the state file, which a tool that prints the state file's
// objects, as runsc statefile does, skips whole. Without the frame, the tool
// read the metadata's bytes as objects, and failed or not by their content.
func TestImageInlineFramesMetadata(t *testing.T) {
	tf := newImageTestFile(t, 8)
	var saved bytes.Buffer
	if err := tf.f.SaveTo(context.Background(), &saved, &SaveOpts{}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	var printed strings.Builder
	if err := pretty.PrintText(&printed, bytes.NewReader(saved.Bytes())); err != nil {
		t.Fatalf("PrintText: %v", err)
	}
	region := regexp.MustCompile(`^\((\d+) bytes non-object data\)$`)
	var framed int
	for _, line := range strings.Split(strings.TrimSuffix(printed.String(), "\n"), "\n") {
		m := region.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("PrintText printed %q, want only regions of non-object data", line)
		}
		n, _ := strconv.Atoi(m[1])
		var header bytes.Buffer
		if err := state.WriteHeader(&wire.Writer{Writer: &header}, uint64(n), false /* object */); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		framed += header.Len() + n
	}
	if framed != saved.Len() {
		t.Errorf("PrintText found %d bytes in regions of non-object data, want all %d bytes saved", framed, saved.Len())
	}

	// The MemoryFile loads from what it saved.
	f2 := newTestMemoryFile(t, testMemoryFileOpts{})
	if err := f2.LoadFrom(context.Background(), &saved, &LoadOpts{Image: &pgallocpb.ImageProto{Layers: []*pgallocpb.LayerProto{{}}}}); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	checkSameContents(t, f2, tf.f, tf.fr)
}
