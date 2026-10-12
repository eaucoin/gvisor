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
