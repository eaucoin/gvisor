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

package pgalloc

import (
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/metric"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sync"
)

// After a restore, a MemoryFile can record the pages that its users touch
// first, in the order that they touch them: its working set. A checkpoint
// saves the latest working set in the image (ImageProto.working_set), and a
// background restore of that image reads it before the rest of the pages
// file (see asyncMemoryFileLoad.prefetch), as REAP does for snapshots of
// functions (Ustiugov et al., ASPLOS 2021). MemoryFile offsets are saved
// state, so the set recorded after restoring an image is what a restore of the
// next image that the sandbox saves touches first.
//
// Touches are mappings of pages into application address spaces (mm calls
// RecordTouch), which see every page that an application touches after a
// restore since its address spaces start empty, and the Sentry's own accesses
// through MapInternal.

// maxWorkingSetExtents bounds the number of extents of a recorded working set,
// and thus the size of ImageProto.working_set: an application that touches
// more scattered memory than this during the recording window ends recording
// early.
const maxWorkingSetExtents = 1 << 16

// Metrics of working sets, so that their value is observable per deployment:
// REAP's hit rate is hit / (hit + miss), and unused bytes were prefetched in
// vain. They are updated when recording ends.
var (
	workingSetHitBytes = metric.MustCreateNewUint64Metric("/checkpoint/working_set_hit_bytes", metric.Uint64Metadata{
		Cumulative:  true,
		Description: "Bytes of memory touched after a restore that the restored image's working set held.",
	})
	workingSetMissBytes = metric.MustCreateNewUint64Metric("/checkpoint/working_set_miss_bytes", metric.Uint64Metadata{
		Cumulative:  true,
		Description: "Bytes of memory touched after a restore that the restored image's working set did not hold.",
	})
	workingSetUnusedBytes = metric.MustCreateNewUint64Metric("/checkpoint/working_set_unused_bytes", metric.Uint64Metadata{
		Cumulative:  true,
		Description: "Bytes of the restored image's working set that were not touched after the restore.",
	})
)

// workingSet records and holds the working set of a MemoryFile.
type workingSet struct {
	// recording is true while touches are recorded.
	recording atomicbitops.Bool

	// mu protects the following fields.
	mu sync.Mutex

	// unit is the recording granularity in bytes, a power of 2 multiple of
	// the page size. start is when recording started, and stop stops it at
	// the end of its window. gen counts recordings, so that the stop of an
	// earlier recording does not stop a later one.
	unit  uint64
	start time.Time
	stop  *time.Timer
	gen   uint64

	// seen has a bit set for each unit of the MemoryFile that was touched.
	seen []uint64

	// extents are the units touched, in first-touch order, merged when
	// adjacent.
	extents []memmap.FileRange

	// set is the latest working set: recorded after the last restore, or, if
	// none was, carried by the image restored.
	set *pgallocpb.WorkingSetProto

	// restored is the working set of the image restored, against which
	// recording measures its hits.
	restored *pgallocpb.WorkingSetProto
}

// SetWorkingSet sets f's working set to the one carried by the image that f is
// restored from, which is ws if ws is not nil.
func (f *MemoryFile) SetWorkingSet(ws *pgallocpb.WorkingSetProto) {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	f.ws.set = ws
	f.ws.restored = ws
}

// WorkingSet returns f's working set, or nil if it has none.
func (f *MemoryFile) WorkingSet() *pgallocpb.WorkingSetProto {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return f.ws.set
}

// StartWorkingSetRecording starts recording the units of unit bytes of f that
// are touched, for window or until StopWorkingSetRecording is called. unit
// must be a power of 2 multiple of the page size.
func (f *MemoryFile) StartWorkingSetRecording(unit uint64, window time.Duration) {
	if unit < hostarch.PageSize || unit&(unit-1) != 0 {
		panic(fmt.Sprintf("invalid working set unit %d", unit))
	}
	ws := &f.ws
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.recording.Load() {
		ws.stopLocked()
	}
	ws.unit = unit
	ws.start = time.Now()
	ws.seen = nil
	ws.extents = nil
	ws.gen++
	gen := ws.gen
	ws.recording.Store(true)
	ws.stop = time.AfterFunc(window, func() {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		if ws.recording.Load() && ws.gen == gen {
			ws.stopLocked()
		}
	})
	log.Infof("MemoryFile(%p): recording the working set in units of %d bytes for %v", f, unit, window)
}

