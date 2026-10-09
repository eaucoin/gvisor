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
	"bytes"
	"fmt"
	"slices"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/usermem"
)

// recordingAddressSpace is a platform.AddressSpace that records the calls
// made to it and maps nothing.
type recordingAddressSpace struct {
	platform.AddressSpace

	// mapped records, for each page mapped and not unmapped since, the access
	// type it was mapped with.
	mapped map[hostarch.Addr]hostarch.AccessType

	// unmaps counts calls to Unmap.
	unmaps int
}

func newRecordingAddressSpace() *recordingAddressSpace {
	return &recordingAddressSpace{mapped: make(map[hostarch.Addr]hostarch.AccessType)}
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *recordingAddressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	for off := uint64(0); off < fr.Length(); off += hostarch.PageSize {
		as.mapped[addr+hostarch.Addr(off)] = at
	}
	return nil
}

// Unmap implements platform.AddressSpace.Unmap.
func (as *recordingAddressSpace) Unmap(addr hostarch.Addr, length uint64) {
	as.unmaps++
	for off := uint64(0); off < length; off += hostarch.PageSize {
		delete(as.mapped, addr+hostarch.Addr(off))
	}
}

// Release implements platform.AddressSpace.Release.
func (as *recordingAddressSpace) Release() {}

// PreFork implements platform.AddressSpace.PreFork.
func (as *recordingAddressSpace) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (as *recordingAddressSpace) PostFork() {}

// recordingPlatform is a platform.Platform whose AddressSpaces are
// recordingAddressSpaces, so that tests start no platform processes.
type recordingPlatform struct {
	platform.Platform
}

// NewAddressSpace implements platform.Platform.NewAddressSpace.
func (recordingPlatform) NewAddressSpace() (platform.AddressSpace, error) {
	return newRecordingAddressSpace(), nil
}

// dirtyTestMM returns a MemoryManager whose AddressSpace records mappings,
// over the MemoryFile of ctx, with dirty tracking enabled on it.
func dirtyTestMM(t *testing.T, ctx context.Context) (*MemoryManager, *recordingAddressSpace) {
	t.Helper()
	p := recordingPlatform{platform.FromContext(ctx)}
	mf := pgalloc.MemoryFileFromContext(ctx)
	mm, err := NewMemoryManager(p, mf)
	if err != nil {
		t.Fatalf("NewMemoryManager: %v", err)
	}
	t.Cleanup(func() { mm.DecUsers(ctx) })
	mm.layout = arch.MmapLayout{
		MinAddr:          p.MinUserAddress(),
		MaxAddr:          p.MaxUserAddress(),
		BottomUpBase:     p.MinUserAddress(),
		TopDownBase:      p.MaxUserAddress(),
		DefaultDirection: arch.MmapBottomUp,
	}
	mf.EnableDirtyTracking()
	mf.SwapDirty(true)
	return mm, mm.as.(*recordingAddressSpace)
}

// mmap maps n anonymous pages in mm.
func mmap(t *testing.T, ctx context.Context, mm *MemoryManager, n uint64, private bool) hostarch.AddrRange {
	t.Helper()
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   n * hostarch.PageSize,
		Private:  private,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	return hostarch.AddrRange{addr, addr + hostarch.Addr(n*hostarch.PageSize)}
}

// fault simulates an application access of type at to addr.
func fault(t *testing.T, ctx context.Context, mm *MemoryManager, addr hostarch.Addr, at hostarch.AccessType) {
	t.Helper()
	if err := mm.HandleUserFault(ctx, addr, at, 0); err != nil {
		t.Fatalf("HandleUserFault(%#x, %v): %v", addr, at, err)
	}
}

