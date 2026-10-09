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

package mm

import (
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sync"
)

// Dirty tracking by write-protection.
//
// Application stores through a platform.AddressSpace mapping of a
// pgalloc.MemoryFile are invisible to the MemoryFile's dirty tracking. The
// MemoryManager makes them visible as mm.Fork makes copy-on-write breaks
// visible: ArmDirtyTracking withholds write permission from every pma that
// maps a dirty-tracked MemoryFile ("arms" it) and unmaps the pmas that were
// writable from the AddressSpace, so that the next write to each, by the
// application or by the Sentry on its behalf (e.g. CopyOut), reaches
// getPMAsLocked. There, the write marks the pages it writes dirty and restores
// write permission to them ("disarms" them), and the write proceeds.
//
// Disarming works on units: the pma is split around the units, aligned to the
// unit size, that the write overlaps, and only these are marked and made
// writable. Larger units make fewer faults and record more pages; the
// difference can be refined by comparing page contents when saving.

// DefaultDirtyTrackingUnit is the unit of disarming before ArmDirtyTracking
// sets one: 64 KiB, at which a first write costs about a sixth of a 4 KiB
// unit's per page on systrap, and which hash refinement of saved pages keeps
// as precise as a 4 KiB unit.
const DefaultDirtyTrackingUnit = 64 << 10

// ArmDirtyTracking arms every pma of mm that maps a dirty-tracked
// pgalloc.MemoryFile, so that the next write to each unit of unit bytes is
// recorded in the MemoryFile's dirty set. Huge pmas are disarmed in huge
// pages.
//
// Preconditions: unit is a power of 2 and at least hostarch.PageSize.
func (mm *MemoryManager) ArmDirtyTracking(unit uint64) {
	if unit < hostarch.PageSize || unit&(unit-1) != 0 {
		panic(fmt.Sprintf("invalid dirty tracking unit %d", unit))
	}
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	mm.dirtyUnit = hostarch.Addr(unit)
	if !pgalloc.DirtyMarkPathEnabled(pgalloc.DirtyMarkWriteProtectArm) {
		return
	}

	// Unmap the pmas that were writable, with one call per vma (spanning the
	// non-writable pmas between them) rather than one per pma: after an epoch,
	// written units are separate pmas, and unmapping each costs a host
	// syscall on systrap and a TLB invalidation on KVM.
	var (
		unmapAR hostarch.AddrRange
		vseg    = mm.vmas.FirstSegment()
	)
	for pseg := mm.pmas.FirstSegment(); pseg.Ok(); pseg = pseg.NextSegment() {
		pma := pseg.ValuePtr()
		if pma.dirtyArmed || !pma.dirtyTracked() {
			continue
		}
		wasWritable := pma.effectivePerms.Write
		pma.armDirty()
		if !wasWritable {
			continue
		}
		if unmapAR.Length() != 0 && pseg.Start() >= vseg.End() {
			mm.unmapASLocked(unmapAR)
			unmapAR = hostarch.AddrRange{}
		}
		vseg = vseg.seekNextLowerBound(pseg.Start())
		unmapAR = joinAddrRanges(unmapAR, pseg.Range())
	}
	mm.unmapASLocked(unmapAR)
	// Merge the units disarmed during the previous epoch back into their
	// neighbors.
	mm.pmas.MergeAll()
}

// dirtyTracked returns true if pma maps a dirty-tracked pgalloc.MemoryFile.
func (pma *pma) dirtyTracked() bool {
	mf, ok := pma.file.(*pgalloc.MemoryFile)
	return ok && mf.DirtyTracked()
}

// armDirty arms pma.
//
// Preconditions: pma.dirtyTracked().
func (pma *pma) armDirty() {
	pma.dirtyArmed = true
	pma.effectivePerms.Write = false
	pma.maxPerms.Write = false
}

// armDirtyIfTracked arms pma, a new pma, if it maps a dirty-tracked
// pgalloc.MemoryFile.
func (pma *pma) armDirtyIfTracked() {
	if pma.dirtyTracked() {
		pma.armDirty()
	}
}

