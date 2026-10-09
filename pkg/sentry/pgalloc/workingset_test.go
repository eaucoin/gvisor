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
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// workingSetOf returns the working set with the given unit and extents.
func workingSetOf(unit uint64, extents ...memmap.FileRange) *pgallocpb.WorkingSetProto {
	ws := &pgallocpb.WorkingSetProto{Version: checkpointimage.WorkingSetVersion, Unit: unit}
	for _, e := range extents {
		ws.Extents = append(ws.Extents, &pgallocpb.FileRangeProto{Start: e.Start, End: e.End})
	}
	return ws
}

// extentsOf returns the extents of ws.
func extentsOf(ws *pgallocpb.WorkingSetProto) []memmap.FileRange {
	var extents []memmap.FileRange
	for _, e := range ws.GetExtents() {
		extents = append(extents, memmap.FileRange{Start: e.GetStart(), End: e.GetEnd()})
	}
	return extents
}

const kib = 1 << 10

// TestWorkingSetRecording checks that a working set holds the units touched,
// in first-touch order, once each, merged when adjacent in that order.
func TestWorkingSetRecording(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	f.StartWorkingSetRecording(64*kib, time.Hour)
	if got := f.WorkingSetUnit(); got != 64*kib {
		t.Errorf("WorkingSetUnit() while recording = %d, want %d", got, 64*kib)
	}
	for _, fr := range []memmap.FileRange{
		{128 * kib, 132 * kib},
		{0, 4 * kib},
		{64 * kib, 68 * kib},   // adjacent to the previous unit
		{130 * kib, 134 * kib}, // touched already
		{1024 * kib, 1160 * kib},
	} {
		f.RecordTouch(fr)
	}
	f.StopWorkingSetRecording()
	if got := f.WorkingSetUnit(); got != 0 {
		t.Errorf("WorkingSetUnit() after recording = %d, want 0", got)
	}
	f.RecordTouch(memmap.FileRange{2048 * kib, 2052 * kib})

	ws := f.WorkingSet()
	want := []memmap.FileRange{{128 * kib, 192 * kib}, {0, 128 * kib}, {1024 * kib, 1216 * kib}}
	if got := extentsOf(ws); !equalRanges(got, want) {
		t.Errorf("working set extents = %v, want %v", got, want)
	}
	if ws.GetUnit() != 64*kib || ws.GetVersion() != checkpointimage.WorkingSetVersion || ws.GetWindowNs() == 0 {
		t.Errorf("working set unit %d, version %d, window %d ns; want %d, %d, non-zero", ws.GetUnit(), ws.GetVersion(), ws.GetWindowNs(), 64*kib, checkpointimage.WorkingSetVersion)
	}
}

func equalRanges(a, b []memmap.FileRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWorkingSetWindow checks that recording stops at the end of its window,
// and that a later recording is not stopped by an earlier one's window.
func TestWorkingSetWindow(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	f.StartWorkingSetRecording(hostarch.PageSize, 10*time.Millisecond)
	f.RecordTouch(memmap.FileRange{0, hostarch.PageSize})
	deadline := time.Now().Add(testWaitTimeout)
	for f.WorkingSetUnit() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("recording continues %v after its window of 10ms", testWaitTimeout)
		}
		time.Sleep(time.Millisecond)
	}
	if got, want := extentsOf(f.WorkingSet()), []memmap.FileRange{{0, hostarch.PageSize}}; !equalRanges(got, want) {
		t.Errorf("working set extents = %v, want %v", got, want)
	}

	f.StartWorkingSetRecording(hostarch.PageSize, 50*time.Millisecond)
	f.StopWorkingSetRecording()
	f.StartWorkingSetRecording(hostarch.PageSize, time.Hour)
	time.Sleep(100 * time.Millisecond)
	if f.WorkingSetUnit() == 0 {
		t.Errorf("the window of a stopped recording stopped the next one")
	}
	f.StopWorkingSetRecording()
}

// TestWorkingSetInheritance checks that a MemoryFile keeps the working set of
// the image it was restored from until a recording replaces it with a
// non-empty set.
func TestWorkingSetInheritance(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	restored := workingSetOf(64*kib, memmap.FileRange{0, 64 * kib})
	f.SetWorkingSet(restored)
	if got := f.WorkingSet(); got != restored {
		t.Fatalf("WorkingSet() = %v, want the restored image's %v", got, restored)
	}
	f.StartWorkingSetRecording(64*kib, time.Hour)
	f.StopWorkingSetRecording()
	if got := f.WorkingSet(); got != restored {
		t.Errorf("WorkingSet() after recording nothing = %v, want the restored image's %v", got, restored)
	}
	f.StartWorkingSetRecording(64*kib, time.Hour)
	f.RecordTouch(memmap.FileRange{128 * kib, 132 * kib})
	f.StopWorkingSetRecording()
	if got, want := extentsOf(f.WorkingSet()), []memmap.FileRange{{128 * kib, 192 * kib}}; !equalRanges(got, want) {
		t.Errorf("working set extents = %v, want the recorded %v", got, want)
	}
}

