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
	"hash/maphash"
	"math/bits"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// Dirty tracking.
//
// A MemoryFile can record which of its pages are written ("dirtied") from a
// point in time on, so that a checkpoint can save only the pages that changed
// since the image the MemoryFile was last saved to or loaded from, or copy
// pages while tasks run and then only the pages dirtied meanwhile. Tracking is
// enabled by EnableDirtyTracking; the set of pages dirtied since tracking
// started, or since the last call to SwapDirty, is returned and cleared by
// SwapDirty.
//
// Writes reach a MemoryFile's pages by several paths. The MemoryFile itself
// marks the pages written by those it sees:
//
//   - MapInternal with write access: the Sentry's own writes, e.g.
//     copy-on-write copies, page cache fills, tmpfs file data and the VDSO
//     parameter page. The mark precedes the write, and callers may write
//     through the returned mappings after the dirty set is swapped; see
//     SwapDirty.
//
//   - Decommit and release, which make page contents zero without any write
//     (fallocate(FALLOC_FL_PUNCH_HOLE)), and the zeroing of recycled pages.
//
//   - Long-lived writable internal mappings, whose writers do not call
//     MapInternal for each write: their owners register them with
//     MarkAlwaysDirty, and they are dirty at every swap. io_uring's rings
//     are the only such mappings: the Sentry writes completions through
//     mappings it obtained once. Other shared rings need no registration:
//     packet-mmap rings (socket/netstack/packetmmap) and kcov's coverage
//     area call MapInternal for each access, and the Sentry never writes to
//     an AIO ring (mm.aioMappable): io_getevents(2) copies events out
//     through the MemoryManager like any other write to application memory.
//
// Other writers call MarkDirty: users of the backing file's FD (DataFD) that
// write through it, and dirty sources, which report application stores
// through the platform's mappings of the MemoryFile, which the MemoryFile
// cannot see. With write tracking of internal mappings (write_tracking.go),
// a dirty source also reports the writes through the MemoryFile's internal
// mappings, including those through MapInternalUntracked's.
//
// Async page loading does not mark pages: it restores the contents of the
// image being loaded, which is the image the next save is relative to. Its
// counters of the time spent waiting for pages are metrics in the Sentry's
// own memory, not MemoryFile pages, so they need no tracking either.
//
// Each marking path can be disabled by TestOnlyDisableDirtyMarkPath, so that
// tests check that verification catches the writes it marks (see "Negative
// controls" below).

// dirtyChunkWords is the number of words in the dirty bitmap of a chunk.
const dirtyChunkWords = chunkSize / hostarch.PageSize / 64

// dirtyChunk is the dirty bitmap of a chunk: bit i%64 of word i/64 is set if
// page i of the chunk is dirty.
type dirtyChunk [dirtyChunkWords]atomic.Uint64

// dirtyBitmap is a bitmap with one bit per page of a MemoryFile, which can be
// marked without locking.
type dirtyBitmap struct {
	// chunks is indexed by chunk. chunks is protected by MemoryFile.mu for
	// writing; it is grown before the chunks it covers become usable, so marks
	// never race with growth. chunks slices are immutable.
	chunks atomic.Pointer[[]*dirtyChunk]
}

func (b *dirtyBitmap) load() []*dirtyChunk {
	if p := b.chunks.Load(); p != nil {
		return *p
	}
	return nil
}

// Preconditions: MemoryFile.mu must be locked.
func (b *dirtyBitmap) growLocked(nrChunks int) {
	old := b.load()
	if len(old) >= nrChunks {
		return
	}
	chunks := make([]*dirtyChunk, nrChunks)
	copy(chunks, old)
	for i := len(old); i < nrChunks; i++ {
		chunks[i] = new(dirtyChunk)
	}
	b.chunks.Store(&chunks)
}

