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
	"math"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// loaderTestImage returns a MemoryFile holding one allocation of length bytes
// written with generation 1, its expected contents, and its saved image. The
// allocation is saved as one pages file range starting at offset 0, so its
// MemoryFile offset off is at pages file offset off-fr.Start.
func loaderTestImage(t *testing.T, length uint64) (memmap.FileRange, pageGens, *testImage) {
	t.Helper()
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocate(t, f, length, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(fr) })
	gens := make(pageGens)
	gens.write(t, f, fr, 1)
	return fr, gens, saveImage(t, SaveOpts{}, f)
}

// waitForWaiters waits until n goroutines are waiting for pages from apfl.
func waitForWaiters(t *testing.T, apfl *AsyncPagesFileLoad, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		apfl.mu.Lock()
		numWaiters := apfl.numWaiters
		apfl.mu.Unlock()
		if numWaiters == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d waiters (have %d)", n, numWaiters)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitAsync reads fr through MapInternal in a new goroutine, as a page fault
// or a syscall does, and returns a channel that receives the result when the
// pages are loaded.
func awaitAsync(f *MemoryFile, fr memmap.FileRange) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := f.MapInternal(fr, hostarch.Read)
		ch <- err
	}()
	return ch
}

// receive receives the result of an operation from ch, failing the test if it
// takes longer than testWaitTimeout.
func receive(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(testWaitTimeout):
		t.Fatalf("timed out after %v waiting for %s", testWaitTimeout, what)
		return nil
	}
}

// TestAsyncLoadAwaitedPageWaitsBehindQueuedReads reproduces, in virtual time,
// how long a page fault waits for its page during a background restore: the
// loader keeps the reader's queue full of sequential reads, so a fault's read
// is issued only when one of them completes, and then waits behind every byte
// still queued on the device. The test pins down today's behavior; changes to
// the loader that shorten the wait must update the expected times.
func TestAsyncLoadAwaitedPageWaitsBehindQueuedReads(t *testing.T) {
	const (
		maxParallel  = 4
		maxReadBytes = 256 << 10
		bandwidth    = 100 << 20 // bytes per second
		latency      = time.Millisecond
	)
	fr, gens, img := loaderTestImage(t, 8<<20)
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: maxReadBytes,
		maxParallel:  maxParallel,
		bandwidth:    bandwidth,
		latency:      latency,
		manual:       true,
	})
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })

	// The loader fills the queue with background reads of the image's first
	// pages.
	r.waitInflight(maxParallel)
	// The application touches the last page.
	last := memmap.FileRange{fr.End - hostarch.PageSize, fr.End}
	lastOff := int64(last.Start - fr.Start)
	done := awaitAsync(restored, last)
	waitForWaiters(t, l.apfl, 1)

	for r.advanceToNext().off != lastOff {
	}
	if err := receive(t, done, "the awaited page"); err != nil {
		t.Fatalf("MapInternal(%v): %v", last, err)
	}

	awaited, _ := r.readAt(lastOff)
	transfer := func(n uint64) time.Duration { return time.Duration(n * uint64(time.Second) / bandwidth) }
	// The awaited read is issued when the first background read completes...
	if want := transfer(maxReadBytes) + latency; awaited.submitted != want {
		t.Errorf("awaited read submitted at %v, want %v (when the first queued read completes)", awaited.submitted, want)
	}
	if awaited.len != hostarch.PageSize {
		t.Errorf("awaited read is %d bytes, want one page", awaited.len)
	}
	// ... and completes after the device has transferred every byte queued
	// before it.
	if want := transfer(maxParallel*maxReadBytes+hostarch.PageSize) + latency; awaited.completed != want {
		t.Errorf("awaited read completed at %v, want %v", awaited.completed, want)
	}
	if waited, own := awaited.completed-awaited.submitted, transfer(hostarch.PageSize)+latency; waited <= own {
		t.Errorf("awaited read took %v, no longer than its own %v", waited, own)
	}

	l.apfl.mu.Lock()
	totalWaiters, bytesWaited := l.apfl.totalWaiters, l.apfl.bytesWaited
	l.apfl.mu.Unlock()
	if totalWaiters != 1 || bytesWaited != hostarch.PageSize {
		t.Errorf("waiter accounting: %d waiters for %d bytes, want 1 for %d", totalWaiters, bytesWaited, hostarch.PageSize)
	}

	r.finish()
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	gens.checkPages(t, restored, fr)
	checkInvariants(t, restored)
}

