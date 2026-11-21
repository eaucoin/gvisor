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
	"errors"
	"io"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/goid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/usermem"
)

// loadingAddressSpace is a recordingAddressSpace whose MapFile waits for the
// pages it maps to be loaded, as platforms' do through memmap.File.DataFD or
// MapInternal, and which may be called concurrently, as MapFile is by faults
// holding mm.activeMu for reading.
type loadingAddressSpace struct {
	mu sync.Mutex
	recordingAddressSpace
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *loadingAddressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	if _, err := f.DataFD(fr); err != nil {
		return err
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.recordingAddressSpace.MapFile(addr, f, fr, at, precommit)
}

// Unmap implements platform.AddressSpace.Unmap. It looks up the pages mapped
// rather than every page of the range, which may span the address space.
func (as *loadingAddressSpace) Unmap(addr hostarch.Addr, length uint64) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.unmaps++
	for page := range as.pages {
		if page >= addr && uint64(page-addr) < length {
			delete(as.pages, page)
		}
	}
}

// mappedAs returns the access type that addr is mapped with, if it is mapped.
func (as *loadingAddressSpace) mappedAs(addr hostarch.Addr) (hostarch.AccessType, bool) {
	as.mu.Lock()
	defer as.mu.Unlock()
	at, ok := as.pages[addr]
	return at, ok
}

// pagesReader is the pages file of a restore, whose reads wait until the test
// lets them complete: all of them, or those within ranges it lets. Reads that
// overlap ranges it fails fail at once.
type pagesReader struct {
	r io.ReaderAt

	mu   sync.Mutex
	cond sync.Cond

	// If all is true, every read completes. let and fail are the ranges of
	// pages file offsets whose reads complete, and fail. started holds the
	// ranges of the reads that began. If closed is true, reads that wait
	// fail. These fields are protected by mu.
	all     bool
	let     []memmap.FileRange
	fail    []memmap.FileRange
	started []memmap.FileRange
	closed  bool
}

func newPagesReader(r io.ReaderAt) *pagesReader {
	pr := &pagesReader{r: r}
	pr.cond.L = &pr.mu
	return pr
}

// errPagesRead is the error of the reads that a pagesReader fails.
var errPagesRead = errors.New("pages file read failed by the test")

// ReadAt implements io.ReaderAt.ReadAt.
func (pr *pagesReader) ReadAt(p []byte, off int64) (int, error) {
	rd := memmap.FileRange{uint64(off), uint64(off) + uint64(len(p))}
	pr.mu.Lock()
	pr.started = append(pr.started, rd)
	pr.cond.Broadcast()
	for {
		if pr.closed || slices.ContainsFunc(pr.fail, rd.Overlaps) {
			pr.mu.Unlock()
			return 0, errPagesRead
		}
		if pr.all || slices.ContainsFunc(pr.let, func(fr memmap.FileRange) bool { return fr.IsSupersetOf(rd) }) {
			break
		}
		pr.cond.Wait()
	}
	pr.mu.Unlock()
	return pr.r.ReadAt(p, off)
}

// close fails the reads that wait, and those that begin after.
func (pr *pagesReader) close() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.closed = true
	pr.cond.Broadcast()
}

// letAll lets every read complete.
func (pr *pagesReader) letAll() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.all = true
	pr.cond.Broadcast()
}

// letRange lets the reads within fr complete.
func (pr *pagesReader) letRange(fr memmap.FileRange) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.let = append(pr.let, fr)
	pr.cond.Broadcast()
}

// failRange makes the reads that overlap fr fail.
func (pr *pagesReader) failRange(fr memmap.FileRange) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.fail = append(pr.fail, fr)
	pr.cond.Broadcast()
}

// waitStarted waits until a read of fr has begun.
func (pr *pagesReader) waitStarted(t *testing.T, fr memmap.FileRange) {
	t.Helper()
	timer := time.AfterFunc(10*time.Second, func() {
		pr.mu.Lock()
		defer pr.mu.Unlock()
		pr.cond.Broadcast()
	})
	defer timer.Stop()
	deadline := time.Now().Add(10 * time.Second)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	for !slices.Contains(pr.started, fr) {
		if time.Now().After(deadline) {
			t.Fatalf("no read of pages file range %v began; reads: %v", fr, pr.started)
		}
		pr.cond.Wait()
	}
}

// pagesFile is the stateio.WaitOrAsyncReader of a restore's pages file, read
// through a pagesReader, which serves reads of 1 MiB, maxParallel at a time,
// each read in a goroutine of its own. As the pages files of restores are
// (stateio.FDReader, the checkpoint gofers'), it can be woken as it waits for
// reads, so that an awaited read is enqueued while background reads are in
// flight.
type pagesFile struct {
	stateio.NoRegisterClientFD
	pr          *pagesReader
	maxParallel int
	cmps        chan stateio.Completion
	reads       sync.WaitGroup
}

func newPagesFile(pr *pagesReader, maxParallel int) *pagesFile {
	return &pagesFile{
		pr:          pr,
		maxParallel: maxParallel,
		cmps:        make(chan stateio.Completion, maxParallel),
	}
}

// Close implements stateio.AsyncReader.Close.
func (pf *pagesFile) Close() error {
	pf.pr.close()
	pf.reads.Wait()
	return nil
}

// MaxReadBytes implements stateio.AsyncReader.MaxReadBytes.
func (pf *pagesFile) MaxReadBytes() uint64 {
	return 1 << 20
}