// mark sets the bits of all pages overlapping fr.
func (b *dirtyBitmap) mark(fr memmap.FileRange) {
	chunks := b.load()
	page := fr.Start / hostarch.PageSize
	end := (fr.End + hostarch.PageSize - 1) / hostarch.PageSize
	if end > uint64(len(chunks))*dirtyChunkWords*64 {
		panic(fmt.Sprintf("dirty pages %v beyond the MemoryFile's %d chunks", fr, len(chunks)))
	}
	for page < end {
		word := page / 64
		bit := page % 64
		mask := ^uint64(0) << bit
		if n := end - page; n < 64-bit {
			mask &= ^uint64(0) >> (64 - bit - n)
		}
		chunks[word/dirtyChunkWords][word%dirtyChunkWords].Or(mask)
		page += 64 - bit
	}
}

// DirtySet is a set of pages of a MemoryFile, returned by SwapDirty.
type DirtySet struct {
	// words has one bit per page: page i is in the set if bit i%64 of
	// words[i/64] is set.
	words []uint64
}

// Contains returns true if the page containing off is in s.
func (s *DirtySet) Contains(off uint64) bool {
	page := off / hostarch.PageSize
	return page/64 < uint64(len(s.words)) && s.words[page/64]&(1<<(page%64)) != 0
}

// Bytes returns the number of bytes in s.
func (s *DirtySet) Bytes() uint64 {
	var n uint64
	for _, w := range s.words {
		n += uint64(bits.OnesCount64(w))
	}
	return n * hostarch.PageSize
}

// ForEachRange calls fn on each maximal range of pages in s that overlaps fr,
// intersected with fr, in increasing order, until fn returns false.
func (s *DirtySet) ForEachRange(fr memmap.FileRange, fn func(memmap.FileRange) bool) {
	limit := min((fr.End+hostarch.PageSize-1)/hostarch.PageSize, uint64(len(s.words))*64)
	page := fr.Start / hostarch.PageSize
	for {
		start := s.next(page, limit, true)
		if start == limit {
			return
		}
		page = s.next(start, limit, false)
		if !fn(memmap.FileRange{start * hostarch.PageSize, page * hostarch.PageSize}.Intersect(fr)) {
			return
		}
	}
}

// next returns the first page at or after page, and before limit, that is in
// s if in is true or not in s otherwise; or limit if there is none.
func (s *DirtySet) next(page, limit uint64, in bool) uint64 {
	for page < limit {
		w := s.words[page/64]
		if !in {
			w = ^w
		}
		w &^= 1<<(page%64) - 1
		if w != 0 {
			return min(page&^63+uint64(bits.TrailingZeros64(w)), limit)
		}
		page = page&^63 + 64
	}
	return limit
}

// Union adds every page in o to s.
func (s *DirtySet) Union(o *DirtySet) {
	if len(o.words) > len(s.words) {
		s.words = append(s.words, make([]uint64, len(o.words)-len(s.words))...)
	}
	for i, w := range o.words {
		s.words[i] |= w
	}
}

// add adds every page in fr to s.
func (s *DirtySet) add(fr memmap.FileRange) {
	for page := fr.Start / hostarch.PageSize; page*hostarch.PageSize < fr.End; page++ {
		s.words[page/64] |= 1 << (page % 64)
	}
}

// dirtyState is the dirty tracking state of a MemoryFile.
type dirtyState struct {
	// tracked is true if dirty tracking is enabled. tracked is set once, with
	// MemoryFile.mu locked, after written and internal are grown.
	tracked atomic.Bool

	// written marks pages dirtied since the last swap.
	written dirtyBitmap

	// internal marks pages dirtied by MapInternal(Write) since the last swap;
	// see SwapDirty.
	internal dirtyBitmap

	// If writesArmed is true, ArmInternalWrites was called: the internal
	// mappings of the first armedChunks chunks are write-protected for
	// write tracking (write_tracking.go), and chunks added later are
	// write-protected as they are mapped. writesArmed and armedChunks are
	// protected by MemoryFile.mu.
	writesArmed bool
	armedChunks int

	// swapMu serializes swaps and protects the fields below. swapMu is
	// ordered after MemoryFile.mu.
	swapMu dirtyMutex

	// carried marks pages to be reported dirty by the next swap: those that
	// MapInternal(Write) marked before the last swap, if writers could still
	// write through their mappings after it.
	//
	// +checklocks:swapMu
	carried []uint64

	// always are the ranges registered by MarkAlwaysDirty, with repetition.
	//
	// +checklocks:swapMu
	always []memmap.FileRange

	// hashes holds a hash of every page at the start of the current epoch,
	// for VerifyDirty; see RecordPageHashes.
	//
	// +checklocks:swapMu
	hashes *pageHashes
}