// copyOut writes n bytes at addr as the Sentry does on behalf of the
// application.
func copyOut(t *testing.T, ctx context.Context, mm *MemoryManager, addr hostarch.Addr, n int) {
	t.Helper()
	if _, err := mm.CopyOut(ctx, addr, make([]byte, n), usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut(%#x, %d): %v", addr, n, err)
	}
}

// dirtyPages returns the pages of ar that are in the dirty set swapped from
// mm's MemoryFile, as page indices into ar.
func dirtyPages(t *testing.T, mm *MemoryManager, ar hostarch.AddrRange) []uint64 {
	t.Helper()
	return dirtyPagesOf(mm, mm.mf.SwapDirty(true), ar)
}

// dirtyPagesOf returns the pages of ar in s, as page indices into ar.
func dirtyPagesOf(mm *MemoryManager, s *pgalloc.DirtySet, ar hostarch.AddrRange) []uint64 {
	var pages []uint64
	mm.activeMu.RLock()
	defer mm.activeMu.RUnlock()
	for addr := ar.Start; addr < ar.End; addr += hostarch.PageSize {
		pseg := mm.pmas.FindSegment(addr)
		if !pseg.Ok() {
			continue
		}
		if s.Contains(pseg.fileRangeOf(hostarch.AddrRange{addr, addr + hostarch.PageSize}).Start) {
			pages = append(pages, uint64(addr-ar.Start)/hostarch.PageSize)
		}
	}
	return pages
}

// pageRange returns the pages [first, first+n) of ar.
func pageRange(ar hostarch.AddrRange, first, n uint64) []uint64 {
	var pages []uint64
	for i := first; i < first+n && ar.Start+hostarch.Addr(i*hostarch.PageSize) < ar.End; i++ {
		pages = append(pages, i)
	}
	return pages
}

func page(ar hostarch.AddrRange, i uint64) hostarch.Addr {
	return ar.Start + hostarch.Addr(i*hostarch.PageSize)
}

func TestDirtyTrackingApplicationWrites(t *testing.T) {
	for _, unit := range []uint64{hostarch.PageSize, 64 << 10} {
		t.Run(fmt.Sprintf("unit=%d", unit), func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm, as := dirtyTestMM(t, ctx)
			ar := mmap(t, ctx, mm, 64, true /* private */)
			// Fault the memory in before arming, as written by a previous
			// epoch.
			for i := uint64(0); i < 64; i++ {
				fault(t, ctx, mm, page(ar, i), hostarch.Write)
			}
			mm.mf.SwapDirty(true)

			mm.ArmDirtyTracking(unit)
			if at, ok := as.mapped[page(ar, 20)]; ok && at.Write {
				t.Fatalf("page 20 still mapped writable after arming")
			}
			// Reads do not disarm.
			fault(t, ctx, mm, page(ar, 20), hostarch.Read)
			if at := as.mapped[page(ar, 20)]; at.Write {
				t.Errorf("page 20 mapped writable after a read")
			}
			// The first write disarms its unit, which becomes writable and
			// dirty.
			fault(t, ctx, mm, page(ar, 20), hostarch.Write)
			unitPages := unit / hostarch.PageSize
			first := 20 / unitPages * unitPages
			want := pageRange(ar, first, unitPages)
			for _, p := range want {
				if at := as.mapped[page(ar, p)]; !at.Write {
					t.Errorf("page %d of the written unit is not mapped writable", p)
				}
			}
			if got := dirtyPages(t, mm, ar); !slices.Equal(got, want) {
				t.Errorf("dirty pages after one write: got %v, want %v", got, want)
			}
			// Further writes to the unit in the same epoch do not fault.
			mm.activeMu.RLock()
			armed := mm.pmas.FindSegment(page(ar, 20)).ValuePtr().dirtyArmed
			mm.activeMu.RUnlock()
			if armed {
				t.Errorf("written unit still armed")
			}
		})
	}
}

func TestDirtyTrackingSentryWrites(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, _ := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 32, true /* private */)
	copyOut(t, ctx, mm, ar.Start, int(ar.Length()))
	mm.mf.SwapDirty(true)

	mm.ArmDirtyTracking(hostarch.PageSize)
	// CopyOut through cached internal mappings must not bypass tracking.
	copyOut(t, ctx, mm, page(ar, 5)+100, 2*hostarch.PageSize)
	if got, want := dirtyPagesOf(mm, mm.mf.SwapDirty(false /* paused */), ar), []uint64{5, 6, 7}; !slices.Equal(got, want) {
		t.Errorf("dirty pages after CopyOut: got %v, want %v", got, want)
	}
	// The MemoryManager tracks the writes through its internal mappings
	// itself, so obtaining them for new memory does not make the pages dirty
	// again in the next epoch, as MapInternal's writers do when the dirty
	// set is swapped while writers run.
	fresh := mmap(t, ctx, mm, 4, true /* private */)
	copyOut(t, ctx, mm, page(fresh, 1), hostarch.PageSize)
	if got, want := dirtyPagesOf(mm, mm.mf.SwapDirty(false /* paused */), fresh), []uint64{1}; !slices.Equal(got, want) {
		t.Errorf("dirty pages after CopyOut to new memory: got %v, want %v", got, want)
	}
	if got := dirtyPages(t, mm, fresh); len(got) != 0 {
		t.Errorf("dirty pages reported again by the next swap: %v", got)
	}
	// Nothing else became dirty: reading did not mark the pma.
	if _, err := mm.CopyIn(ctx, ar.Start, make([]byte, ar.Length()), usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	if got := dirtyPages(t, mm, ar); len(got) != 0 {
		t.Errorf("dirty pages after CopyIn: %v", got)
	}
}