// MaxRanges implements stateio.AsyncReader.MaxRanges.
func (pf *pagesFile) MaxRanges() int {
	return 1
}

// MaxParallel implements stateio.AsyncReader.MaxParallel.
func (pf *pagesFile) MaxParallel() int {
	return pf.maxParallel
}

// AddRead implements stateio.AsyncReader.AddRead.
func (pf *pagesFile) AddRead(id int, off int64, _ stateio.DestinationFile, _ memmap.FileRange, dstMap []byte) {
	pf.read(id, off, stateio.LocalClientMapping(dstMap))
}

// AddReadv implements stateio.AsyncReader.AddReadv.
func (pf *pagesFile) AddReadv(id int, off int64, _ uint64, _ stateio.DestinationFile, _ []memmap.FileRange, dstMaps []unix.Iovec) {
	pf.read(id, off, stateio.LocalClientMappings(dstMaps))
}

func (pf *pagesFile) read(id int, off int64, dst stateio.LocalClientRanges) {
	pf.reads.Add(1)
	go func() {
		defer pf.reads.Done()
		c := stateio.Completion{ID: id}
		for _, m := range dst.Mappings {
			n, err := pf.pr.ReadAt(m, off)
			off += int64(n)
			c.N += uint64(n)
			if err != nil {
				c.Err = err
				break
			}
		}
		pf.cmps <- c
	}()
}

// Wait implements stateio.AsyncReader.Wait.
func (pf *pagesFile) Wait(cs []stateio.Completion, minCompletions int) ([]stateio.Completion, error) {
	return stateio.CompletionChanWait(pf.cmps, cs, minCompletions)
}

// WaitOr implements stateio.WaitOrAsyncReader.WaitOr.
func (pf *pagesFile) WaitOr(cs []stateio.Completion, wake <-chan struct{}) ([]stateio.Completion, error) {
	return stateio.CompletionChanWaitOr(pf.cmps, cs, wake)
}

// restoredPages is the memory of a MemoryManager restored from a checkpoint
// whose pages are loaded asynchronously, from a pages file whose reads wait
// until the test lets them complete.
type restoredPages struct {
	mm *MemoryManager
	as *loadingAddressSpace
	mf *pgalloc.MemoryFile
	pf *pagesReader

	// ar is the restored memory, whose page i holds the byte restoredByte(i)
	// throughout, at pages file offset i*hostarch.PageSize.
	ar hostarch.AddrRange

	// loaded receives the result of loading. wantLoadErr is true if the test
	// expects loading to fail.
	loaded      chan error
	wantLoadErr bool
}

// restoreOpts says how restorePages restores pages.
type restoreOpts struct {
	// pages is the number of pages restored.
	pages uint64

	// If dirtyTracked, the MemoryFile tracks dirty pages and the restored
	// pages' pma is armed for it, as a restore that saves incrementally arms
	// them.
	dirtyTracked bool

	// If huge, the restored pages are hugepage-backed, mapped from a
	// hugepage-aligned address by a huge pma.
	huge bool

	// If shared, the restored pages are mapped shared, through a Mappable
	// that holds them, as a restored shared memory segment's or tmpfs file's
	// are; otherwise, a private anonymous mapping's pma holds them.
	shared bool
}

// restoredByte returns the byte that every byte of the restored page i holds:
// never zero, so that every page is saved with its data.
func restoredByte(i uint64) byte {
	return byte(i%255 + 1)
}

