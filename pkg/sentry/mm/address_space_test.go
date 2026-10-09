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
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// recordingAddressSpace is a platform.AddressSpace that records the calls
// made to it, and maps nothing.
type recordingAddressSpace struct {
	platform.NoAddressSpaceIO

	// mapped records the ranges of the calls to MapFile, in order.
	mapped []hostarch.AddrRange

	// pages records, for each page mapped and not unmapped since, the access
	// type it was mapped with.
	pages map[hostarch.Addr]hostarch.AccessType

	// unmaps counts the calls to Unmap.
	unmaps int
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *recordingAddressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	as.mapped = append(as.mapped, hostarch.AddrRange{Start: addr, End: addr + hostarch.Addr(fr.Length())})
	if as.pages == nil {
		as.pages = make(map[hostarch.Addr]hostarch.AccessType)
	}
	for off := uint64(0); off < fr.Length(); off += hostarch.PageSize {
		as.pages[addr+hostarch.Addr(off)] = at
	}
	return nil
}

// Unmap implements platform.AddressSpace.Unmap.
func (as *recordingAddressSpace) Unmap(addr hostarch.Addr, length uint64) {
	as.unmaps++
	for off := uint64(0); off < length; off += hostarch.PageSize {
		delete(as.pages, addr+hostarch.Addr(off))
	}
}

// Release implements platform.AddressSpace.Release.
func (*recordingAddressSpace) Release() {}

// PreFork implements platform.AddressSpace.PreFork.
func (*recordingAddressSpace) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (*recordingAddressSpace) PostFork() {}

// gatedReaderAt is an io.ReaderAt whose reads wait until open is closed.
type gatedReaderAt struct {
	r    io.ReaderAt
	open chan struct{}
}

// ReadAt implements io.ReaderAt.ReadAt.
func (g *gatedReaderAt) ReadAt(dst []byte, off int64) (int, error) {
	<-g.open
	return g.r.ReadAt(dst, off)
}

// newTestMemoryFile returns a new MemoryFile backed by a memfd.
func newTestMemoryFile(t *testing.T, expectHugepages bool) *pgalloc.MemoryFile {
	t.Helper()
	fd, err := memutil.CreateMemFD("mm-test", 0)
	if err != nil {
		t.Fatalf("CreateMemFD: %v", err)
	}
	file := os.NewFile(uintptr(fd), "mm-test")
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DisableMemoryAccounting: true,
		ExpectHugepages:         expectHugepages,
	})
	if err != nil {
		file.Close()
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(mf.Destroy)
	return mf
}

// loadingMemoryFile returns a MemoryFile being restored, as runsc restore
// --background does, from an image of one page that it has not loaded yet,
// and a function that lets it load the page and waits until loading has
// ended.
func loadingMemoryFile(ctx context.Context, t *testing.T, expectHugepages bool) (*pgalloc.MemoryFile, func()) {
	t.Helper()

	// Save an image of one page.
	src := newTestMemoryFile(t, false /* expectHugepages */)
	fr, err := src.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndCommit})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	defer src.DecRef(fr)
	// Write the page, since zero pages are saved without data, and so need no
	// loading.
	ims, err := src.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	if _, err := safemem.CopySeq(ims, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(bytes.Repeat([]byte{1}, hostarch.PageSize)))); err != nil {
		t.Fatalf("writing the page: %v", err)
	}
	var meta, pages bytes.Buffer
	saved := make(chan error, 1)
	apfs, err := pgalloc.StartAsyncPagesFileSave(stateio.NewIOWriter(&pages, 256<<10, 64, 4), func(err error) { saved <- err })
	if err != nil {
		t.Fatalf("StartAsyncPagesFileSave: %v", err)
	}
	w := checkpointimage.NewWriter(&meta, &pgallocpb.ImageProto{Layers: []*pgallocpb.LayerProto{{}}})
	if err := src.SaveTo(ctx, w, &pgalloc.SaveOpts{PagesFile: apfs}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	apfs.MemoryFilesDone()
	if err := <-saved; err != nil {
		t.Fatalf("saving pages: %v", err)
	}
	img, err := w.Finish(apfs.PagesFileOffset())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Restore it from a pages file whose reads wait for open.
	open := make(chan struct{})
	var openOnce sync.Once
	letLoad := func() { openOnce.Do(func() { close(open) }) }
	t.Cleanup(letLoad)
	ar := stateio.NewIOReader(&gatedReaderAt{bytes.NewReader(pages.Bytes()), open}, 256<<10, 64, 4)
	apfl, err := pgalloc.StartAsyncPagesFileLoad(ar, func(error) {}, nil /* timeline */)
	if err != nil {
		t.Fatalf("StartAsyncPagesFileLoad: %v", err)
	}
	mf := newTestMemoryFile(t, expectHugepages)
	loaded := make(chan error, 1)
	opts := pgalloc.LoadOpts{
		Image:        img.Proto,
		PagesFiles:   []*pgalloc.AsyncPagesFileLoad{apfl},
		DoneCallback: func(err error) { loaded <- err },
	}
	if err := mf.LoadFrom(ctx, img.MemoryFileRecords(), &opts); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	apfl.MemoryFilesDone()
	if !mf.IsAsyncLoading() {
		t.Fatalf("MemoryFile is not loading")
	}
	return mf, func() {
		t.Helper()
		letLoad()
		select {
		case err := <-loaded:
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for loading to end")
		}
		mf.DecRef(fr)
	}
}