// TestAsyncLoadAwaitedReadsInArrivalOrder checks that awaited ranges are read
// before unawaited ones, in the order in which they were first awaited, one
// read at a time (the rest of a range longer than a read goes to the back of
// the queue, so that it does not delay later waiters), and that a waiter
// wakes only when all of its range is loaded.
func TestAsyncLoadAwaitedReadsInArrivalOrder(t *testing.T) {
	fr, gens, img := loaderTestImage(t, 64*hostarch.PageSize)
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: hostarch.PageSize,
		maxParallel:  1,
		latency:      time.Millisecond,
		manual:       true,
	})
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })
	r.waitInflight(1)

	page := func(i uint64) memmap.FileRange {
		return memmap.FileRange{fr.Start + i*hostarch.PageSize, fr.Start + (i+1)*hostarch.PageSize}
	}
	// Three waiters, in this order: page 50; pages 30-31; page 40.
	first := awaitAsync(restored, page(50))
	waitForWaiters(t, l.apfl, 1)
	second := awaitAsync(restored, memmap.FileRange{page(30).Start, page(31).End})
	waitForWaiters(t, l.apfl, 2)
	third := awaitAsync(restored, page(40))
	waitForWaiters(t, l.apfl, 3)

	// Complete the background read in flight, then the awaited reads one at
	// a time.
	r.advanceToNext()
	for _, want := range []struct {
		page uint64
		done <-chan error
	}{
		{50, first},
		{30, nil},
		{40, third},
		{31, second},
	} {
		rd := r.advanceToNext()
		if got := uint64(rd.off) / hostarch.PageSize; got != want.page {
			t.Fatalf("read page %d, want page %d", got, want.page)
		}
		if want.done == nil {
			continue
		}
		if err := receive(t, want.done, "a waiter"); err != nil {
			t.Fatalf("MapInternal: %v", err)
		}
		if want.page == 40 {
			select {
			case err := <-second:
				t.Fatalf("waiter for pages 30-31 woke (%v) with page 31 unloaded", err)
			default:
			}
		}
	}

	r.finish()
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	gens.checkPages(t, restored, fr)
}

// TestAsyncLoadCompletionAccounting checks the loader's tracking of the first
// unloaded offset, which lets MapInternal skip locking for loaded pages, and
// its completion of MemoryFile loading.
func TestAsyncLoadCompletionAccounting(t *testing.T) {
	fr, gens, img := loaderTestImage(t, 4*hostarch.PageSize)
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: hostarch.PageSize,
		maxParallel:  1,
		latency:      time.Millisecond,
		manual:       true,
	})
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })

	loads := restored.asyncPageLoad.Load()
	if loads == nil {
		t.Fatalf("IsAsyncLoading() = false before any page is loaded")
	}
	amfl := loads.amfls[0]
	for i := uint64(0); i < 4; i++ {
		r.waitPending()
		if got, want := amfl.minUnloaded.Load(), fr.Start+i*hostarch.PageSize; got != want {
			t.Errorf("before loading page %d: minUnloaded = %#x, want %#x", i, got, want)
		}
		r.advanceToNext()
	}
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	if err := receive(t, l.mfErrs, "MemoryFile loading"); err != nil {
		t.Errorf("MemoryFile load completed with error %v", err)
	}
	if got := amfl.minUnloaded.Load(); got != math.MaxUint64 {
		t.Errorf("after loading: minUnloaded = %#x, want MaxUint64", got)
	}
	if restored.IsAsyncLoading() {
		t.Errorf("IsAsyncLoading() = true after loading completed")
	}
	l.apfl.mu.Lock()
	bytesLoaded := l.apfl.bytesLoaded
	l.apfl.mu.Unlock()
	if bytesLoaded != fr.Length() {
		t.Errorf("bytesLoaded = %d, want %d", bytesLoaded, fr.Length())
	}
	gens.checkPages(t, restored, fr)
}

// TestAsyncLoadCancelWasteLoad checks that pages freed before they are loaded
// are never read.
func TestAsyncLoadCancelWasteLoad(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	kept := allocate(t, f, 8*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	freed := allocate(t, f, 8*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() {
		f.DecRef(kept)
		f.DecRef(freed)
	})
	gens := make(pageGens)
	gens.write(t, f, kept, 1)
	gens.write(t, f, freed, 1)
	img := saveImage(t, SaveOpts{}, f)

	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: hostarch.PageSize,
		maxParallel:  1,
		latency:      time.Millisecond,
		manual:       true,
	})
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	r.waitInflight(1)
	// The application frees the second allocation while the loader reads the
	// first.
	restored.DecRef(freed)
	r.finish()
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	freedOff := int64(freed.Start - kept.Start)
	for _, rd := range r.readsSnapshot() {
		if rd.off+int64(rd.len) > freedOff {
			t.Errorf("read of pages file range [%#x, %#x) includes freed pages", rd.off, rd.off+int64(rd.len))
		}
	}
	gens.checkPages(t, restored, kept)
	restored.DecRef(kept)
	checkInvariants(t, restored)
}