// restorePages returns a MemoryManager holding opts.pages pages restored from
// a checkpoint, none of which is loaded yet. The pages file serves reads of
// 1 MiB, two at a time: loading keeps one background read in flight, and
// leaves the other slot to awaited reads.
func restorePages(ctx context.Context, t *testing.T, opts restoreOpts) *restoredPages {
	t.Helper()
	size := opts.pages * hostarch.PageSize

	// Save the pages.
	src := newHugeTestMemoryFile(t, opts.huge)
	fr, err := src.Allocate(size, pgalloc.AllocOpts{Kind: usage.Anonymous, Huge: opts.huge})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	contents := make([]byte, size)
	for i := uint64(0); i < opts.pages; i++ {
		copy(contents[i*hostarch.PageSize:], bytes.Repeat([]byte{restoredByte(i)}, hostarch.PageSize))
	}
	if _, err := unix.Pwrite(src.FD(), contents, int64(fr.Start)); err != nil {
		t.Fatalf("Pwrite: %v", err)
	}
	var metadata, pages bytes.Buffer
	saved := make(chan error, 1)
	apfs, err := pgalloc.StartAsyncPagesFileSave(stateio.NewIOWriter(&pages, 1<<20, 1, 1), func(err error) { saved <- err })
	if err != nil {
		t.Fatalf("StartAsyncPagesFileSave: %v", err)
	}
	if err := src.SaveTo(ctx, &metadata, &pgalloc.SaveOpts{PagesFile: apfs}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	apfs.MemoryFilesDone()
	if err := <-saved; err != nil {
		t.Fatalf("saving pages: %v", err)
	}
	if got := uint64(pages.Len()); got != size {
		t.Fatalf("pages file holds %d bytes, want %d", got, size)
	}

	// Restore them from a pages file whose reads wait for the test.
	rp := &restoredPages{
		pf:     newPagesReader(bytes.NewReader(pages.Bytes())),
		loaded: make(chan error, 1),
	}
	rp.mf = newHugeTestMemoryFile(t, opts.huge)
	// Before the MemoryFile is destroyed, if the test ends early:
	t.Cleanup(rp.letLoad)
	apfl, err := pgalloc.StartAsyncPagesFileLoad(newPagesFile(rp.pf, 2), func(err error) { rp.loaded <- err }, nil /* timeline */, pgalloc.AsyncPagesFileLoadOpts{})
	if err != nil {
		t.Fatalf("StartAsyncPagesFileLoad: %v", err)
	}
	// The records SaveTo wrote are those of an image whose one layer is pages,
	// without page hashes, as a filesystem checkpoint saves them.
	if err := rp.mf.LoadFrom(ctx, &metadata, &pgalloc.LoadOpts{
		Image:      &pgallocpb.ImageProto{Layers: []*pgallocpb.LayerProto{{PagesSize: uint64(pages.Len())}}},
		PagesFiles: []*pgalloc.AsyncPagesFileLoad{apfl},
	}); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	apfl.MemoryFilesDone()
	if opts.dirtyTracked {
		rp.mf.EnableDirtyTracking()
		rp.mf.SwapDirty(true)
	}

	p := platform.FromContext(ctx)
	rp.mm, err = NewMemoryManager(p, rp.mf)
	if err != nil {
		t.Fatalf("NewMemoryManager: %v", err)
	}
	rp.mm.layout = arch.MmapLayout{
		MinAddr:          p.MinUserAddress(),
		MaxAddr:          p.MaxUserAddress(),
		BottomUpBase:     p.MinUserAddress(),
		TopDownBase:      p.MaxUserAddress(),
		DefaultDirection: arch.MmapBottomUp,
	}

	// Map the restored pages from a hugepage-aligned address, at the start of
	// a mapping larger than a huge page, which MMap does not populate.
	mopts := memmap.MMapOpts{
		Length:   max(size, 2*hostarch.HugePageSize),
		Addr:     1 << 30,
		Fixed:    true,
		Private:  !opts.shared,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	}
	if opts.shared {
		// The Mappable takes over the reference that the restore made on
		// the pages.
		m := NewSpecialMappable("restored", rp.mf, fr)
		defer m.DecRef(ctx)
		mopts.Length = size
		mopts.MappingIdentity = m
		mopts.Mappable = m
	}
	addr, err := rp.mm.MMap(ctx, mopts)
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	rp.ar = hostarch.AddrRange{addr, addr + hostarch.Addr(size)}
	if !opts.shared {
		// A single pma holds the reference that the restore made on the
		// pages, as a restored pma does.
		rp.mm.activeMu.Lock()
		restored := pma{
			file:           rp.mf,
			off:            fr.Start,
			translatePerms: hostarch.AnyAccess,
			effectivePerms: hostarch.ReadWrite,
			maxPerms:       hostarch.AnyAccess,
			private:        true,
			huge:           opts.huge,
		}
		restored.armDirtyIfTracked()
		rp.mm.pmas.Insert(rp.mm.pmas.FindGap(rp.ar.Start), rp.ar, restored)
		rp.mm.addRSSLocked(rp.ar)
		rp.mm.activeMu.Unlock()
	}
	rp.as = &loadingAddressSpace{}
	rp.mm.as = rp.as
	if !rp.mf.IsAsyncLoading() {
		t.Fatalf("MemoryFile is not loading")
	}
	return rp
}

// letLoad lets the pages file's reads complete.
func (rp *restoredPages) letLoad() {
	rp.pf.letAll()
}

// finish lets the pages file's reads complete, waits until loading has ended,
// and releases the MemoryManager, whose pages may be released only once their
// reads have completed. It then checks that every page of the MemoryFile is
// released: that every reference taken on the restored pages, the faults'
// among them, has been dropped.
func (rp *restoredPages) finish(ctx context.Context, t *testing.T) {
	t.Helper()
	rp.letLoad()
	select {
	case err := <-rp.loaded:
		if rp.wantLoadErr && err == nil {
			t.Errorf("loading succeeded, want an error")
		} else if !rp.wantLoadErr && err != nil {
			t.Errorf("loading: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for loading to end")
	}
	rp.mm.DecUsers(ctx)
	// Pages whose last reference is dropped are decommitted as they are
	// released, asynchronously.
	for deadline := time.Now().Add(10 * time.Second); ; {
		usage, err := rp.mf.TotalUsage()
		if err != nil {
			t.Fatalf("TotalUsage: %v", err)
		}
		if usage == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d bytes of the MemoryFile are still in use once its MemoryManager is released: a reference on them was not dropped", usage)
		}
		time.Sleep(time.Millisecond)
	}
}

// pagesOf returns the pages file range of the restored pages [i, i+n).
func pagesOf(i, n uint64) memmap.FileRange {
	return memmap.FileRange{i * hostarch.PageSize, (i + n) * hostarch.PageSize}
}

// page returns the address of the restored page i.
func (rp *restoredPages) page(i uint64) hostarch.Addr {
	return rp.ar.Start + hostarch.Addr(i*hostarch.PageSize)
}

// faultResult is the outcome of a fault handled in its own goroutine.
type faultResult struct {
	goid int64
	err  chan error
}

// startFault handles a fault at addr in a goroutine of its own, and waits
// until it waits for pages being loaded.
func startFault(ctx context.Context, t *testing.T, mm *MemoryManager, addr hostarch.Addr, at hostarch.AccessType) *faultResult {
	t.Helper()
	fault := &faultResult{err: make(chan error, 1)}
	started := make(chan int64)
	go func() {
		started <- goid.Get()
		fault.err <- mm.HandleUserFault(ctx, addr, at, 0 /* sp */)
	}()
	fault.goid = <-started
	waitForLoadWaiter(t, fault)
	return fault
}

// Frames of the stacks of goroutines waiting for pages being loaded: every
// such wait, a fault's unlocked wait, and a wait holding activeMu to map the
// pages.
const (
	loadWaitFrame     = "pgalloc.(*asyncMemoryFileLoad).finishAwait"
	unlockedWaitFrame = "pgalloc.(*MemoryFile).AwaitLoad"
	lockedWaitFrame   = "mm.(*MemoryManager).mapASLocked"
)

// waitForLoadWaiter waits until a goroutine waits for pages being loaded,
// failing if fault ends first.
func waitForLoadWaiter(t *testing.T, fault *faultResult) {
	t.Helper()
	waitForStacks(t, fault, 1, loadWaitFrame)
}

// waitForStacks waits until n goroutines have stacks holding every one of
// frames, failing if fault ends first.
func waitForStacks(t *testing.T, fault *faultResult, n int, frames ...string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case err := <-fault.err:
			t.Fatalf("the fault ended before it waited for its pages: %v", err)
		default:
		}
		stacks := buf[:runtime.Stack(buf, true /* all */)]
		found := 0
		for stack := range bytes.SplitSeq(stacks, []byte("\n\n")) {
			if !slices.ContainsFunc(frames, func(frame string) bool { return !bytes.Contains(stack, []byte(frame)) }) {
				found++
			}
		}
		if found >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines wait in %v, want %d", found, frames, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// within runs op, and fails if op does not return within 10 seconds, the
// time in which no test operation waits for a lock that a fault waiting for
// pages holds. It then lets rp's pages load, so that op ends.
func within(t *testing.T, rp *restoredPages, what string, op func()) {
	t.Helper()
	timer := time.AfterFunc(10*time.Second, func() {
		t.Errorf("%s waited for a fault waiting for pages being loaded", what)
		rp.letLoad()
	})
	op()
	timer.Stop()
}

// faultEnds returns the error of fault, which must end within 10 seconds.
func faultEnds(t *testing.T, fault *faultResult) error {
	t.Helper()
	select {
	case err := <-fault.err:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("the fault did not end")
		return nil
	}
}

// readPage returns the byte that every byte of the page at addr holds, failing
// if they differ.
func readPage(ctx context.Context, t *testing.T, mm *MemoryManager, addr hostarch.Addr) byte {
	t.Helper()
	b := make([]byte, hostarch.PageSize)
	if _, err := mm.CopyIn(ctx, addr, b, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyIn(%#x): %v", addr, err)
	}
	if !bytes.Equal(b, bytes.Repeat(b[:1], len(b))) {
		t.Fatalf("page %#x holds differing bytes", addr)
	}
	return b[0]
}

// TestFaultWaitsForLoadUnlocked checks that a fault waiting for its pages to
// be loaded holds none of its MemoryManager's locks: the faults of other
// tasks, the copies of their system calls, and the mappings they make proceed
// meanwhile. Once loaded, the faulting page is mapped with the restored data.
func TestFaultWaitsForLoadUnlocked(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32})
	defer rp.finish(ctx, t)

	fault := startFault(ctx, t, rp.mm, rp.page(20), hostarch.Read)
	within(t, rp, "another task's access to loaded memory", func() {
		other := mmap(t, ctx, rp.mm, 4, true /* private */)
		for end := time.Now().Add(100 * time.Millisecond); time.Now().Before(end); {
			if err := rp.mm.HandleUserFault(ctx, other.Start, hostarch.Write, 0 /* sp */); err != nil {
				t.Fatalf("HandleUserFault: %v", err)
			}
			if _, err := rp.mm.CopyOut(ctx, other.Start+1, []byte{1, 2, 3}, usermem.IOOpts{}); err != nil {
				t.Fatalf("CopyOut: %v", err)
			}
		}
		if err := rp.mm.MUnmap(ctx, other.Start, uint64(other.Length())); err != nil {
			t.Fatalf("MUnmap: %v", err)
		}
	})
	select {
	case err := <-fault.err:
		t.Fatalf("the fault ended before its pages were loaded: %v", err)
	default:
	}

	rp.letLoad()
	if err := faultEnds(t, fault); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if _, ok := rp.as.mappedAs(rp.page(20)); !ok {
		t.Errorf("the faulting page is not mapped")
	}
	if got, want := readPage(ctx, t, rp.mm, rp.page(20)), restoredByte(20); got != want {
		t.Errorf("the faulting page holds %d, want %d", got, want)
	}
}