func TestDirtyTrackingRearm(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, as := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 64, true /* private */)
	for i := uint64(0); i < 64; i++ {
		fault(t, ctx, mm, page(ar, i), hostarch.Write)
	}
	mm.ArmDirtyTracking(hostarch.PageSize)
	for _, i := range []uint64{1, 3, 5, 7, 40} {
		fault(t, ctx, mm, page(ar, i), hostarch.Write)
	}
	mm.mf.SwapDirty(true)

	// Re-arming makes the written units write-protected again, with one unmap
	// for the vma however many units were written, and merges the pmas.
	unmaps := as.unmaps
	mm.ArmDirtyTracking(hostarch.PageSize)
	if got := as.unmaps - unmaps; got != 1 {
		t.Errorf("re-arming unmapped %d times, want once", got)
	}
	mm.activeMu.RLock()
	npmas := 0
	for pseg := mm.pmas.LowerBoundSegment(ar.Start); pseg.Ok() && pseg.Start() < ar.End; pseg = pseg.NextSegment() {
		npmas++
	}
	mm.activeMu.RUnlock()
	if npmas != 1 {
		t.Errorf("%d pmas after re-arming, want 1", npmas)
	}
	fault(t, ctx, mm, page(ar, 3), hostarch.Write)
	if got, want := dirtyPages(t, mm, ar), []uint64{3}; !slices.Equal(got, want) {
		t.Errorf("dirty pages in the next epoch: got %v, want %v", got, want)
	}
}

func TestDirtyTrackingMProtect(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, _ := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 8, true /* private */)
	for i := uint64(0); i < 8; i++ {
		fault(t, ctx, mm, page(ar, i), hostarch.Write)
	}
	mm.ArmDirtyTracking(hostarch.PageSize)
	mm.mf.SwapDirty(true)
	// Making the memory read-only and then writable again must not make it
	// writable without tracking.
	if err := mm.MProtect(ar.Start, uint64(ar.Length()), hostarch.Read, false); err != nil {
		t.Fatalf("MProtect(Read): %v", err)
	}
	if err := mm.MProtect(ar.Start, uint64(ar.Length()), hostarch.ReadWrite, false); err != nil {
		t.Fatalf("MProtect(ReadWrite): %v", err)
	}
	mm.activeMu.RLock()
	pma := mm.pmas.FindSegment(page(ar, 2)).ValuePtr()
	writable, armed := pma.effectivePerms.Write, pma.dirtyArmed
	mm.activeMu.RUnlock()
	if writable || !armed {
		t.Errorf("after mprotect: writable=%v armed=%v, want an armed read-only pma", writable, armed)
	}
	fault(t, ctx, mm, page(ar, 2), hostarch.Write)
	if got, want := dirtyPages(t, mm, ar), []uint64{2}; !slices.Equal(got, want) {
		t.Errorf("dirty pages: got %v, want %v", got, want)
	}
}