// EnableDirtyTracking starts tracking the pages of f that are dirtied. The
// first SwapDirty returns the pages dirtied since tracking started. Calls
// after the first have no effect.
func (f *MemoryFile) EnableDirtyTracking() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirty.tracked.Load() {
		return
	}
	f.dirty.growLocked(len(f.chunksLoad()))
	f.dirty.tracked.Store(true)
}

// growLocked grows the dirty bitmaps to cover nrChunks chunks. It must be
// called before chunks become usable.
//
// Preconditions: MemoryFile.mu must be locked.
func (s *dirtyState) growLocked(nrChunks int) {
	s.written.growLocked(nrChunks)
	s.internal.growLocked(nrChunks)
}

// DirtyTracked returns true if dirty tracking is enabled for f.
func (f *MemoryFile) DirtyTracked() bool {
	return f.dirty.tracked.Load()
}

// MarkDirty records that the contents of pages overlapping fr changed, or are
// about to change. It has no effect if f is not tracked.
//
// Preconditions: fr must be within an allocated range of f.
func (f *MemoryFile) MarkDirty(fr memmap.FileRange) {
	if !f.dirty.tracked.Load() {
		return
	}
	f.dirty.written.mark(fr)
}

// MarkDirtyBy is MarkDirty for a mark made by path p, which tests may disable
// (see TestOnlyDisableDirtyMarkPath).
func (f *MemoryFile) MarkDirtyBy(p DirtyMarkPath, fr memmap.FileRange) {
	if !f.dirty.tracked.Load() || !DirtyMarkPathEnabled(p) {
		return
	}
	f.dirty.written.mark(fr)
}

// markDirtyInternal records that the caller of MapInternal may write the
// pages overlapping fr through the mappings it returns.
func (f *MemoryFile) markDirtyInternal(fr memmap.FileRange) {
	if !f.dirty.tracked.Load() || !DirtyMarkPathEnabled(DirtyMarkMapInternal) {
		return
	}
	f.dirty.written.mark(fr)
	f.dirty.internal.mark(fr)
}

// MarkAlwaysDirty registers fr as written through a long-lived internal
// mapping, so that its pages are dirty at every swap until a matching call to
// ClearAlwaysDirty. It is used by writers that keep the mappings returned by
// MapInternal and write through them without calling MapInternal again.
// Ranges are counted: each call must be matched by a call to
// ClearAlwaysDirty with the same range.
func (f *MemoryFile) MarkAlwaysDirty(fr memmap.FileRange) {
	f.dirty.swapMu.Lock()
	defer f.dirty.swapMu.Unlock()
	f.dirty.always = append(f.dirty.always, fr)
}

// ClearAlwaysDirty unregisters a range registered by MarkAlwaysDirty. Its
// pages are dirty at the next swap.
//
// Preconditions: fr was registered by MarkAlwaysDirty and not yet
// unregistered.
func (f *MemoryFile) ClearAlwaysDirty(fr memmap.FileRange) {
	f.dirty.swapMu.Lock()
	defer f.dirty.swapMu.Unlock()
	for i, always := range f.dirty.always {
		if always == fr {
			f.dirty.always = append(f.dirty.always[:i], f.dirty.always[i+1:]...)
			f.MarkDirty(fr)
			return
		}
	}
	panic(fmt.Sprintf("ClearAlwaysDirty(%v): range not registered by MarkAlwaysDirty", fr))
}

