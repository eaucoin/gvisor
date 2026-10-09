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

package systrap

import (
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/systrap/testutil"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

const dataAddr = hostarch.Addr(0x200000)

// writeTrackingTest runs testutil.StoreCode in a stub that maps pages of a
// MemoryFile at dataAddr.
type writeTrackingTest struct {
	t    *testing.T
	s    *Systrap
	as   platform.AddressSpace
	pctx *platformContext
	mm   *testMemoryManager
	ac   *arch.Context64
	mf   *pgalloc.MemoryFile
	fr   memmap.FileRange
}

func newWriteTrackingTest(t *testing.T, pages uint64) *writeTrackingTest {
	if testProcFS == nil {
		t.Skip("write tracking is not available")
	}
	s, as, pctx, ac := newSystrapTest(t, testutil.StoreCode)
	memfd, err := memutil.CreateMemFD("write-tracking-test", 0)
	if err != nil {
		t.Fatalf("CreateMemFD: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(os.NewFile(uintptr(memfd), "write-tracking-test"), pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(mf.Destroy)
	fr, err := mf.Allocate(pages*hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	t.Cleanup(func() { mf.DecRef(fr) })
	wt := &writeTrackingTest{t: t, s: s, as: as, pctx: pctx, mm: &testMemoryManager{as: as}, mf: mf, fr: fr}
	// Run to the first getpid, from which the program stores where getpid
	// "returns".
	wt.switchOnce(ac)
	wt.ac = ac
	return wt
}

// switchOnce switches to the program until its next getpid.
func (wt *writeTrackingTest) switchOnce(ac *arch.Context64) {
	wt.t.Helper()
	if si, _, err := wt.pctx.Switch(pkgcontext.Background(), wt.mm, ac, 0); err != nil {
		wt.t.Fatalf("Switch: %v (signal %v)", err, si)
	}
	if sysno := ac.SyscallNo(); sysno != unix.SYS_GETPID {
		wt.t.Fatalf("program made syscall %d, want getpid", sysno)
	}
}

// store makes the program store to page i of the mapping at dataAddr.
func (wt *writeTrackingTest) store(i uint64) {
	wt.t.Helper()
	wt.ac.SetReturn(uintptr(dataAddr) + uintptr(i*hostarch.PageSize))
	wt.switchOnce(wt.ac)
}

// mapPages maps the MemoryFile's pages at dataAddr.
func (wt *writeTrackingTest) mapPages() {
	wt.t.Helper()
	if err := wt.as.MapFile(dataAddr, wt.mf, wt.fr, hostarch.ReadWrite, false); err != nil {
		wt.t.Fatalf("MapFile: %v", err)
	}
}

// dirty returns the pages of the mapping that the MemoryFile has marked dirty
// since the last call, after harvesting the writes if harvest is true.
func (wt *writeTrackingTest) dirty(harvest bool) []uint64 {
	wt.t.Helper()
	if harvest {
		if err := wt.s.HarvestWrites(); err != nil {
			wt.t.Fatalf("HarvestWrites: %v", err)
		}
	}
	s := wt.mf.SwapDirty(true /* paused */)
	var pages []uint64
	for off := wt.fr.Start; off < wt.fr.End; off += hostarch.PageSize {
		if s.Contains(off) {
			pages = append(pages, (off-wt.fr.Start)/hostarch.PageSize)
		}
	}
	return pages
}

func (wt *writeTrackingTest) checkDirty(what string, harvest bool, want ...uint64) {
	wt.t.Helper()
	if got := wt.dirty(harvest); !slices.Equal(got, want) {
		wt.t.Errorf("%s: dirty pages %v, want %v", what, got, want)
	}
}

func TestWriteTracking(t *testing.T) {
	wt := newWriteTrackingTest(t, 16)
	wt.mf.EnableDirtyTracking()
	wt.mapPages()
	wt.checkDirty("after mapping", true)

	wt.store(1)
	wt.store(5)
	wt.checkDirty("after stores", true, 1, 5)
	// The harvest write-protected the stored pages again.
	wt.checkDirty("after a harvest", true)
	wt.store(5)
	wt.checkDirty("after a second store", true, 5)

	// Unmapping or replacing a mapping harvests it first: its write-protection
	// state is lost with it.
	wt.store(7)
	wt.as.Unmap(dataAddr+7*hostarch.PageSize, hostarch.PageSize)
	wt.checkDirty("after unmapping a stored page", false, 7)
	wt.store(9)
	wt.mapPages()
	wt.checkDirty("after replacing a stored page", false, 9)
	// The replacing mapping is tracked.
	wt.store(7)
	wt.checkDirty("after a store to the replacing mapping", true, 7)
}

// TestWriteTrackingArm checks that the mappings of a MemoryFile that tracks
// dirty pages only after it was mapped are tracked from the next ArmWrites.
func TestWriteTrackingArm(t *testing.T) {
	wt := newWriteTrackingTest(t, 4)
	wt.mapPages()
	wt.store(0)
	wt.mf.EnableDirtyTracking()
	if err := wt.s.ArmWrites(); err != nil {
		t.Fatalf("ArmWrites: %v", err)
	}
	wt.checkDirty("after arming", true)
	wt.store(2)
	wt.checkDirty("after a store", true, 2)
}

// TestWriteTrackingDeadStub checks that the mappings of a stub that died, whose
// written pages cannot be read, are reported written whole.
func TestWriteTrackingDeadStub(t *testing.T) {
	wt := newWriteTrackingTest(t, 4)
	wt.mf.EnableDirtyTracking()
	wt.mapPages()
	wt.checkDirty("after mapping", true)
	stub := wt.as.(*subprocess)
	stub.kill()
	stub.releaseWriteTracking()
	wt.checkDirty("after the stub died", false, 0, 1, 2, 3)
}