// TestFaultWaitingForLoadRacesMUnmap checks that unmapping the pages that a
// fault waits for proceeds, that the pages are not released while it waits,
// and that the fault then fails as a fault on unmapped memory does.
func TestFaultWaitingForLoadRacesMUnmap(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32})
	defer rp.finish(ctx, t)

	fault := startFault(ctx, t, rp.mm, rp.page(3), hostarch.Read)
	within(t, rp, "MUnmap", func() {
		if err := rp.mm.MUnmap(ctx, rp.ar.Start, uint64(rp.ar.Length())); err != nil {
			t.Fatalf("MUnmap: %v", err)
		}
	})
	rp.letLoad()
	if err := faultEnds(t, fault); !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("HandleUserFault on unmapped memory: got %v, want EFAULT", err)
	}
	if _, ok := rp.as.mappedAs(rp.page(3)); ok {
		t.Errorf("the unmapped page is mapped")
	}
}

// TestFaultWaitingForLoadRacesMRemap checks that moving the pages that a
// fault waits for proceeds, that the fault then fails as a fault on unmapped
// memory does, and that the pages hold their data at their new address.
func TestFaultWaitingForLoadRacesMRemap(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32})
	defer rp.finish(ctx, t)

	fault := startFault(ctx, t, rp.mm, rp.page(7), hostarch.Write)
	newAddr := rp.ar.Start + 16<<20 // past the mapping
	within(t, rp, "MRemap", func() {
		if _, err := rp.mm.MRemap(ctx, rp.ar.Start, uint64(rp.ar.Length()), uint64(rp.ar.Length()), MRemapOpts{
			Move:    MRemapMustMove,
			NewAddr: newAddr,
		}); err != nil {
			t.Fatalf("MRemap: %v", err)
		}
	})
	rp.letLoad()
	if err := faultEnds(t, fault); !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("HandleUserFault on moved memory: got %v, want EFAULT", err)
	}
	if got, want := readPage(ctx, t, rp.mm, newAddr+7*hostarch.PageSize), restoredByte(7); got != want {
		t.Errorf("the moved page holds %d, want %d", got, want)
	}
}