// SwapDirty returns the pages of f dirtied since tracking was enabled or since
// the previous call to SwapDirty, and starts recording anew.
//
// The returned set always includes the ranges registered by MarkAlwaysDirty.
// If paused is false, writers may run concurrently with SwapDirty and with
// the caller's use of the returned set: callers of MapInternal(Write) may then
// write through their mappings after their pages were marked and swapped out,
// so the pages they marked before the swap are also reported by the next
// swap. If paused is true, the caller guarantees that no writer runs from
// before SwapDirty until it resumes writers.
//
// Preconditions: Dirty tracking must be enabled.
func (f *MemoryFile) SwapDirty(paused bool) *DirtySet {
	if !f.dirty.tracked.Load() {
		panic(fmt.Sprintf("MemoryFile(%p).SwapDirty() called without dirty tracking", f))
	}
	f.dirty.swapMu.Lock()
	defer f.dirty.swapMu.Unlock()
	written := f.dirty.written.load()
	internal := f.dirty.internal.load()
	s := &DirtySet{words: make([]uint64, len(written)*dirtyChunkWords)}
	var carried []uint64
	if !paused {
		carried = make([]uint64, len(s.words))
	}
	for c, chunk := range written {
		for i := range chunk {
			word := c*dirtyChunkWords + i
			s.words[word] = chunk[i].Swap(0)
			if in := internal[c][i].Swap(0); carried != nil {
				carried[word] = in
			}
			if word < len(f.dirty.carried) {
				s.words[word] |= f.dirty.carried[word]
			}
		}
	}
	f.dirty.carried = carried
	for _, fr := range f.dirty.always {
		s.add(fr)
	}
	return s
}

// DirtyBytes returns the number of bytes in the pages that the next call to
// SwapDirty would return if it were called now.
//
// Preconditions: Dirty tracking must be enabled.
func (f *MemoryFile) DirtyBytes() uint64 {
	f.dirty.swapMu.Lock()
	defer f.dirty.swapMu.Unlock()
	written := f.dirty.written.load()
	s := &DirtySet{words: make([]uint64, len(written)*dirtyChunkWords)}
	for c, chunk := range written {
		for i := range chunk {
			s.words[c*dirtyChunkWords+i] = chunk[i].Load()
		}
	}
	s.Union(&DirtySet{words: f.dirty.carried})
	for _, fr := range f.dirty.always {
		s.add(fr)
	}
	return s.Bytes()
}

// UnswapDirty returns the pages in s, which a call to SwapDirty returned, to
// f's dirty set, so that the next swap reports them again. It is used when
// the save that consumed s failed, so that no dirty page is lost.
func (f *MemoryFile) UnswapDirty(s *DirtySet) {
	written := f.dirty.written.load()
	for word, w := range s.words {
		if w != 0 {
			written[word/dirtyChunkWords][word%dirtyChunkWords].Or(w)
		}
	}
}

// Dirty tracking verification.
//
// Verification checks that dirty tracking is complete: a page whose contents
// changed since the start of an epoch must be in the dirty set swapped at its
// end. RecordPageHashes hashes every page at the start of an epoch;
// VerifyDirty hashes every page again at its end, reports pages that changed
// without being dirty ("escapes"), and records the new hashes for the next
// epoch. It reads every committed page, so it is a debugging and testing aid.

// pageHashes holds a hash of each allocated page of a MemoryFile. Pages
// without a hash are zero pages.
type pageHashes struct {
	// chunks is indexed by chunk, then by page in the chunk. A nil chunk has
	// only zero pages.
	chunks [][]uint64
}

// pageHashSeed seeds page hashes. Hashes never leave the process, so they
// need not be stable across processes.
var pageHashSeed = maphash.MakeSeed()

// zeroPageHash is the hash of a zero page.
var zeroPageHash = maphash.Bytes(pageHashSeed, make([]byte, hostarch.PageSize))

func (h *pageHashes) get(off uint64) uint64 {
	c := off / chunkSize
	if c >= uint64(len(h.chunks)) || h.chunks[c] == nil {
		return zeroPageHash
	}
	return h.chunks[c][(off%chunkSize)/hostarch.PageSize]
}

func (h *pageHashes) set(off, hash uint64) {
	c := off / chunkSize
	if h.chunks[c] == nil {
		if hash == zeroPageHash {
			return
		}
		h.chunks[c] = make([]uint64, chunkSize/hostarch.PageSize)
		for i := range h.chunks[c] {
			h.chunks[c][i] = zeroPageHash
		}
	}
	h.chunks[c][(off%chunkSize)/hostarch.PageSize] = hash
}