// disarmDirtyLocked handles a write to the addresses ar of the armed pma
// pseg, which lies in the vma vseg: it isolates the units of pseg that ar
// overlaps, marks them dirty in the pma's MemoryFile, and restores their
// write permission. It returns the iterator to the isolated pma.
//
// Preconditions:
//   - mm.activeMu must be locked for writing.
//   - pseg.ValuePtr().dirtyArmed.
//   - vseg.Range().IsSupersetOf(pseg.Range().Intersect(ar)).
func (mm *MemoryManager) disarmDirtyLocked(vseg vmaIterator, pseg pmaIterator, ar hostarch.AddrRange) pmaIterator {
	unit := mm.dirtyUnit
	if pseg.ValuePtr().huge {
		unit = hostarch.HugePageSize
	} else if unit == 0 {
		unit = DefaultDirtyTrackingUnit
	}
	ar = ar.Intersect(pseg.Range())
	unitAR := hostarch.AddrRange{ar.Start &^ (unit - 1), ar.End}
	if end := (ar.End + unit - 1) &^ (unit - 1); end > ar.End {
		unitAR.End = end
	}
	unitAR = unitAR.Intersect(pseg.Range()).Intersect(vseg.Range())
	if unitAR != pseg.Range() {
		pseg = mm.pmas.Isolate(pseg, unitAR)
	}
	pma := pseg.ValuePtr()
	vma := vseg.ValuePtr()
	pma.dirtyArmed = false
	pma.effectivePerms = vma.effectivePerms.Intersect(pma.translatePerms)
	pma.maxPerms = vma.maxPerms.Intersect(pma.translatePerms)
	if pma.needCOW {
		pma.effectivePerms.Write = false
		pma.maxPerms.Write = false
	}
	// The mark precedes the write, which is made possible only after
	// getPMAsLocked returns.
	pma.file.(*pgalloc.MemoryFile).MarkDirtyBy(pgalloc.DirtyMarkWriteProtectFault, pseg.fileRange())
	mm.dirtyThrottle.dirtied.Add(uint64(pseg.Range().Length()))
	return pseg
}

// Dirty rate limiting.
//
// While a checkpoint pre-copies memory, a task that dirties memory faster than
// the checkpoint can write it keeps the pre-copy from converging. As QEMU's
// dirty-limit does for vCPUs, the kernel can then limit the rate at which each
// MemoryManager's tasks dirty memory, delaying a task after the faults that
// disarm units (DirtyThrottleDelay); tasks that write little are not delayed.

// dirtyThrottleBurst is the time's worth of the limit that a MemoryManager may
// dirty at once.
const dirtyThrottleBurst = 100 * time.Millisecond

// dirtyThrottle is the rate limiting state of a MemoryManager: a token bucket
// of credit in bytes.
type dirtyThrottle struct {
	// dirtied is the number of bytes disarmed since the last call to
	// DirtyThrottleDelay.
	dirtied atomicbitops.Uint64

	mu sync.Mutex

	// credit is the number of bytes that may be dirtied without delay; it is
	// negative when the MemoryManager is over its limit.
	//
	// +checklocks:mu
	credit int64

	// last is the time of the last call to DirtyThrottleDelay, in
	// nanoseconds of the caller's monotonic clock.
	//
	// +checklocks:mu
	last int64

	// session identifies the limiting period of the last call to
	// DirtyThrottleDelay.
	//
	// +checklocks:mu
	session uint64
}

// DirtyThrottleDelay charges the bytes that mm's tasks dirtied since its last
// call against a limit of limit bytes per second, and returns how long the
// calling task should wait for mm to be back within it. now is the current
// time, in nanoseconds of a monotonic clock. session identifies the period
// during which the limit applies: the first call of a new session starts with
// full credit, and does not charge what was dirtied before.
//
// Preconditions: limit > 0.
func (mm *MemoryManager) DirtyThrottleDelay(limit, session uint64, now int64) time.Duration {
	t := &mm.dirtyThrottle
	t.mu.Lock()
	defer t.mu.Unlock()
	dirtied := int64(t.dirtied.Swap(0))
	burst := int64(limit) * int64(dirtyThrottleBurst) / int64(time.Second)
	if t.session != session {
		t.session = session
		t.credit = burst
		dirtied = 0
	} else if elapsed := now - t.last; elapsed > 0 {
		// Credit refills at limit, up to burst.
		refill := float64(limit) * time.Duration(elapsed).Seconds()
		t.credit = int64(min(float64(t.credit)+refill, float64(burst)))
	}
	t.last = now
	t.credit -= dirtied
	if t.credit >= 0 {
		return 0
	}
	return time.Duration(float64(-t.credit) / float64(limit) * float64(time.Second))
}