func TestDirtyTrackingForkCopyOnWrite(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, as := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 8, true /* private */)
	for i := uint64(0); i < 8; i++ {
		fault(t, ctx, mm, page(ar, i), hostarch.Write)
	}
	mm.ArmDirtyTracking(hostarch.PageSize)
	mm.mf.SwapDirty(true)

	child, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	// The child's write copies the page; the copy is dirty.
	copyOut(t, ctx, child, page(ar, 1), 8)
	// Once the child exits, the parent owns its pages again without a
	// copy; its writes must still be tracked: the page it writes is dirty,
	// and the others are not mapped writable.
	child.DecUsers(ctx)
	fault(t, ctx, mm, page(ar, 6), hostarch.Write)
	fault(t, ctx, mm, page(ar, 2), hostarch.Read)
	if at := as.mapped[page(ar, 2)]; at.Write {
		t.Errorf("page 2 mapped writable after the parent took ownership of it")
	}
	s := mm.mf.SwapDirty(true)
	mm.activeMu.RLock()
	parentPage := mm.pmas.FindSegment(page(ar, 6))
	written := parentPage.fileRangeOf(hostarch.AddrRange{page(ar, 6), page(ar, 7)})
	mm.activeMu.RUnlock()
	if !s.Contains(written.Start) {
		t.Errorf("the parent's write after the child exited is not dirty")
	}
	if s.Bytes() < 2*hostarch.PageSize {
		t.Errorf("dirty set has %d bytes, want at least the child's copy and the parent's write", s.Bytes())
	}
}

func TestDirtyTrackingSharedMemory(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, _ := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 8, false /* private */)
	for i := uint64(0); i < 8; i++ {
		fault(t, ctx, mm, page(ar, i), hostarch.Write)
	}
	mm.ArmDirtyTracking(hostarch.PageSize)
	mm.mf.SwapDirty(true)
	fault(t, ctx, mm, page(ar, 4), hostarch.Write)
	if got, want := dirtyPages(t, mm, ar), []uint64{4}; !slices.Equal(got, want) {
		t.Errorf("dirty pages of shared memory: got %v, want %v", got, want)
	}
}

func TestDirtyTrackingNewPMAs(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, _ := dirtyTestMM(t, ctx)
	mm.ArmDirtyTracking(hostarch.PageSize)
	// Memory first touched after arming is tracked from its first write.
	ar := mmap(t, ctx, mm, 16, true /* private */)
	fault(t, ctx, mm, page(ar, 0), hostarch.Read)
	fault(t, ctx, mm, page(ar, 9), hostarch.Write)
	if got, want := dirtyPages(t, mm, ar), []uint64{9}; !slices.Equal(got, want) {
		t.Errorf("dirty pages of new memory: got %v, want %v", got, want)
	}
}

func TestDirtyTrackingMRemap(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, _ := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 8, true /* private */)
	for i := uint64(0); i < 8; i++ {
		fault(t, ctx, mm, page(ar, i), hostarch.Write)
	}
	mm.ArmDirtyTracking(hostarch.PageSize)
	mm.mf.SwapDirty(true)
	newAddr, err := mm.MRemap(ctx, ar.Start, uint64(ar.Length()), 2*uint64(ar.Length()), MRemapOpts{Move: MRemapMayMove})
	if err != nil {
		t.Fatalf("MRemap: %v", err)
	}
	newAR := hostarch.AddrRange{newAddr, newAddr + 2*ar.Length()}
	// Moved pmas stay armed.
	fault(t, ctx, mm, page(newAR, 3), hostarch.Write)
	if got, want := dirtyPages(t, mm, newAR), []uint64{3}; !slices.Equal(got, want) {
		t.Errorf("dirty pages after mremap: got %v, want %v", got, want)
	}
}

func TestDirtyTrackingHugePMAs(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, _ := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 2*hostarch.HugePageSize/hostarch.PageSize, true /* private */)
	fault(t, ctx, mm, ar.Start, hostarch.Write)
	mm.activeMu.Lock()
	for pseg := mm.pmas.LowerBoundSegment(ar.Start); pseg.Ok() && pseg.Start() < ar.End; pseg = pseg.NextSegment() {
		// Make the pmas huge, as with ExpectHugepages.
		pseg.ValuePtr().huge = true
	}
	mm.activeMu.Unlock()
	mm.ArmDirtyTracking(hostarch.PageSize)
	mm.mf.SwapDirty(true)
	fault(t, ctx, mm, page(ar, 3), hostarch.Write)
	// Huge pmas disarm whole huge pages.
	if got := uint64(len(dirtyPages(t, mm, ar))); got != hostarch.HugePageSize/hostarch.PageSize {
		t.Errorf("%d dirty pages after a write to a huge pma, want a huge page", got)
	}
}

