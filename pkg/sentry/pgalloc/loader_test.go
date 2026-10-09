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

// Pages file models for the loader's timing tests: reads of 256 KiB from a
// disk throttled to 100 MiB/s (experiment 07's bench), from a local disk at
// 1.5 GiB/s, and reads of 4 MiB from an object store serving 8 at a time, each
// at 64 MiB/s with 1 ms of latency.
var (
	testDisk = testPagesFileOpts{
		maxReadBytes: 256 << 10,
		maxParallel:  128,
		bandwidth:    100 << 20,
		latency:      100 * time.Microsecond,
	}
	testFastDisk = testPagesFileOpts{
		maxReadBytes: 256 << 10,
		maxParallel:  128,
		bandwidth:    1536 << 20,
		latency:      100 * time.Microsecond,
	}
	testObjectStore = testPagesFileOpts{
		maxReadBytes: 4 << 20,
		maxParallel:  8,
		channels:     8,
		bandwidth:    64 << 20,
		latency:      time.Millisecond,
	}
)

// transferTime returns how long opts's device takes to transfer n bytes.
func (opts testPagesFileOpts) transferTime(n uint64) time.Duration {
	return time.Duration(n * uint64(time.Second) / opts.bandwidth)
}

// waitOnlyReader hides the WaitOr method of a stateio.WaitOrAsyncReader.
type waitOnlyReader struct {
	stateio.AsyncReader
}

// TestAsyncLoadFaultWait checks how long a page fault waits for its page in
// the middle of a background restore: its read is enqueued at once, although
// the loader is waiting for background reads to complete, and the background
// reads in flight ahead of it are bounded: on a disk, which serves reads in
// order, to about one read and aplQueueDelay; on an object store, which serves
// them in parallel, to none, since a slot and a channel are left for it.
// Before these bounds, the fault waited for every background read in flight:
// 128 reads of 256 KiB at 100 MiB/s, 320 ms.
func TestAsyncLoadFaultWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts testPagesFileOpts
		size uint64
		// at is when the fault happens, once loading is steady.
		at time.Duration
		// maxWait is the longest the fault may wait for its page.
		maxWait time.Duration
	}{
		{
			name: "disk",
			opts: testDisk,
			size: 16 << 20,
			at:   50 * time.Millisecond,
			// The background reads in flight, at most a read and
			// aplQueueDelay, then the fault's own read.
			maxWait: testDisk.transferTime(testDisk.maxReadBytes) + aplQueueDelay +
				testDisk.transferTime(hostarch.PageSize) + 2*testDisk.latency,
		},
		{
			name:    "object store",
			opts:    testObjectStore,
			size:    64 << 20,
			at:      60 * time.Millisecond,
			maxWait: testObjectStore.transferTime(hostarch.PageSize) + testObjectStore.latency,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, gens, img := loaderTestImage(t, tc.size)
			opts := tc.opts
			opts.manual = true
			r := newTestPagesFile(t, img.pages, opts)
			r.useClock()
			restored := newTestMemoryFile(t, testMemoryFileOpts{})
			l := startLoad(t, img, r, restored)
			t.Cleanup(func() { releaseAll(t, restored) })

			for r.advanceToNext().completed < tc.at {
			}
			r.waitPending()
			last := memmap.FileRange{fr.End - hostarch.PageSize, fr.End}
			lastOff := int64(last.Start - fr.Start)
			if _, ok := r.readAt(lastOff); ok {
				t.Fatalf("the last page was read before the fault; load a larger image")
			}
			faultAt := time.Duration(r.nanotime())
			done := awaitAsync(restored, last)
			waitForWaiters(t, l.apfl, 1)
			for rd := r.advanceToNext(); rd.off != lastOff; rd = r.advanceToNext() {
			}
			if err := receive(t, done, "the awaited page"); err != nil {
				t.Fatalf("MapInternal(%v): %v", last, err)
			}

			awaited, _ := r.readAt(lastOff)
			if awaited.submitted != faultAt {
				t.Errorf("awaited read submitted at %v, want at once, at the fault's %v", awaited.submitted, faultAt)
			}
			if awaited.len != hostarch.PageSize {
				t.Errorf("awaited read is %d bytes, want one page", awaited.len)
			}
			waited := awaited.completed - faultAt
			t.Logf("the fault waited %v for its page", waited)
			if waited > tc.maxWait {
				t.Errorf("the fault waited %v for its page, want at most %v", waited, tc.maxWait)
			}

			r.finish()
			if err := l.wait(t); err != nil {
				t.Fatalf("async page loading: %v", err)
			}
			gens.checkPages(t, restored, fr)
			checkInvariants(t, restored)
		})
	}
}

