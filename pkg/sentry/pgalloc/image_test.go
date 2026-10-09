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
	"slices"
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
// parent with clean as SaveOpts.Clean, parent's layers becoming the following
// layers of the image.
func saveTestImage(t *testing.T, f *MemoryFile, parent *testImage, clean func(uint64) bool) *testImage {
	t.Helper()
	return saveTestImageOpts(t, f, parent, SaveOpts{Clean: clean, PageHashes: true})
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
		apfl, err := StartAsyncPagesFileLoad(ar, func(err error) {
			if err != nil {
				t.Errorf("async page loading from layer %d: %v", i, err)
			}
		}, nil)
		if err != nil {
			t.Fatalf("StartAsyncPagesFileLoad: %v", err)
		}
		t.Cleanup(apfl.MemoryFilesDone)
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

// TestImageDeltaByHash saves a delta without SaveOpts.Clean: every page is
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

// TestImageChain saves chains of deltas and restores every link.
func TestImageChain(t *testing.T) {
	for _, depth := range []int{2, 15} {
		t.Run(fmt.Sprintf("depth %d", depth), func(t *testing.T) {
			const pages = 128
			tf := newImageTestFile(t, pages)
			rng := rand.New(rand.NewSource(int64(depth)))
			ti := saveTestImage(t, tf.f, nil, nil)
			for gen := uint64(1); gen < uint64(depth); gen++ {
				clear(tf.modified)
				for j := 0; j < pages/8; j++ {
					tf.write(uint64(rng.Intn(pages)), gen)
				}
				ti = saveTestImage(t, tf.f, ti, tf.clean)
				checkSameContents(t, loadTestImage(t, ti), tf.f, tf.fr)
			}
			if n := len(ti.img.Layers()); n < 2 || n > depth {
				t.Errorf("last image has %d layers", n)
			}
		})
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
					Clean:                     func(uint64) bool { return true },
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

// TestImageLoadRejects checks that loading fails, before reading any page, if
// the pages files do not match the image's layers.
func TestImageLoadRejects(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	tf.write(1, 1)
	delta := saveTestImage(t, tf.f, parent, tf.clean)

	f := newTestMemoryFile(t, testMemoryFileOpts{})
	ar := stateio.NewIOReader(bytes.NewReader(delta.pages), 64<<10, 16, 4)
	apfl, err := StartAsyncPagesFileLoad(ar, func(error) {}, nil)
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