// TestDirtyTrackingSaveRestore checks that pmas armed when they are saved
// become writable, and record writes, after they are restored, whether or not
// the restored MemoryFile is tracked: the permissions that arming withholds
// are saved with the pmas. (Otherwise, an application write faults forever:
// the fault maps the pma without write permission again.)
func TestDirtyTrackingSaveRestore(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, as := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 4, true)
	copyOut(t, ctx, mm, ar.Start, int(ar.Length()))
	mm.ArmDirtyTracking(hostarch.PageSize)
	mm.mf.SwapDirty(true)

	// Save and load mm's pmas, as a save and restore of mm do.
	mm.mf.MarkSavable()
	mm.activeMu.Lock()
	var buf bytes.Buffer
	if _, err := state.Save(ctx, &buf, &mm.pmas); err != nil {
		mm.activeMu.Unlock()
		t.Fatalf("saving pmas: %v", err)
	}
	var restored pmaSet
	if _, err := state.Load(ctx, &buf, &restored); err != nil {
		mm.activeMu.Unlock()
		t.Fatalf("loading pmas: %v", err)
	}
	// restored holds the references that mm.pmas held.
	mm.pmas = restored
	mm.activeMu.Unlock()

	fault(t, ctx, mm, page(ar, 1), hostarch.Write)
	if at := as.mapped[page(ar, 1)]; !at.Write {
		t.Errorf("page 1 mapped %v after a write fault, want writable", at)
	}
	copyOut(t, ctx, mm, page(ar, 2), hostarch.PageSize)
	if got, want := dirtyPages(t, mm, ar), []uint64{1, 2}; !slices.Equal(got, want) {
		t.Errorf("dirty pages after restore: got %v, want %v", got, want)
	}
}

// TestDirtyThrottleDelay checks the delays that limit the rate at which a
// MemoryManager's tasks dirty memory.
func TestDirtyThrottleDelay(t *testing.T) {
	ctx := contexttest.Context(t)
	mm, _ := dirtyTestMM(t, ctx)
	ar := mmap(t, ctx, mm, 16, true)
	copyOut(t, ctx, mm, ar.Start, int(ar.Length()))
	mm.ArmDirtyTracking(hostarch.PageSize)
	const (
		limit = 40 * hostarch.PageSize // bytes per second; 4 pages of burst
		ms    = int64(time.Millisecond)
	)

	// The first call of a session charges nothing dirtied before it.
	copyOut(t, ctx, mm, page(ar, 0), hostarch.PageSize)
	if d := mm.DirtyThrottleDelay(limit, 1, 0); d != 0 {
		t.Errorf("first delay of a session: got %v, want 0", d)
	}
	// Within the burst, no delay.
	copyOut(t, ctx, mm, page(ar, 1), 3*hostarch.PageSize)
	if d := mm.DirtyThrottleDelay(limit, 1, 0); d != 0 {
		t.Errorf("delay within the burst: got %v, want 0", d)
	}
	// 6 pages more: 5 over the limit, which takes 125 ms to make up.
	copyOut(t, ctx, mm, page(ar, 4), 6*hostarch.PageSize)
	if d, want := mm.DirtyThrottleDelay(limit, 1, 0), 125*time.Millisecond; d != want {
		t.Errorf("delay over the limit: got %v, want %v", d, want)
	}
	// 125 ms later, the deficit is made up.
	if d := mm.DirtyThrottleDelay(limit, 1, 125*ms); d != 0 {
		t.Errorf("delay after making up the deficit: got %v, want 0", d)
	}
	// Writes to disarmed units are not charged.
	copyOut(t, ctx, mm, page(ar, 4), 6*hostarch.PageSize)
	if d := mm.DirtyThrottleDelay(limit, 1, 125*ms); d != 0 {
		t.Errorf("delay after rewriting dirty units: got %v, want 0", d)
	}
	// A new session starts with full credit.
	copyOut(t, ctx, mm, page(ar, 10), 6*hostarch.PageSize)
	if d := mm.DirtyThrottleDelay(limit, 2, 125*ms); d != 0 {
		t.Errorf("first delay of a new session: got %v, want 0", d)
	}
}
