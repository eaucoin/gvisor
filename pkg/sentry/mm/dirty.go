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

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
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
	pma.file.(*pgalloc.MemoryFile).MarkDirty(pseg.fileRange())
	return pseg
}