// TestFaultWaitingForLoadRacesFork checks that a fork proceeds while a write
// fault waits for its pages, and that the pages are then copied on write: the
// fault's task writes to its own copy, and the child keeps the restored data.
func TestFaultWaitingForLoadRacesFork(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32})
	defer rp.finish(ctx, t)

	fault := startFault(ctx, t, rp.mm, rp.page(5), hostarch.Write)
	var child *MemoryManager
	within(t, rp, "Fork", func() {
		var err error
		if child, err = rp.mm.Fork(ctx); err != nil {
			t.Fatalf("Fork: %v", err)
		}
	})
	defer child.DecUsers(ctx)
	rp.letLoad()
	if err := faultEnds(t, fault); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if at, ok := rp.as.mappedAs(rp.page(5)); !ok || !at.Write {
		t.Errorf("the page written is mapped %v (%t), want writable", at, ok)
	}
	if _, err := rp.mm.CopyOut(ctx, rp.page(5), bytes.Repeat([]byte{99}, hostarch.PageSize), usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	if got, want := readPage(ctx, t, rp.mm, rp.page(5)), byte(99); got != want {
		t.Errorf("the parent's page holds %d, want %d", got, want)
	}
	if got, want := readPage(ctx, t, child, rp.page(5)), restoredByte(5); got != want {
		t.Errorf("the child's page holds %d, want %d", got, want)
	}
}

// TestFaultWaitingForLoadIsKillable checks that a fault waiting for its pages
// ends with EINTR once its task is killed, as its wait is cut short.
func TestFaultWaitingForLoadIsKillable(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32})
	defer rp.finish(ctx, t)

	fault := startFault(ctx, t, rp.mm, rp.page(9), hostarch.Read)
	pgalloc.CancelAwaits(fault.goid)
	defer pgalloc.ForgetCanceledAwaits(fault.goid)
	if err := faultEnds(t, fault); err != linuxerr.EINTR {
		t.Errorf("HandleUserFault of a killed task: got %v, want EINTR", err)
	}
	if _, ok := rp.as.mappedAs(rp.page(9)); ok {
		t.Errorf("the page of the killed task's fault is mapped")
	}
}

// TestFaultWaitingForLoadDirtyTracking checks that dirty tracking can be armed
// again while a write fault waits for its pages, and that the write is then
// recorded in the dirty set of the epoch that the re-arming began, so that an
// incremental checkpoint saves it.
func TestFaultWaitingForLoadDirtyTracking(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32, dirtyTracked: true})
	defer rp.finish(ctx, t)

	fault := startFault(ctx, t, rp.mm, rp.page(17), hostarch.Write)
	within(t, rp, "re-arming dirty tracking", func() {
		rp.mm.mf.SwapDirty(true)
		rp.mm.ArmDirtyTracking(hostarch.PageSize)
	})
	rp.letLoad()
	if err := faultEnds(t, fault); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if at, ok := rp.as.mappedAs(rp.page(17)); !ok || !at.Write {
		t.Errorf("the page written is mapped %v (%t), want writable", at, ok)
	}
	if got, want := dirtyPages(t, rp.mm, rp.ar), []uint64{17}; !slices.Equal(got, want) {
		t.Errorf("dirty pages in the epoch the re-arming began: got %v, want %v", got, want)
	}
}