// hashPagesLocked returns the hashes of every allocated page of f that may
// hold data: its known-committed pages and the data ranges of the backing
// file in its other allocated ranges, so that holes are not read (and
// committed); other pages are zero.
//
// Preconditions:
//   - f.mu must be locked.
//   - No page of f is being loaded asynchronously.
func (f *MemoryFile) hashPagesLocked() (*pageHashes, error) {
	h := &pageHashes{chunks: make([][]uint64, len(f.chunksLoad()))}
	hash := func(fr memmap.FileRange) {
		off := fr.Start
		f.forEachMappingSlice(fr, func(bs []byte) {
			for i := 0; i < len(bs); i += hostarch.PageSize {
				h.set(off, maphash.Bytes(pageHashSeed, bs[i:i+hostarch.PageSize]))
				off += hostarch.PageSize
			}
		})
	}
	seeker := f.newHostFileDataSeeker()
	for seg := f.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		ma := seg.ValuePtr()
		if ma.wasteOrReleasing {
			continue
		}
		fr := seg.Range()
		if ma.knownCommitted || seeker == nil {
			hash(fr)
			continue
		}
		for fr.Length() != 0 {
			data, err := seeker.dataAtOrAfter(fr.Start)
			if err != nil {
				return nil, err
			}
			data = data.Intersect(fr)
			if data.Length() == 0 {
				break
			}
			hash(data)
			fr.Start = data.End
		}
	}
	return h, nil
}

// RecordPageHashes records a hash of every page of f, starting an epoch that
// VerifyDirty ends.
//
// Preconditions:
//   - Dirty tracking must be enabled.
//   - No writer may run concurrently.
func (f *MemoryFile) RecordPageHashes() error {
	if err := f.AwaitLoadAll(); err != nil {
		return err
	}
	f.mu.Lock()
	h, err := f.hashPagesLocked()
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.dirty.swapMu.Lock()
	defer f.dirty.swapMu.Unlock()
	f.dirty.hashes = h
	return nil
}

// DirtyEscape is a page whose contents changed during a dirty tracking epoch
// without being in the dirty set swapped at its end.
type DirtyEscape struct {
	// Offset is the page's offset in the MemoryFile.
	Offset uint64

	// Kind is the page's memory accounting kind.
	Kind usage.MemoryKind
}

// String implements fmt.Stringer.
func (e DirtyEscape) String() string {
	return fmt.Sprintf("%#x (%v)", e.Offset, e.Kind)
}

// DirtyVerification is the result of VerifyDirty.
type DirtyVerification struct {
	f      *MemoryFile
	hashes *pageHashes

	// Escapes is the number of pages that changed during the epoch without
	// being dirty.
	Escapes uint64

	// First holds the first escapes, in increasing order of offset, up to
	// maxDirtyEscapesReported.
	First []DirtyEscape
}

// maxDirtyEscapesReported is the maximum length of DirtyVerification.First.
const maxDirtyEscapesReported = 64

// VerifyDirty ends the epoch started by the last call to RecordPageHashes or
// to DirtyVerification.Commit: it hashes every allocated page of f and
// reports the pages that are not in s, the dirty set swapped at the end of the
// epoch, and whose contents changed. Pages allocated during the epoch were
// zero at its start. Commit starts the next epoch.
//
// Preconditions:
//   - RecordPageHashes was called.
//   - No writer may run concurrently.
func (f *MemoryFile) VerifyDirty(s *DirtySet) (*DirtyVerification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty.swapMu.Lock()
	old := f.dirty.hashes
	f.dirty.swapMu.Unlock()
	if old == nil {
		panic(fmt.Sprintf("MemoryFile(%p).VerifyDirty() called without a previous RecordPageHashes()", f))
	}
	hashes, err := f.hashPagesLocked()
	if err != nil {
		return nil, err
	}
	v := &DirtyVerification{
		f:      f,
		hashes: hashes,
	}
	for seg := f.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		ma := seg.ValuePtr()
		if ma.wasteOrReleasing {
			continue
		}
		for off := seg.Start(); off < seg.End(); off += hostarch.PageSize {
			if s.Contains(off) || v.hashes.get(off) == old.get(off) {
				continue
			}
			v.Escapes++
			if len(v.First) < maxDirtyEscapesReported {
				v.First = append(v.First, DirtyEscape{Offset: off, Kind: ma.kind})
			}
		}
	}
	return v, nil
}

