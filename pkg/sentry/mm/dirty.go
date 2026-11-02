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
	checkDirtyTrackingUnit(unit)
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	mm.armDirtyTrackingLocked(unit, 0, ^uint64(0))
}

// ArmDirtyTrackingInSteps arms mm as ArmDirtyTracking does, but while its
// tasks run: in steps, each of which holds mm's locks while it arms at most
// step bytes of pmas (or one huge page, if step is smaller), splitting larger
// pmas, so that a task of mm that faults waits for one step rather than for
// the whole arm. It returns the number of steps and the longest.
//
// Units that tasks write between steps are disarmed by their faults, as after
// any arm, and pmas that tasks create behind a step are armed as they are
// created if they map a dirty-tracked pgalloc.MemoryFile; completeness rests
// on the ArmDirtyTracking that follows with tasks paused, not on this one.
//
// Preconditions: unit is a power of 2 and at least hostarch.PageSize; step >
// 0.
func (mm *MemoryManager) ArmDirtyTrackingInSteps(unit, step uint64) (steps int, longest time.Duration) {
	checkDirtyTrackingUnit(unit)
	var addr hostarch.Addr
	for {
		start := time.Now()
		mm.mappingMu.RLock()
		mm.activeMu.Lock()
		next, more := mm.armDirtyTrackingLocked(unit, addr, step)
		mm.activeMu.Unlock()
		mm.mappingMu.RUnlock()
		steps++
		longest = max(longest, time.Since(start))
		if !more {
			return steps, longest
		}
		addr = next
	}
}

// checkDirtyTrackingUnit panics if unit is not a valid dirty tracking unit.
func checkDirtyTrackingUnit(unit uint64) {
	if unit < hostarch.PageSize || unit&(unit-1) != 0 {
		panic(fmt.Sprintf("invalid dirty tracking unit %d", unit))
	}
}

// armDirtyTrackingLocked arms the pmas of mm from addr on, until it has armed
// limit bytes of them, splitting a pma that would take it beyond in whole huge
// pages, unless limit is the largest uint64. If pmas are left beyond, it
// returns the address of the first and true.
//
// Preconditions:
//   - mm.mappingMu must be locked for reading.
//   - mm.activeMu must be locked for writing.
//   - unit is a power of 2 and at least hostarch.PageSize.
func (mm *MemoryManager) armDirtyTrackingLocked(unit uint64, addr hostarch.Addr, limit uint64) (hostarch.Addr, bool) {
	mm.dirtyUnit = hostarch.Addr(unit)
	if !pgalloc.DirtyMarkPathEnabled(pgalloc.DirtyMarkWriteProtectArm) {
		return 0, false
	}

	// Unmap the pmas that were writable, with one call per vma (spanning the
	// non-writable pmas between them) rather than one per pma: after an epoch,
	// written units are separate pmas, and unmapping each costs a host
	// syscall on systrap and a TLB invalidation on KVM.
	var (
		unmapAR hostarch.AddrRange
		vseg    = mm.vmas.LowerBoundSegment(addr)
		pseg    = mm.pmas.LowerBoundSegment(addr)
		armed   uint64
	)
	for ; pseg.Ok() && armed < limit; pseg = pseg.NextSegment() {
		if pma := pseg.ValuePtr(); pma.dirtyArmed || !pma.dirtyTracked() {
			continue
		}
		if left := limit - armed; limit != ^uint64(0) && uint64(pseg.Range().Length()) > left {
			// Arm what is left of the step of a larger pma, in whole huge
			// pages, and the rest of it in the steps that follow.
			if n := max(hostarch.Addr(left)&^(hostarch.HugePageSize-1), hostarch.HugePageSize); n < pseg.Range().Length() {
				pseg = mm.pmas.Isolate(pseg, hostarch.AddrRange{pseg.Start(), pseg.Start() + n})
			}
		}
		pma := pseg.ValuePtr()
		wasWritable := pma.effectivePerms.Write
		pma.armDirty()
		armed += uint64(pseg.Range().Length())
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
	// neighbors, up to the pmas left.
	end := ^hostarch.Addr(0)
	if pseg.Ok() {
		end = pseg.Start()
	}
	mm.pmas.MergeInsideRange(hostarch.AddrRange{addr, end})
	return end, pseg.Ok()
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
	mm.dirtied.Add(uint64(pseg.Range().Length()))
	return pseg
}

// Dirty rate limiting.
//
// While a checkpoint pre-copies memory, tasks that dirty memory faster than
// the checkpoint can write it keep the pre-copy from converging. As QEMU's
// dirty-limit does for vCPUs, the kernel can then limit the rate at which its
// tasks dirty memory, all MemoryManagers together, delaying a task after the
// faults that disarm units (DirtyLimiter); tasks that write little are not
// delayed.

// dirtyLimitBurst is the time's worth of the limit that may be dirtied at
// once.
const dirtyLimitBurst = 100 * time.Millisecond

// TakeDirtied returns the number of bytes that mm's tasks disarmed since the
// last call, to charge against a dirtying limit during the limiting period
// session: none at mm's first call of a session, whose tasks are not charged
// for what they dirtied before it.
func (mm *MemoryManager) TakeDirtied(session uint64) uint64 {
	n := mm.dirtied.Swap(0)
	if mm.dirtiedSession.Swap(session) != session {
		return 0
	}
	return n
}

// DirtyLimiter limits the rate at which tasks dirty memory: a token bucket of
// credit in bytes, shared by the MemoryManagers it limits together.
type DirtyLimiter struct {
	mu sync.Mutex

	// credit is the number of bytes that may be dirtied without delay; it is
	// negative when the tasks are over their limit.
	//
	// +checklocks:mu
	credit int64

	// last is the time of the last call to Delay, in nanoseconds of the
	// caller's monotonic clock.
	//
	// +checklocks:mu
	last int64

	// session identifies the limiting period of the last call to Delay.
	//
	// +checklocks:mu
	session uint64
}

// Delay charges dirtied bytes, which a task's MemoryManager dirtied since it
// last charged them (TakeDirtied), against a limit of limit bytes per second,
// and returns how long the task should wait for the tasks to be back within
// it. now is the current time, in nanoseconds of a monotonic clock. session
// identifies the period during which the limit applies: the first call of a
// new session starts with full credit.
//
// Preconditions: limit > 0.
func (l *DirtyLimiter) Delay(dirtied, limit, session uint64, now int64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	burst := int64(limit) * int64(dirtyLimitBurst) / int64(time.Second)
	if l.session != session {
		l.session = session
		l.credit = burst
	} else if elapsed := now - l.last; elapsed > 0 {
		// Credit refills at limit, up to burst.
		refill := float64(limit) * time.Duration(elapsed).Seconds()
		l.credit = int64(min(float64(l.credit)+refill, float64(burst)))
	}
	l.last = now
	l.credit -= int64(dirtied)
	if l.credit >= 0 {
		return 0
	}
	return time.Duration(float64(-l.credit) / float64(limit) * float64(time.Second))
}
