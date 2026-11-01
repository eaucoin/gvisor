// Copyright 2024 The gVisor Authors.
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
	"strings"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// If logAwaitedLoads is true, events relevant to awaited async page loads, and
// the first touches of a working set being recorded, are logged at level Info.
// runsc sets it from --async-awaits-trace=log, before any MemoryFile loads.
var logAwaitedLoads atomicbitops.Bool

// SetLogAwaitedLoads sets whether events relevant to awaited async page loads,
// and the first touches of a working set being recorded, are logged.
func SetLogAwaitedLoads(enabled bool) {
	logAwaitedLoads.Store(enabled)
}

// LoggingAwaitedLoads returns true if events relevant to awaited async page
// loads are logged, so that their users can log what they await.
func LoggingAwaitedLoads() bool {
	return logAwaitedLoads.Load()
}

// If logDirtyAttribution is true, each swap of a MemoryFile's dirty set logs
// what the swapped pages hold, by memory kind, and which paths marked them
// since the last swap; and the MemoryManagers count the bytes that writes to
// their vmas disarm. runsc sets it from --dirty-trace=log, before any
// MemoryFile is tracked.
var logDirtyAttribution atomicbitops.Bool

// SetLogDirtyAttribution sets whether dirty pages are attributed and logged.
func SetLogDirtyAttribution(enabled bool) {
	logDirtyAttribution.Store(enabled)
}

// LoggingDirtyAttribution returns true if dirty pages are attributed and
// logged, so that MemoryManagers count what their tasks dirty.
func LoggingDirtyAttribution() bool {
	return logDirtyAttribution.Load()
}

// dirtyMarkPathNames names the DirtyMarkPaths in attribution logs.
var dirtyMarkPathNames = [dirtyMarkPaths]string{
	DirtyMarkNone:              "unattributed",
	DirtyMarkMapInternal:       "map-internal",
	DirtyMarkDecommit:          "decommit",
	DirtyMarkTmpfsWrite:        "tmpfs-write",
	DirtyMarkIOUring:           "io-uring",
	DirtyMarkWriteProtectFault: "wp-fault",
	DirtyMarkWriteProtectArm:   "wp-arm",
	DirtyMarkUffdInternal:      "uffd-internal",
	DirtyMarkUffdUnmap:         "uffd-unmap",
}

// memoryKindName names a usage.MemoryKind in attribution logs.
func memoryKindName(k usage.MemoryKind) string {
	switch k {
	case usage.System:
		return "system"
	case usage.Anonymous:
		return "anonymous"
	case usage.PageCache:
		return "page-cache"
	case usage.Tmpfs:
		return "tmpfs"
	case usage.Ramdiskfs:
		return "ramdiskfs"
	case usage.Mapped:
		return "mapped"
	default:
		return fmt.Sprintf("kind-%d", int(k))
	}
}

// logDirtyAttribution logs s, the dirty set f just swapped, by the memory kind
// of its pages, and the bytes each path marked since the previous swap, which
// it resets.
func (f *MemoryFile) logDirtyAttribution(s *DirtySet) {
	var kinds [usage.Mapped + 1]uint64
	var waste, uncommitted uint64
	f.mu.Lock()
	var accounted uint64
	for maseg := f.memAcct.FirstSegment(); maseg.Ok(); maseg = maseg.NextSegment() {
		var n uint64
		s.ForEachRange(maseg.Range(), func(fr memmap.FileRange) bool {
			n += fr.Length()
			return true
		})
		accounted += n
		ma := maseg.ValuePtr()
		switch {
		case ma.wasteOrReleasing:
			waste += n
		case int(ma.kind) < len(kinds):
			kinds[ma.kind] += n
		}
	}
	f.mu.Unlock()
	total := s.Bytes()
	uncommitted = total - accounted
	var b strings.Builder
	fmt.Fprintf(&b, "Dirty attribution: MemoryFile %p swapped %d dirty bytes; by kind:", f, total)
	for k, n := range kinds {
		if n != 0 {
			fmt.Fprintf(&b, " %s=%d", memoryKindName(usage.MemoryKind(k)), n)
		}
	}
	fmt.Fprintf(&b, " waste-or-releasing=%d uncommitted=%d; newly marked since the last swap, by path:", waste, uncommitted)
	for p := range f.dirty.marked {
		if n := f.dirty.marked[p].Swap(0); n != 0 {
			fmt.Fprintf(&b, " %s=%d", dirtyMarkPathNames[p], n)
		}
	}
	log.Infof("%s", b.String())
}