// StopWorkingSetRecording stops recording f's working set, if it is being
// recorded. The set recorded, unless it is empty, becomes f's working set.
func (f *MemoryFile) StopWorkingSetRecording() {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	if f.ws.recording.Load() {
		f.ws.stopLocked()
	}
}

// WorkingSetUnit returns the recording granularity of f's working set while
// it is recorded, and 0 otherwise. Users of f that map its pages in units of
// more than this would record more than was touched.
func (f *MemoryFile) WorkingSetUnit() uint64 {
	if !f.ws.recording.Load() {
		return 0
	}
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	if !f.ws.recording.Load() {
		return 0
	}
	return f.ws.unit
}

// RecordTouch records that fr was touched, if f's working set is being
// recorded.
func (f *MemoryFile) RecordTouch(fr memmap.FileRange) {
	if !f.ws.recording.Load() {
		return
	}
	ws := &f.ws
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.recording.Load() {
		return
	}
	for u := fr.Start / ws.unit; u < (fr.End+ws.unit-1)/ws.unit; u++ {
		word, bit := u/64, uint64(1)<<(u%64)
		if word >= uint64(len(ws.seen)) {
			ws.seen = append(ws.seen, make([]uint64, word+1-uint64(len(ws.seen)))...)
		}
		if ws.seen[word]&bit != 0 {
			continue
		}
		ws.seen[word] |= bit
		start := u * ws.unit
		if n := len(ws.extents); n != 0 && ws.extents[n-1].End == start {
			ws.extents[n-1].End += ws.unit
			continue
		}
		if len(ws.extents) == maxWorkingSetExtents {
			log.Infof("MemoryFile(%p): the working set reached %d extents after %v", f, maxWorkingSetExtents, time.Since(ws.start))
			ws.stopLocked()
			return
		}
		ws.extents = append(ws.extents, memmap.FileRange{Start: start, End: start + ws.unit})
	}
}

// touched returns true if unit u was touched.
//
// Preconditions: ws.mu must be locked.
func (ws *workingSet) touched(u uint64) bool {
	word := u / 64
	return word < uint64(len(ws.seen)) && ws.seen[word]&(1<<(u%64)) != 0
}

// stopLocked stops recording.
//
// Preconditions:
//   - ws.mu must be locked.
//   - ws.recording.Load() == true.
func (ws *workingSet) stopLocked() {
	ws.recording.Store(false)
	ws.stop.Stop()
	window := time.Since(ws.start)
	touched, hit, unused := ws.coverageLocked()
	if ws.restored != nil {
		workingSetHitBytes.IncrementBy(hit)
		workingSetMissBytes.IncrementBy(touched - hit)
		workingSetUnusedBytes.IncrementBy(unused)
		log.Infof("Working set: %d bytes in %d extents touched in %v; the restored image's set held %d of them and %d bytes not touched", touched, len(ws.extents), window.Round(time.Millisecond), hit, unused)
	} else {
		log.Infof("Working set: %d bytes in %d extents touched in %v", touched, len(ws.extents), window.Round(time.Millisecond))
	}
	if len(ws.extents) != 0 {
		set := &pgallocpb.WorkingSetProto{
			Version:  checkpointimage.WorkingSetVersion,
			Unit:     ws.unit,
			WindowNs: uint64(window.Nanoseconds()),
			Extents:  make([]*pgallocpb.FileRangeProto, len(ws.extents)),
		}
		for i, e := range ws.extents {
			set.Extents[i] = &pgallocpb.FileRangeProto{Start: e.Start, End: e.End}
		}
		ws.set = set
	}
	ws.restored = nil
	ws.seen = nil
	ws.extents = nil
}

// coverageLocked returns the number of bytes touched, of those that the
// restored image's working set held, and of that set's that were not touched,
// in units of the recording.
//
// Preconditions: ws.mu must be locked.
func (ws *workingSet) coverageLocked() (touched, hit, unused uint64) {
	for _, e := range ws.extents {
		touched += e.Length()
	}
	counted := make(map[uint64]struct{})
	for _, e := range ws.restored.GetExtents() {
		for u := e.GetStart() / ws.unit; u < (e.GetEnd()+ws.unit-1)/ws.unit; u++ {
			if _, ok := counted[u]; ok {
				continue
			}
			counted[u] = struct{}{}
			if ws.touched(u) {
				hit += ws.unit
			} else {
				unused += ws.unit
			}
		}
	}
	return touched, hit, unused
}