// TestAsyncLoadErrorIsSticky checks that a failed read stops loading and fails
// every waiter for pages that are not loaded, now and later, while loaded
// pages stay usable.
func TestAsyncLoadErrorIsSticky(t *testing.T) {
	fr, gens, img := loaderTestImage(t, 16*hostarch.PageSize)
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: hostarch.PageSize,
		maxParallel:  1,
		latency:      time.Millisecond,
		manual:       true,
	})
	r.failAt(8 * hostarch.PageSize)
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })
	r.waitInflight(1)
	page := func(i uint64) memmap.FileRange {
		return memmap.FileRange{fr.Start + i*hostarch.PageSize, fr.Start + (i+1)*hostarch.PageSize}
	}
	failed := awaitAsync(restored, page(8))
	waitForWaiters(t, l.apfl, 1)

	r.finish()
	if err := receive(t, failed, "the waiter for the failed page"); err != linuxerr.EIO {
		t.Errorf("waiter for the failed page: got %v, want EIO", err)
	}
	if err := l.wait(t); err != linuxerr.EIO {
		t.Errorf("async page loading: got %v, want EIO", err)
	}
	if err := receive(t, l.mfErrs, "MemoryFile loading"); err != linuxerr.EIO {
		t.Errorf("MemoryFile load completed with %v, want EIO", err)
	}
	// The loader stopped at the failure: the page it loaded before is
	// usable, the others fail.
	gens.checkPages(t, restored, page(0))
	for _, i := range []uint64{1, 8, 15} {
		if _, err := restored.MapInternal(page(i), hostarch.Read); err != linuxerr.EIO {
			t.Errorf("MapInternal(page %d) after the failure: got %v, want EIO", i, err)
		}
	}
	if err := restored.AwaitLoadAll(); err != linuxerr.EIO {
		t.Errorf("AwaitLoadAll after the failure: got %v, want EIO", err)
	}
}

// TestAsyncLoadErrorWakesLoadedWaiters checks that when a read fails, the
// waiters for pages loaded by reads completed in the same batch are woken.
func TestAsyncLoadErrorWakesLoadedWaiters(t *testing.T) {
	fr, gens, img := loaderTestImage(t, 2*hostarch.PageSize)
	// Both reads complete at the same virtual time, so that the loader gets
	// both completions from one Wait: first page 0's, then page 1's, which
	// fails.
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: hostarch.PageSize,
		maxParallel:  2,
		latency:      time.Millisecond,
		manual:       true,
	})
	r.failAt(hostarch.PageSize)
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })
	r.waitInflight(2)
	page0 := memmap.FileRange{fr.Start, fr.Start + hostarch.PageSize}
	loaded := awaitAsync(restored, page0)
	waitForWaiters(t, l.apfl, 1)

	r.finish()
	if err := receive(t, loaded, "the waiter for the loaded page"); err != nil {
		t.Errorf("waiter for the loaded page: got %v, want success", err)
	}
	if err := l.wait(t); err != linuxerr.EIO {
		t.Errorf("async page loading: got %v, want EIO", err)
	}
	gens.checkPages(t, restored, page0)
}

// TestSaveDuringAsyncLoad checks that saving a MemoryFile whose pages are
// still loading waits for them and saves them all.
func TestSaveDuringAsyncLoad(t *testing.T) {
	fr, gens, img := loaderTestImage(t, 32*hostarch.PageSize)
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: 4 * hostarch.PageSize,
		maxParallel:  1,
		latency:      time.Millisecond,
		manual:       true,
	})
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })
	r.waitInflight(1)

	var (
		meta, pages bytes.Buffer
		saveImg     *checkpointimage.Image
	)
	saved := make(chan error, 1)
	go func() {
		var err error
		saveImg, err = saveImageTo(&meta, stateio.NewIOWriter(&pages, 256<<10, 64, 4), SaveOpts{}, restored)
		saved <- err
	}()
	waitForWaiters(t, l.apfl, 1)
	r.finish()
	if err := receive(t, saved, "saving"); err != nil {
		t.Fatalf("saving during loading: %v", err)
	}
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	img2 := &testImage{meta: meta.Bytes(), pages: pages.Bytes(), img: saveImg, layers: [][]byte{pages.Bytes()}}
	if !bytes.Equal(img2.pages, img.pages) {
		t.Errorf("image saved during loading differs from the image loaded")
	}
	again := newTestMemoryFile(t, testMemoryFileOpts{})
	loadImage(t, img2, again)
	t.Cleanup(func() { releaseAll(t, again) })
	gens.checkPages(t, again, fr)
}