// Commit starts the next verification epoch from the page contents hashed by
// VerifyDirty.
func (v *DirtyVerification) Commit() {
	v.f.dirty.swapMu.Lock()
	defer v.f.dirty.swapMu.Unlock()
	v.f.dirty.hashes = v.hashes
}

// Negative controls.
//
// Verification is only useful if it catches the writes that a missing mark
// would lose. Tests check that it does by disabling one marking path at a
// time, with TestOnlyDisableDirtyMarkPath, and expecting escapes: in package
// tests, and in container tests through runsc's
// --TESTONLY-dirty-tracking-break flag. MapInternal and MarkDirtyBy check
// whether their path is disabled only when the MemoryFile is tracked, with one
// atomic load.

// DirtyMarkPath identifies a path by which writes to MemoryFile pages are
// marked dirty, so that tests can disable it.
type DirtyMarkPath uint32

const (
	// DirtyMarkNone identifies no path: disabling it enables every path.
	DirtyMarkNone DirtyMarkPath = iota

	// DirtyMarkMapInternal is the mark made by MapInternal with write
	// access, before the Sentry's writes through internal mappings:
	// copy-on-write copies, page cache fills, tmpfs file data, the VDSO
	// parameter page, and the zeroing of recycled pages.
	DirtyMarkMapInternal

	// DirtyMarkDecommit is the mark made by decommit and release, which zero
	// pages without writing to them, and by the manual zeroing that replaces
	// a failed decommit.
	DirtyMarkDecommit

	// DirtyMarkTmpfsWrite is the mark made by tmpfs before it writes file
	// data to a disk-backed MemoryFile through the MemoryFile's FD.
	DirtyMarkTmpfsWrite

	// DirtyMarkIOUring is io_uring's registration of its rings with
	// MarkAlwaysDirty.
	DirtyMarkIOUring

	// DirtyMarkWriteProtectFault is the mark made by the MemoryManager when
	// a write, by the application or by the Sentry on its behalf, reaches a
	// pma write-protected for dirty tracking (mm's dirty.go).
	DirtyMarkWriteProtectFault

	// DirtyMarkWriteProtectArm is the MemoryManager's write-protection of the
	// pmas that exist at the start of an epoch
	// (mm.MemoryManager.ArmDirtyTracking). Disabled, pmas written during an
	// earlier epoch stay writable, and their next writes are not marked.
	DirtyMarkWriteProtectArm

	// DirtyMarkUffdInternal is the mark made by HarvestInternalWrites, which
	// reports the writes through internal mappings that write tracking
	// recorded (write_tracking.go): the Sentry's writes through
	// MapInternalUntracked's mappings and, on kvm, the application's stores.
	DirtyMarkUffdInternal

	// DirtyMarkUffdUnmap is the mark made by a platform that tracks writes
	// through its own mappings (platform.WriteTracker, on systrap) when it
	// harvests a mapping that it is about to unmap or replace, which loses
	// what write tracking recorded in it.
	DirtyMarkUffdUnmap
)

// disabledDirtyMarkPath is the DirtyMarkPath disabled by
// TestOnlyDisableDirtyMarkPath.
var disabledDirtyMarkPath atomic.Uint32

// TestOnlyDisableDirtyMarkPath disables the marks made by p, and enables
// those of every other path; DirtyMarkNone enables every path. Dirty tracking
// then misses the writes that p marks, which verification must report as
// escapes. It must only be used by tests.
func TestOnlyDisableDirtyMarkPath(p DirtyMarkPath) {
	disabledDirtyMarkPath.Store(uint32(p))
}

// DirtyMarkPathEnabled returns false if p was disabled by
// TestOnlyDisableDirtyMarkPath. Marking paths that do not mark through
// MarkDirtyBy check it.
func DirtyMarkPathEnabled(p DirtyMarkPath) bool {
	return DirtyMarkPath(disabledDirtyMarkPath.Load()) != p
}