// TestWorkingSetCoverage checks the measure of how much of what was touched
// the restored image's working set held, and how much of it was not touched.
func TestWorkingSetCoverage(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	f.SetWorkingSet(workingSetOf(4*kib,
		memmap.FileRange{0, 8 * kib},
		memmap.FileRange{256 * kib, 260 * kib},
		memmap.FileRange{4 * kib, 8 * kib}, // the same 64 KiB unit again
	))
	f.StartWorkingSetRecording(64*kib, time.Hour)
	f.RecordTouch(memmap.FileRange{0, 4 * kib})
	f.RecordTouch(memmap.FileRange{512 * kib, 516 * kib})
	f.ws.mu.Lock()
	touched, hit, unused := f.ws.coverageLocked()
	f.ws.mu.Unlock()
	f.StopWorkingSetRecording()
	if touched != 128*kib || hit != 64*kib || unused != 64*kib {
		t.Errorf("coverage: %d bytes touched, %d held by the set, %d of the set not touched; want %d, %d, %d", touched, hit, unused, 128*kib, 64*kib, 64*kib)
	}
}

// TestWorkingSetMapInternal checks that the Sentry's own accesses to pages
// are touches.
func TestWorkingSetMapInternal(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocate(t, f, 64*kib, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(fr) })
	f.StartWorkingSetRecording(hostarch.PageSize, time.Hour)
	touched := memmap.FileRange{fr.Start + 8*kib, fr.Start + 16*kib}
	if _, err := f.MapInternal(touched, hostarch.Read); err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	f.StopWorkingSetRecording()
	if got, want := extentsOf(f.WorkingSet()), []memmap.FileRange{touched}; !equalRanges(got, want) {
		t.Errorf("working set extents = %v, want %v", got, want)
	}
}

// testSlowDisk is a disk that loads a 16 MiB image in 1.6 s, long enough for
// a working set to be read first.
var testSlowDisk = testPagesFileOpts{
	maxReadBytes: 256 << 10,
	maxParallel:  128,
	bandwidth:    10 << 20,
	latency:      100 * time.Microsecond,
}

// TestAsyncLoadPrefetch checks the order in which a background restore reads
// a MemoryFile whose image holds a working set: the set first, in its order,
// then the rest of the pages file, unless prefetching is off or the pages file
// loads whole too quickly for it to matter.
func TestAsyncLoadPrefetch(t *testing.T) {
	const size = 16 << 20
	for _, tc := range []struct {
		name     string
		opts     testPagesFileOpts
		prefetch PrefetchPolicy
		// first is true if the working set must be read before the pages
		// that precede it in the pages file.
		first bool
	}{
		{name: "auto", opts: testSlowDisk, prefetch: PrefetchAuto, first: true},
		{name: "off", opts: testSlowDisk, prefetch: PrefetchOff},
		{name: "auto on a fast disk", opts: testFastDisk, prefetch: PrefetchAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, gens, img := loaderTestImage(t, size)
			// The working set: 64 KiB at 15 MiB, then 64 KiB at 12 MiB.
			set := []memmap.FileRange{
				{fr.Start + 15<<20, fr.Start + 15<<20 + 64*kib},
				{fr.Start + 12<<20, fr.Start + 12<<20 + 64*kib},
			}
			img.img.Proto = proto.Clone(img.img.Proto).(*pgallocpb.ImageProto)
			img.img.Proto.WorkingSet = workingSetOf(64*kib, set...)
			r := newTestPagesFile(t, img.pages, tc.opts)
			r.useClock()
			restored := newTestMemoryFile(t, testMemoryFileOpts{})
			l := startLoadWithPrefetch(t, img, r, tc.prefetch, restored)
			t.Cleanup(func() { releaseAll(t, restored) })
			if err := l.wait(t); err != nil {
				t.Fatalf("async page loading failed: %v", err)
			}
			gens.checkPages(t, restored, fr)
			if got := restored.WorkingSet(); !proto.Equal(got, img.img.Proto.WorkingSet) {
				t.Errorf("restored MemoryFile's working set = %v, want the image's %v", got, img.img.Proto.WorkingSet)
			}

			read := func(off uint64) testRead {
				rd, ok := r.readAt(int64(off - fr.Start))
				if !ok {
					t.Fatalf("no read of MemoryFile offset %#x", off)
				}
				return rd
			}
			ws0, ws1 := read(set[0].Start), read(set[1].Start)
			before := read(fr.Start + 4<<20)
			if tc.first {
				if ws0.submitted > ws1.submitted || ws1.submitted > before.submitted {
					t.Errorf("reads submitted at %v (working set, at 15 MiB), %v (working set, at 12 MiB), %v (at 4 MiB): want the working set first, in its order", ws0.submitted, ws1.submitted, before.submitted)
				}
			} else if ws0.submitted < before.submitted || ws1.submitted < before.submitted {
				t.Errorf("reads submitted at %v (working set, at 15 MiB), %v (working set, at 12 MiB), %v (at 4 MiB): want pages file order", ws0.submitted, ws1.submitted, before.submitted)
			}
		})
	}
}