// TestFaultWaitingForLoadRetriesBounded checks that a fault whose pages are
// replaced by others not loaded yet each time it has waited for them stops
// waiting unlocked after maxFaultLoadWaits waits, and waits for the last
// pages holding activeMu, as other accesses to pages being loaded do, so that
// it ends. Here, another task moves a range of pages not loaded yet onto the
// faulting page as the fault waits.
func TestFaultWaitingForLoadRetriesBounded(t *testing.T) {
	ctx := contexttest.Context(t)
	// Units of pages that a fault maps while they load, past the first MiB,
	// whose background read is not let complete.
	const unitPages = asyncLoadingMapUnit / hostarch.PageSize
	rp := restorePages(ctx, t, restoreOpts{pages: 256 + (maxFaultLoadWaits+1)*unitPages})
	defer rp.finish(ctx, t)
	unit := func(k uint64) uint64 { return 256 + k*unitPages }
	addr := rp.page(unit(0))

	fault := startFault(ctx, t, rp.mm, addr, hostarch.Read)
	for k := uint64(0); k < maxFaultLoadWaits; k++ {
		waitForStacks(t, fault, 1, loadWaitFrame, unlockedWaitFrame)
		rp.pf.waitStarted(t, pagesOf(unit(k), unitPages))
		// Move unit k+1 onto the faulting page while the fault waits for
		// unit k.
		within(t, rp, "MRemap", func() {
			if _, err := rp.mm.MRemap(ctx, rp.page(unit(k+1)), asyncLoadingMapUnit, asyncLoadingMapUnit, MRemapOpts{
				Move:    MRemapMustMove,
				NewAddr: addr,
			}); err != nil {
				t.Fatalf("MRemap: %v", err)
			}
		})
		// Unit k loads; the fault finds unit k+1 at its page.
		rp.pf.letRange(pagesOf(unit(k), unitPages))
	}
	// The fault waited unlocked maxFaultLoadWaits times: it now waits for the
	// last unit holding activeMu.
	rp.pf.waitStarted(t, pagesOf(unit(maxFaultLoadWaits), unitPages))
	waitForStacks(t, fault, 1, loadWaitFrame, lockedWaitFrame)
	rp.pf.letRange(pagesOf(unit(maxFaultLoadWaits), unitPages))
	if err := faultEnds(t, fault); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if _, ok := rp.as.mappedAs(addr); !ok {
		t.Errorf("the faulting page is not mapped")
	}
	if got, want := readPage(ctx, t, rp.mm, addr), restoredByte(unit(maxFaultLoadWaits)); got != want {
		t.Errorf("the faulting page holds %d, want %d", got, want)
	}
}

// TestFaultWaitingForLoadFails checks that a fault whose pages fail to load,
// with an error other than its task being killed, handles the fault as it
// did before it waited unlocked: mapping the pages fails with the error.
func TestFaultWaitingForLoadFails(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 256 + 32})
	rp.wantLoadErr = true
	defer rp.finish(ctx, t)

	// The awaited read of the fault's pages, past the first MiB, whose
	// background read waits, fails.
	fault := startFault(ctx, t, rp.mm, rp.page(256+3), hostarch.Read)
	unit := pagesOf(256, asyncLoadingMapUnit/hostarch.PageSize)
	rp.pf.waitStarted(t, unit)
	rp.pf.failRange(unit)
	if err := faultEnds(t, fault); !linuxerr.Equals(linuxerr.EIO, err) {
		t.Errorf("HandleUserFault on pages that failed to load: got %v, want EIO", err)
	}
	if _, ok := rp.as.mappedAs(rp.page(256 + 3)); ok {
		t.Errorf("the page that failed to load is mapped")
	}
}

// TestFaultOfKilledTaskWaitsForNoLoad checks that the fault of a task killed
// before it faults does not wait for its pages: it ends at once with EINTR.
func TestFaultOfKilledTaskWaitsForNoLoad(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32})
	defer rp.finish(ctx, t)

	fault := &faultResult{err: make(chan error, 1)}
	started := make(chan int64)
	killed := make(chan struct{})
	go func() {
		started <- goid.Get()
		<-killed
		fault.err <- rp.mm.HandleUserFault(ctx, rp.page(9), hostarch.Read, 0 /* sp */)
	}()
	fault.goid = <-started
	pgalloc.CancelAwaits(fault.goid)
	defer pgalloc.ForgetCanceledAwaits(fault.goid)
	close(killed)
	if err := faultEnds(t, fault); err != linuxerr.EINTR {
		t.Errorf("HandleUserFault of a killed task: got %v, want EINTR", err)
	}
	if _, ok := rp.as.mappedAs(rp.page(9)); ok {
		t.Errorf("the page of the killed task's fault is mapped")
	}
}

// TestFaultsWaitingForSameLoad checks that two faults on pages that load
// together both wait for them unlocked, and both map them once loaded.
func TestFaultsWaitingForSameLoad(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32})
	defer rp.finish(ctx, t)

	first := startFault(ctx, t, rp.mm, rp.page(20), hostarch.Read)
	second := startFault(ctx, t, rp.mm, rp.page(21), hostarch.Write)
	waitForStacks(t, second, 2, loadWaitFrame, unlockedWaitFrame)
	rp.letLoad()
	for i, fault := range []*faultResult{first, second} {
		if err := faultEnds(t, fault); err != nil {
			t.Errorf("HandleUserFault %d: %v", i, err)
		}
	}
	for _, i := range []uint64{20, 21} {
		if _, ok := rp.as.mappedAs(rp.page(i)); !ok {
			t.Errorf("faulting page %d is not mapped", i)
		}
		if got, want := readPage(ctx, t, rp.mm, rp.page(i)), restoredByte(i); got != want {
			t.Errorf("page %d holds %d, want %d", i, got, want)
		}
	}
}