// TestMapUnitWhileLoading checks how much of a pma a first touch maps while
// its MemoryFile is being loaded by a background restore, since mapping waits
// for every page mapped to be loaded: 64 KiB around the touch, or the huge
// page for pmas backed by huge pages. Once loading has ended, pmas are mapped
// whole again.
func TestMapUnitWhileLoading(t *testing.T) {
	for _, test := range []struct {
		name string
		huge bool
		unit hostarch.Addr
	}{
		{name: "small pages", unit: asyncLoadingMapUnit},
		{name: "huge pages", huge: true, unit: hostarch.HugePageSize},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			mf, finishLoading := loadingMemoryFile(ctx, t, test.huge)
			p := platform.FromContext(ctx)
			mm, err := NewMemoryManager(p, mf)
			if err != nil {
				t.Fatalf("NewMemoryManager: %v", err)
			}
			mm.layout = arch.MmapLayout{
				MinAddr:          p.MinUserAddress(),
				MaxAddr:          p.MaxUserAddress(),
				BottomUpBase:     p.MinUserAddress(),
				TopDownBase:      p.MaxUserAddress(),
				DefaultDirection: arch.MmapBottomUp,
			}
			defer mm.DecUsers(ctx)
			as := &recordingAddressSpace{}
			mm.as = as

			// Map two huge pages, populated so that they get one pma.
			const length = 2 * hostarch.HugePageSize
			addr, err := mm.MMap(ctx, memmap.MMapOpts{
				Length:         length,
				Addr:           1 << 30,
				Fixed:          true,
				Private:        true,
				Perms:          hostarch.ReadWrite,
				MaxPerms:       hostarch.AnyAccess,
				PlatformEffect: memmap.PlatformEffectPopulate,
			})
			if err != nil {
				t.Fatalf("MMap: %v", err)
			}
			mm.activeMu.RLock()
			pseg := mm.pmas.FindSegment(addr)
			pmaAR, huge := pseg.Range(), pseg.ValuePtr().huge
			mm.activeMu.RUnlock()
			if want := (hostarch.AddrRange{Start: addr, End: addr + length}); pmaAR != want {
				t.Fatalf("pma %v, want one pma for the whole mapping %v", pmaAR, want)
			}
			if huge != test.huge {
				t.Fatalf("pma huge = %t, want %t", huge, test.huge)
			}

			touch := addr + hostarch.HugePageSize + 5*hostarch.PageSize
			firstTouch := func() hostarch.AddrRange {
				t.Helper()
				as.mapped = nil
				if err := mm.HandleUserFault(ctx, touch, hostarch.Write, 0 /* sp */); err != nil {
					t.Fatalf("HandleUserFault: %v", err)
				}
				if len(as.mapped) != 1 {
					t.Fatalf("a touch mapped %v, want one range", as.mapped)
				}
				return as.mapped[0]
			}
			// around returns the range of length unit around touch.
			around := func(unit hostarch.Addr) hostarch.AddrRange {
				start := touch &^ (unit - 1)
				return hostarch.AddrRange{Start: start, End: start + unit}
			}
			if got, want := firstTouch(), around(test.unit); got != want {
				t.Errorf("while loading, a touch mapped %v, want %v", got, want)
			}
			finishLoading()
			if mf.IsAsyncLoading() {
				t.Fatalf("MemoryFile is still loading")
			}
			// Once loading has ended, pmas are mapped whole, or in the
			// platform's map unit if it has one.
			want := pmaAR
			if unit := p.MapUnit(); unit != 0 {
				want = want.Intersect(around(hostarch.Addr(unit)))
			}
			if got := firstTouch(); got != want {
				t.Errorf("after loading, a touch mapped %v, want %v", got, want)
			}
		})
	}
}