// TestAsyncLoadPrefetchParallelPagesFile checks that loading does not read
// the working set first from an object store that delivers all pages within
// aplPrefetchMinLoadTime with the reads it serves in parallel, although its
// first read, alone, would take longer, and that loading then takes no
// longer than without the set.
func TestAsyncLoadPrefetchParallelPagesFile(t *testing.T) {
	const size = 64 << 20
	took := make(map[PrefetchPolicy]time.Duration)
	for _, prefetch := range []PrefetchPolicy{PrefetchAuto, PrefetchOff} {
		fr, gens, img := loaderTestImage(t, size)
		// A working set of 64 ranges of 64 KiB, one per MiB from the end
		// of the pages file to its start.
		var set []memmap.FileRange
		for off := uint64(size - 1<<20); ; off -= 1 << 20 {
			set = append(set, memmap.FileRange{fr.Start + off, fr.Start + off + 64*kib})
			if off == 0 {
				break
			}
		}
		img.img.Proto = proto.Clone(img.img.Proto).(*pgallocpb.ImageProto)
		img.img.Proto.WorkingSet = workingSetOf(64*kib, set...)
		r := newTestPagesFile(t, img.pages, testContendedObjectStore)
		r.useClock()
		restored := newTestMemoryFile(t, testMemoryFileOpts{})
		l := startLoadWithPrefetch(t, img, r, prefetch, restored)
		if err := l.wait(t); err != nil {
			t.Fatalf("async page loading failed: %v", err)
		}
		gens.checkPages(t, restored, fr)
		releaseAll(t, restored)
		if prefetch == PrefetchAuto && l.apfl.prefetched != 0 {
			t.Errorf("loading read %d bytes of the working set first; want none, since all pages load within %v", l.apfl.prefetched, aplPrefetchMinLoadTime)
		}
		for _, rd := range r.readsSnapshot() {
			took[prefetch] = max(took[prefetch], rd.completed)
		}
	}
	t.Logf("loading took %v with the working set first if worth it, %v without", took[PrefetchAuto], took[PrefetchOff])
	if took[PrefetchAuto] > took[PrefetchOff] {
		t.Errorf("loading took %v with the working set first if worth it, longer than %v without", took[PrefetchAuto], took[PrefetchOff])
	}
}

// TestAsyncLoadPrefetchFaultWait checks that reading a working set first does
// not delay a page fault: its read is submitted at once, ahead of the reads
// of the set that remain.
func TestAsyncLoadPrefetchFaultWait(t *testing.T) {
	const size = 16 << 20
	fr, _, img := loaderTestImage(t, size)
	// A working set of 4 MiB in the middle of the pages file.
	img.img.Proto = proto.Clone(img.img.Proto).(*pgallocpb.ImageProto)
	img.img.Proto.WorkingSet = workingSetOf(64*kib, memmap.FileRange{fr.Start + 6<<20, fr.Start + 10<<20})
	opts := testSlowDisk
	opts.manual = true
	r := newTestPagesFile(t, img.pages, opts)
	r.useClock()
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoadWithPrefetch(t, img, r, PrefetchAuto, restored)
	t.Cleanup(func() { releaseAll(t, restored) })

	// Run until the loader has read the first read of the set.
	setOff := int64(6 << 20)
	for {
		r.advanceToNext()
		if _, ok := r.readAt(setOff); ok {
			break
		}
	}
	r.waitPending()
	last := memmap.FileRange{fr.End - hostarch.PageSize, fr.End}
	faultAt := time.Duration(r.nanotime())
	done := awaitAsync(restored, last)
	waitForWaiters(t, l.apfl, 1)
	lastOff := int64(last.Start - fr.Start)
	for rd := r.advanceToNext(); rd.off != lastOff; rd = r.advanceToNext() {
	}
	if err := receive(t, done, "the awaited page"); err != nil {
		t.Fatalf("MapInternal(%v): %v", last, err)
	}
	awaited, _ := r.readAt(lastOff)
	if awaited.submitted != faultAt {
		t.Errorf("awaited read submitted at %v, want at once, at the fault's %v", awaited.submitted, faultAt)
	}
	r.finish()
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading failed: %v", err)
	}
	if setEnd, ok := r.readAt(10<<20 - hostarch.PageSize); !ok || setEnd.submitted < awaited.submitted {
		t.Errorf("the end of the working set was read at %v, before the fault's page at %v", setEnd.submitted, awaited.submitted)
	}
}