// TestFaultWaitsForLoadUnlockedHuge checks that a fault on a huge pma waits
// unlocked for the huge page it maps while the pages load, and maps it once
// loaded.
func TestFaultWaitsForLoadUnlockedHuge(t *testing.T) {
	ctx := contexttest.Context(t)
	const hugePages = hostarch.HugePageSize / hostarch.PageSize
	rp := restorePages(ctx, t, restoreOpts{pages: 2 * hugePages, huge: true})
	defer rp.finish(ctx, t)

	// A page of the second huge page, past the background read of the
	// first MiB.
	page := uint64(hugePages + 7)
	fault := startFault(ctx, t, rp.mm, rp.page(page), hostarch.Write)
	within(t, rp, "another task's fault and copy", func() {
		other := mmap(t, ctx, rp.mm, 4, true /* private */)
		if err := rp.mm.HandleUserFault(ctx, other.Start, hostarch.Write, 0 /* sp */); err != nil {
			t.Fatalf("HandleUserFault: %v", err)
		}
		if _, err := rp.mm.CopyOut(ctx, other.Start+1, []byte{1, 2, 3}, usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyOut: %v", err)
		}
		if err := rp.mm.MUnmap(ctx, other.Start, uint64(other.Length())); err != nil {
			t.Fatalf("MUnmap: %v", err)
		}
	})
	rp.letLoad()
	if err := faultEnds(t, fault); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	// The huge page is mapped whole.
	for _, i := range []uint64{hugePages, page, 2*hugePages - 1} {
		if _, ok := rp.as.mappedAs(rp.page(i)); !ok {
			t.Errorf("page %d of the faulting huge page is not mapped", i)
		}
	}
	if got, want := readPage(ctx, t, rp.mm, rp.page(page)), restoredByte(page); got != want {
		t.Errorf("the faulting page holds %d, want %d", got, want)
	}
}

// TestFaultWaitsForLoadUnlockedShared checks that a fault on a shared mapping
// of restored pages, whose pma's pages its Mappable holds, waits for them
// unlocked, and maps them once loaded.
func TestFaultWaitsForLoadUnlockedShared(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32, shared: true})
	defer rp.finish(ctx, t)

	fault := startFault(ctx, t, rp.mm, rp.page(12), hostarch.Write)
	within(t, rp, "another task's fault on the shared mapping's loaded pages", func() {
		other := mmap(t, ctx, rp.mm, 4, true /* private */)
		if err := rp.mm.HandleUserFault(ctx, other.Start, hostarch.Write, 0 /* sp */); err != nil {
			t.Fatalf("HandleUserFault: %v", err)
		}
		if err := rp.mm.MUnmap(ctx, other.Start, uint64(other.Length())); err != nil {
			t.Fatalf("MUnmap: %v", err)
		}
	})
	rp.letLoad()
	if err := faultEnds(t, fault); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if at, ok := rp.as.mappedAs(rp.page(12)); !ok || !at.Write {
		t.Errorf("the page written is mapped %v (%t), want writable", at, ok)
	}
	if got, want := readPage(ctx, t, rp.mm, rp.page(12)), restoredByte(12); got != want {
		t.Errorf("the faulting page holds %d, want %d", got, want)
	}
}

// TestFaultWaitingForLoadWokenByDecommit checks that a fault waiting unlocked
// for pages whose read has not started is woken once MADV_DONTNEED has
// decommitted all of them in place, before any read of them, and maps them
// reading as zeros: here, the pages of a huge page, which two MADV_DONTNEEDs
// of parts of it decommit, while another fault's awaited read and a
// background read, which wait, take both of the pages file's slots.
func TestFaultWaitingForLoadWokenByDecommit(t *testing.T) {
	ctx := contexttest.Context(t)
	const hugePages = hostarch.HugePageSize / hostarch.PageSize
	rp := restorePages(ctx, t, restoreOpts{pages: 2 * hugePages, huge: true})
	defer rp.finish(ctx, t)

	// A fault on the first huge page waits for the background read of its
	// first MiB and the awaited read of its second.
	first := startFault(ctx, t, rp.mm, rp.page(hugePages/2+3), hostarch.Read)
	rp.pf.waitStarted(t, pagesOf(hugePages/2, hugePages/2))
	// A fault on the second huge page waits for reads that cannot start.
	page := uint64(hugePages + 7)
	second := startFault(ctx, t, rp.mm, rp.page(page), hostarch.Write)
	waitForStacks(t, second, 2, loadWaitFrame, unlockedWaitFrame)
	secondPages := pagesOf(hugePages, hugePages)
	readOf := func(fr memmap.FileRange) bool {
		rp.pf.mu.Lock()
		defer rp.pf.mu.Unlock()
		return slices.ContainsFunc(rp.pf.started, fr.Overlaps)
	}
	if readOf(secondPages) {
		t.Fatalf("a read of the second huge page began: the test needs none to")
	}

	// MADV_DONTNEED of all of the second huge page but its first page, then of
	// its first page: each decommits part of the huge page in place.
	within(t, rp, "MADV_DONTNEED", func() {
		if err := rp.mm.Decommit(rp.page(hugePages+1), (hugePages-1)*hostarch.PageSize); err != nil {
			t.Fatalf("Decommit(pages %d-%d): %v", hugePages+1, 2*hugePages-1, err)
		}
		if err := rp.mm.Decommit(rp.page(hugePages), hostarch.PageSize); err != nil {
			t.Fatalf("Decommit(page %d): %v", hugePages, err)
		}
	})
	// The second fault needs no read anymore: it ends while the first one's
	// reads still wait.
	if err := faultEnds(t, second); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if at, ok := rp.as.mappedAs(rp.page(page)); !ok || !at.Write {
		t.Errorf("the page written is mapped %v (%t), want writable", at, ok)
	}
	select {
	case err := <-first.err:
		t.Fatalf("the first fault ended before its pages were loaded: %v", err)
	default:
	}

	// The copies below read through the internal mappings of the whole pma,
	// which wait for the first huge page to be loaded.
	rp.letLoad()
	if err := faultEnds(t, first); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if got, want := readPage(ctx, t, rp.mm, rp.page(hugePages/2+3)), restoredByte(hugePages/2+3); got != want {
		t.Errorf("the first fault's page holds %d, want %d", got, want)
	}
	for _, i := range []uint64{hugePages, page, 2*hugePages - 1} {
		if got := readPage(ctx, t, rp.mm, rp.page(i)); got != 0 {
			t.Errorf("decommitted page %d holds %d, want 0", i, got)
		}
	}
	if readOf(secondPages) {
		t.Errorf("the decommitted pages were read")
	}
}