// TestAsyncLoadFaultWaitWithoutWaitOr checks that a pages file that cannot
// interrupt its waits still serves faults first, at its next completion.
func TestAsyncLoadFaultWaitWithoutWaitOr(t *testing.T) {
	fr, gens, img := loaderTestImage(t, 4<<20)
	opts := testDisk
	opts.manual = true
	r := newTestPagesFile(t, img.pages, opts)
	r.useClock()
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, waitOnlyReader{r}, restored)
	t.Cleanup(func() { releaseAll(t, restored) })

	next := r.waitPending()
	last := memmap.FileRange{fr.End - hostarch.PageSize, fr.End}
	lastOff := int64(last.Start - fr.Start)
	done := awaitAsync(restored, last)
	waitForWaiters(t, l.apfl, 1)
	for rd := r.advanceToNext(); rd.off != lastOff; rd = r.advanceToNext() {
	}
	if err := receive(t, done, "the awaited page"); err != nil {
		t.Fatalf("MapInternal(%v): %v", last, err)
	}
	if awaited, _ := r.readAt(lastOff); awaited.submitted != next.completed {
		t.Errorf("awaited read submitted at %v, want at the next completion, %v", awaited.submitted, next.completed)
	}

	r.finish()
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	gens.checkPages(t, restored, fr)
}

// TestAsyncLoadBackgroundThroughput checks that bounding background reads
// does not slow loading: the whole image loads within 10% of the time the
// pages file takes to deliver it with every read it can have in flight but
// one. The cases include a pages file so slow that its bandwidth-delay
// product is less than one read, which loading still keeps busy.
func TestAsyncLoadBackgroundThroughput(t *testing.T) {
	slowDisk := testDisk
	slowDisk.bandwidth = 1 << 20
	for _, tc := range []struct {
		name string
		opts testPagesFileOpts
		size uint64
	}{
		{"disk", testDisk, 16 << 20},
		{"fast disk", testFastDisk, 16 << 20},
		{"slow disk", slowDisk, 2 << 20},
		{"object store", testObjectStore, 64 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, gens, img := loaderTestImage(t, tc.size)
			r := newTestPagesFile(t, img.pages, tc.opts)
			r.useClock()
			restored := newTestMemoryFile(t, testMemoryFileOpts{})
			l := startLoad(t, img, r, restored)
			t.Cleanup(func() { releaseAll(t, restored) })
			if err := l.wait(t); err != nil {
				t.Fatalf("async page loading: %v", err)
			}
			gens.checkPages(t, restored, fr)

			var took time.Duration
			for _, rd := range r.readsSnapshot() {
				took = max(took, rd.completed)
			}
			// Reads are served in rounds of as many as the pages file
			// transfers at once, background reads leaving a slot free.
			reads := tc.size / tc.opts.maxReadBytes
			parallel := uint64(min(max(tc.opts.channels, 1), tc.opts.maxParallel-1))
			rounds := (reads + parallel - 1) / parallel
			ideal := time.Duration(rounds)*tc.opts.transferTime(tc.opts.maxReadBytes) + tc.opts.latency
			if parallel == 1 {
				ideal = tc.opts.transferTime(tc.size) + tc.opts.latency
			}
			t.Logf("loading took %v, ideally %v", took, ideal)
			if took > ideal*11/10 {
				t.Errorf("loading took %v, want at most 10%% more than %v", took, ideal)
			}
		})
	}
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
	// fails. (Background reads leave one of the three slots free.)
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: hostarch.PageSize,
		maxParallel:  3,
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

// TestAsyncLoadWaitMetrics checks that loading reports the waits for pages
// that it served in its metrics.
func TestAsyncLoadWaitMetrics(t *testing.T) {
	fr, gens, img := loaderTestImage(t, 1<<20)
	opts := testDisk
	opts.manual = true
	r := newTestPagesFile(t, img.pages, opts)
	r.useClock()
	waits, waitBytes, waitNanoseconds := asyncLoadWaits.Value(), asyncLoadWaitBytes.Value(), asyncLoadWaitNanoseconds.Value()
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })

	r.waitPending()
	last := memmap.FileRange{fr.End - hostarch.PageSize, fr.End}
	faultAt := time.Duration(r.nanotime())
	done := awaitAsync(restored, last)
	waitForWaiters(t, l.apfl, 1)
	r.finish()
	if err := receive(t, done, "the awaited page"); err != nil {
		t.Fatalf("MapInternal(%v): %v", last, err)
	}
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	awaited, _ := r.readAt(int64(last.Start - fr.Start))
	if got := asyncLoadWaits.Value() - waits; got != 1 {
		t.Errorf("waits = %d, want 1", got)
	}
	if got := asyncLoadWaitBytes.Value() - waitBytes; got != hostarch.PageSize {
		t.Errorf("bytes waited for = %d, want %d", got, hostarch.PageSize)
	}
	if got, want := time.Duration(asyncLoadWaitNanoseconds.Value()-waitNanoseconds), awaited.completed-faultAt; got != want {
		t.Errorf("time waited = %v, want %v", got, want)
	}
	gens.checkPages(t, restored, fr)
}