// pageFileRange returns the MemoryFile range of the page at addr, which a pma
// maps.
func pageFileRange(t *testing.T, mm *MemoryManager, addr hostarch.Addr) memmap.FileRange {
	t.Helper()
	mm.activeMu.RLock()
	defer mm.activeMu.RUnlock()
	pseg := mm.pmas.FindSegment(addr)
	if !pseg.Ok() {
		t.Fatalf("no pma maps %#x", addr)
	}
	return pseg.fileRangeOf(hostarch.AddrRange{addr, addr + hostarch.PageSize})
}

// TestFaultWaitingForLoadHoldsNoReference checks that a fault waiting for its
// pages holds no reference on them: the pma that maps them holds the only
// one, so that another task's write fault on them takes them over rather
// than copying them, as it does once the wait has ended.
func TestFaultWaitingForLoadHoldsNoReference(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 256 + 64})
	defer rp.finish(ctx, t)

	// A fault past the first MiB, whose background read waits, takes the
	// pages file's other slot with its awaited read, which holds references
	// on the pages it reads. A second fault's pages, past the first's, wait
	// for a read that cannot start.
	first := startFault(ctx, t, rp.mm, rp.page(256+3), hostarch.Read)
	rp.pf.waitStarted(t, pagesOf(256, asyncLoadingMapUnit/hostarch.PageSize))
	page := uint64(256 + 40)
	second := startFault(ctx, t, rp.mm, rp.page(page), hostarch.Read)
	waitForStacks(t, second, 2, loadWaitFrame)
	if fr := pageFileRange(t, rp.mm, rp.page(page)); !rp.mf.HasUniqueRef(fr) {
		t.Errorf("pages %v have another reference than their pma's while a fault waits for them", fr)
	}
	rp.letLoad()
	for i, fault := range []*faultResult{first, second} {
		if err := faultEnds(t, fault); err != nil {
			t.Errorf("HandleUserFault %d: %v", i, err)
		}
	}
	if got, want := readPage(ctx, t, rp.mm, rp.page(page)), restoredByte(page); got != want {
		t.Errorf("the faulting page holds %d, want %d", got, want)
	}
}

// TestFaultWaitingForLoadOfFreedPages checks that a fault waiting for pages
// whose read has not started ends once the pages are freed, as unmapping
// them frees them, rather than once they would have loaded: it then fails as
// a fault on unmapped memory does.
func TestFaultWaitingForLoadOfFreedPages(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 256 + 64})
	defer rp.finish(ctx, t)

	// As in TestFaultWaitingForLoadHoldsNoReference, the second fault's
	// pages wait for a read that cannot start.
	first := startFault(ctx, t, rp.mm, rp.page(256+3), hostarch.Read)
	rp.pf.waitStarted(t, pagesOf(256, asyncLoadingMapUnit/hostarch.PageSize))
	page := uint64(256 + 40)
	second := startFault(ctx, t, rp.mm, rp.page(page), hostarch.Read)
	waitForStacks(t, second, 2, loadWaitFrame)
	unit := hostarch.Addr(asyncLoadingMapUnit)
	start := rp.page(page) &^ (unit - 1)
	within(t, rp, "MUnmap", func() {
		if err := rp.mm.MUnmap(ctx, start, uint64(unit)); err != nil {
			t.Fatalf("MUnmap: %v", err)
		}
	})
	if err := faultEnds(t, second); !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("HandleUserFault on freed memory: got %v, want EFAULT", err)
	}
	rp.letLoad()
	if err := faultEnds(t, first); err != nil {
		t.Errorf("HandleUserFault: %v", err)
	}
}

// TestWriteFaultWaitingForLoadLeavesPageClean checks that a write fault
// waiting for its pages, which dirty tracking write-protects, leaves them
// write-protected and clean until they are loaded: a save meanwhile need not
// wait for them, as it waits for the dirty pages still loading that it reads.
// Once loaded, the write fault makes the page writable and dirty.
func TestWriteFaultWaitingForLoadLeavesPageClean(t *testing.T) {
	ctx := contexttest.Context(t)
	rp := restorePages(ctx, t, restoreOpts{pages: 32, dirtyTracked: true})
	defer rp.finish(ctx, t)
	rp.mm.ArmDirtyTracking(hostarch.PageSize)

	fault := startFault(ctx, t, rp.mm, rp.page(17), hostarch.Write)
	if n := rp.mf.DirtyBytes(); n != 0 {
		t.Errorf("%d bytes are dirty while a write fault waits for its pages, want 0", n)
	}
	rp.letLoad()
	if err := faultEnds(t, fault); err != nil {
		t.Fatalf("HandleUserFault: %v", err)
	}
	if at, ok := rp.as.mappedAs(rp.page(17)); !ok || !at.Write {
		t.Errorf("the page written is mapped %v (%t), want writable", at, ok)
	}
	if got, want := dirtyPages(t, rp.mm, rp.ar), []uint64{17}; !slices.Equal(got, want) {
		t.Errorf("dirty pages: got %v, want %v", got, want)
	}
}
