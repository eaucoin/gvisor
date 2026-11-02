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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"gvisor.dev/gvisor/pkg/gohacks"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sync"
)

// Copy-on-write saves.
//
// A copy-on-write save defines the image at a save point, inside the pause
// of a save (FreezeForCoWSave), and copies the pages it saves after the pause
// ends, while tasks run. Every write to a page of the MemoryFile passes one of
// its dirty marks before it can land (dirty.go): the mark of a first write to
// write-protected application memory, MapInternal's, decommit's and release's,
// tmpfs's. While a save is copying, each mark first copies the pages it
// covers that the save has not copied yet ("captures" them, to a staging
// buffer), and only then lets the write proceed. The save's copier captures
// the rest in file order. Each page is captured exactly once, by whichever
// comes first, under cowSave.mu; its captured contents are its contents at
// the save point, and the copier writes the image from them, never from the
// MemoryFile's mappings.
//
// This requires that no writer bypass the marks after the save point: the
// MemoryManagers' write-protection must be armed at the save point
// (kernel.Kernel arms it there), and writers that keep internal mappings
// across the pause (MarkAlwaysDirty's) are captured at the save point. It is
// the contract of SwapDirty with paused true, which dirty tracking
// verification checks; with --TESTONLY-cow-verify, a save also hashes every
// page at the save point and checks the captured pages against the hashes.

// CoWOpts are the options of a copy-on-write save.
type CoWOpts struct {
	// StagingMax is the most bytes that the marks may hold captured and not
	// yet written to the pages file; a mark that would hold more waits for
	// the copier to write what is staged.
	StagingMax uint64

	// If Verify is true, FreezeForCoWSave hashes every page that the save
	// captures, and SaveTo fails if a captured page differs from its hash
	// (ErrCoWEscapes).
	Verify bool

	// CopyDelay delays the copier's sweep, so that tests make the marks
	// capture most pages. It must only be used by tests.
	CopyDelay time.Duration
}

// ErrCoWEscapes fails a copy-on-write save of which a page was written
// between the save point and its capture without a mark capturing it first.
var ErrCoWEscapes = errors.New("pages written without being captured")

// cowSweepBytes is the most bytes that the copier captures at once, with
// cowSave.mu locked.
const cowSweepBytes = 256 << 10

// cowSweepInFlightMax is the most bytes that the copier keeps captured by its
// sweep and not yet written.
const cowSweepInFlightMax = 16 << 20

// cowPage values: the result of a captured page, from which SaveTo builds the
// image's extents. Other values are 1 + the offset in the pages file at which
// the page is written.
const (
	cowPageUncaptured = 0
	cowPageZero       = ^uint64(0)
	cowPageBase       = ^uint64(0) - 1
)

// cowSave is the state of a copy-on-write save of a MemoryFile, from
// FreezeForCoWSave until SaveTo has written its metadata.
type cowSave struct {
	f *MemoryFile

	// Immutable after FreezeForCoWSave:

	// meta is f's metadata at the save point. Its MemAcct is the memory
	// accounting at the save point, which SaveTo replaces with the
	// commitment it finds.
	meta *pgallocpb.MemoryFileMetadataProto

	// opts are the options of the save.
	opts CoWOpts
	base *checkpointimage.MemoryFileImage

	// size is the size of f at the save point; pages beyond it are not saved.
	size uint64

	// tHashes holds the XXH64 of each page in pending at the save point, if
	// opts.Verify, indexed by page.
	tHashes []uint64

	// results holds the result of each captured page, indexed by page, and
	// hashes its XXH64. They are written by the copier only.
	results []uint64
	hashes  []uint64

	mu cowMutex

	// pending marks the pages that are still to be captured, one bit per
	// page.
	//
	// +checklocks:mu
	pending []uint64

	// +checklocks:mu
	pendingBytes uint64

	// captured holds the runs captured and not yet taken by the copier.
	//
	// +checklocks:mu
	captured []cowRun

	// staged is the number of bytes captured by marks and not yet written or
	// found not to need writing; stagedCond is broadcast when it shrinks, or
	// when the save fails.
	//
	// +checklocks:mu
	staged     uint64
	stagedCond sync.Cond

	// sweepInFlight is the number of bytes captured by the copier and not
	// yet written.
	//
	// +checklocks:mu
	sweepInFlight uint64

	// If err is not nil, the save failed: marks stop capturing.
	//
	// +checklocks:mu
	err error

	// Counters for logging.
	//
	// +checklocks:mu
	markCaptures uint64
	// +checklocks:mu
	markBytes uint64
	// +checklocks:mu
	markWaits uint64
	// markWaited is how long marks waited for staged bytes to be written, in
	// all, and markWaitedLongest the longest that one mark did.
	//
	// +checklocks:mu
	markWaited time.Duration
	// +checklocks:mu
	markWaitedLongest time.Duration
	// +checklocks:mu
	stagedPeak uint64

	// free holds the buffers that captured pages are all written from,
	// for captures to reuse: the save allocates no more buffers than it
	// holds captured pages at once.
	//
	// +checklocks:mu
	free []*cowBuf

	// wake is notified when a run is captured or a write completes.
	wake chan struct{}
}

// cowBufSize is the size of the buffers that pages are captured to: runs
// are captured in pieces of at most this many bytes, each in a buffer of its
// own, which is reused once every byte of it is written.
const cowBufSize = 64 << 10

// cowBuf is a buffer that pages are captured to.
type cowBuf struct {
	data [cowBufSize]byte

	// left is the number of bytes captured to the buffer that are not yet
	// written to the pages file nor found not to need writing. left is
	// protected by the cowSave's mu.
	left uint64
}

// cowRun is a run of pages captured together, to one buffer.
type cowRun struct {
	fr   memmap.FileRange
	data []byte
	buf  *cowBuf
	// mark is true if a mark captured the run, and false if the copier did.
	mark bool
}

// FreezeForCoWSave starts a copy-on-write save of f, defining its contents at
// the save point: SaveTo with the same options then saves them. opts are the
// options SaveTo will be called with. If dirty is not nil, the save is a
// delta (opts.Base is not nil), and dirty holds the pages dirtied since Base,
// which opts.Clean reports as not clean; otherwise every page is saved.
//
// Preconditions:
//   - No writer runs from before the call until it returns, as for
//     SwapDirty(paused=true).
//   - Writers that keep mappings returned by MapInternal across the pause
//     are registered with MarkAlwaysDirty.
//   - f is dirty-tracked, and every write to it is marked before it lands.
func (f *MemoryFile) FreezeForCoWSave(opts *SaveOpts, dirty *DirtySet, cowOpts CoWOpts) error {
	if !opts.PageHashes {
		return fmt.Errorf("a copy-on-write save requires page hashes")
	}
	if opts.Precopy != nil {
		return fmt.Errorf("a copy-on-write save cannot complete a pre-copy")
	}
	if (opts.Base == nil) != (dirty == nil) {
		return fmt.Errorf("a copy-on-write delta requires both a base and its dirty set")
	}
	if !f.dirty.tracked.Load() {
		return fmt.Errorf("a copy-on-write save requires dirty tracking")
	}
	start := gohacks.Nanotime()
	if opts.Clean == nil {
		if err := f.AwaitLoadAll(); err != nil {
			return fmt.Errorf("previous async page loading failed: %w", err)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for f.haveWaste {
		f.mu.Unlock()
		runtime.Gosched()
		f.mu.Lock()
	}
	if len(f.evictable) != 0 {
		panic(fmt.Sprintf("evictions still pending for %d users; call StartEvictions and WaitForEvictions before FreezeForCoWSave", len(f.evictable)))
	}
	if f.cowSave != nil {
		return fmt.Errorf("a copy-on-write save of MemoryFile %p is already in progress", f)
	}

	size := uint64(len(f.chunksLoad())) * chunkSize
	pages := size / hostarch.PageSize
	c := &cowSave{
		f:       f,
		meta:    f.exportMetadataProto(),
		opts:    cowOpts,
		base:    opts.Base,
		size:    size,
		results: make([]uint64, pages),
		hashes:  make([]uint64, pages),
		pending: make([]uint64, (pages+63)/64),
		wake:    make(chan struct{}, 1),
	}
	c.stagedCond.L = &c.mu
	c.mu.Lock()
	defer c.mu.Unlock()
	for seg := f.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		if seg.ValuePtr().wasteOrReleasing {
			panic(fmt.Sprintf("found waste or releasing pages %v during pgalloc.MemoryFile.FreezeForCoWSave()", seg.Range()))
		}
		c.addPendingLocked(seg.Range())
	}
	if dirty != nil {
		for i := range c.pending {
			if i < len(dirty.words) {
				c.pending[i] &= dirty.words[i]
			} else {
				c.pending[i] = 0
			}
		}
	}
	c.pendingBytes = 0
	for _, w := range c.pending {
		c.pendingBytes += uint64(bits.OnesCount64(w)) * hostarch.PageSize
	}
	if cowOpts.Verify {
		c.tHashes = make([]uint64, pages)
		c.forEachPendingLocked(memmap.FileRange{0, size}, func(fr memmap.FileRange) {
			off := fr.Start
			f.forEachMappingSlice(fr, func(bs []byte) {
				for i := 0; i < len(bs); i += hostarch.PageSize {
					c.tHashes[off/hostarch.PageSize] = xxhash.Sum64(bs[i : i+hostarch.PageSize])
					off += hostarch.PageSize
				}
			})
		})
	}

	// Writers that keep internal mappings write without marks: capture
	// their pages now.
	f.dirty.swapMu.Lock()
	always := append([]memmap.FileRange(nil), f.dirty.always...)
	f.dirty.swapMu.Unlock()
	for _, fr := range always {
		c.captureLocked(fr, false /* mark */)
	}

	f.cowSave = c
	if c.pendingBytes != 0 {
		f.cow.Store(c)
	}
	log.Infof("MemoryFile(%p): copy-on-write save point: %d bytes pending of %d bytes, frozen in %s", f, c.pendingBytes, size, time.Duration(gohacks.Nanotime()-start))
	return nil
}

// addPendingLocked marks every page of fr pending.
//
// +checklocks:c.mu
func (c *cowSave) addPendingLocked(fr memmap.FileRange) {
	for page := fr.Start / hostarch.PageSize; page < fr.End/hostarch.PageSize; page++ {
		c.pending[page/64] |= 1 << (page % 64)
	}
}

// forEachPendingLocked calls fn on each maximal run of pending pages in fr, in
// increasing order.
//
// +checklocks:c.mu
func (c *cowSave) forEachPendingLocked(fr memmap.FileRange, fn func(memmap.FileRange)) {
	s := DirtySet{words: c.pending}
	s.ForEachRange(fr, func(r memmap.FileRange) bool {
		fn(r)
		return true
	})
}

// cowBeforeWrite captures the pages of fr that a copy-on-write save of f
// still has to capture, before they are written by a writer whose mark is
// made by path p.
func (f *MemoryFile) cowBeforeWrite(p DirtyMarkPath, fr memmap.FileRange) {
	c := f.cow.Load()
	if c == nil || !cowCapturePathEnabled(p) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captureLocked(fr, true /* mark */)
}

// captureLocked captures the pending pages of fr. If mark is true, a mark
// captures them, and waits for staged bytes to be written first if the
// staging limit is reached.
//
// +checklocks:c.mu
func (c *cowSave) captureLocked(fr memmap.FileRange, mark bool) {
	fr.Start = hostarch.PageRoundDown(fr.Start)
	fr.End = min(hostarch.MustPageRoundUp(fr.End), c.size)
	var waited time.Duration
	for c.err == nil && c.pendingBytes != 0 && fr.Start < fr.End {
		var run memmap.FileRange
		c.forEachPendingLocked(fr, func(r memmap.FileRange) {
			if run.Length() == 0 {
				run = r
			}
		})
		if run.Length() == 0 {
			break
		}
		if mark && c.staged != 0 && c.staged+run.Length() > c.opts.StagingMax {
			c.markWaits++
			waitStart := gohacks.Nanotime()
			c.stagedCond.Wait()
			waited += time.Duration(gohacks.Nanotime() - waitStart)
			// The copier may have captured pages of fr meanwhile.
			continue
		}
		c.captureRunLocked(run, mark)
		fr.Start = run.End
	}
	if waited != 0 {
		c.markWaited += waited
		c.markWaitedLongest = max(c.markWaitedLongest, waited)
	}
}

// captureRunLocked copies the pages of run, which are all pending, and makes
// them captured.
//
// +checklocks:c.mu
func (c *cowSave) captureRunLocked(run memmap.FileRange, mark bool) {
	for start := run.Start; start < run.End; start += cowBufSize {
		piece := memmap.FileRange{start, min(start+cowBufSize, run.End)}
		var buf *cowBuf
		if n := len(c.free); n != 0 {
			buf = c.free[n-1]
			c.free = c.free[:n-1]
		} else {
			buf = new(cowBuf)
		}
		buf.left = piece.Length()
		data := buf.data[:piece.Length()]
		n := 0
		c.f.forEachMappingSlice(piece, func(bs []byte) {
			n += copy(data[n:], bs)
		})
		c.captured = append(c.captured, cowRun{fr: piece, data: data, buf: buf, mark: mark})
	}
	for page := run.Start / hostarch.PageSize; page < run.End/hostarch.PageSize; page++ {
		c.pending[page/64] &^= 1 << (page % 64)
	}
	c.pendingBytes -= run.Length()
	if mark {
		c.markCaptures++
		c.markBytes += run.Length()
		c.staged += run.Length()
		c.stagedPeak = max(c.stagedPeak, c.staged)
	} else {
		c.sweepInFlight += run.Length()
	}
	if c.pendingBytes == 0 {
		// Marks have nothing left to capture.
		c.f.cow.Store(nil)
	}
	c.notify()
}

func (c *cowSave) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// written releases n bytes of r once they are written or found not to need
// writing; its buffer is reused once all of them are.
func (c *cowSave) written(r cowRun, n uint64) {
	c.mu.Lock()
	if r.buf.left -= n; r.buf.left == 0 {
		c.free = append(c.free, r.buf)
	}
	if r.mark {
		c.staged -= n
		c.stagedCond.Broadcast()
	} else {
		c.sweepInFlight -= n
	}
	c.mu.Unlock()
	c.notify()
}

// fail fails the save: marks stop capturing, and those waiting return.
func (c *cowSave) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
	c.f.cow.Store(nil)
	c.stagedCond.Broadcast()
}

// CancelCoWSave ends the copy-on-write save of f that FreezeForCoWSave
// started, if SaveTo has not ended it: the save failed before SaveTo saved
// f.
func (f *MemoryFile) CancelCoWSave() {
	f.mu.Lock()
	c := f.cowSave
	f.cowSave = nil
	f.mu.Unlock()
	if c != nil {
		c.fail(fmt.Errorf("copy-on-write save of MemoryFile %p cancelled", f))
	}
}

// takeCoWSave returns the copy-on-write save of f that FreezeForCoWSave
// started, if any, and ends it as far as f is concerned.
func (f *MemoryFile) takeCoWSave() *cowSave {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cowSave
	f.cowSave = nil
	return c
}

// cowSaveTo is SaveTo for a MemoryFile frozen by FreezeForCoWSave: it
// captures every page still pending, writes the captured pages to
// opts.PagesFile, and writes the metadata of f at the save point to w.
func (f *MemoryFile) cowSaveTo(ctx context.Context, w io.Writer, c *cowSave, opts *SaveOpts) (retErr error) {
	defer func() {
		if retErr != nil {
			c.fail(retErr)
		}
	}()
	if opts.PagesFile == nil || opts.PagesFile.aw.NeedRegisterSourceFD() {
		return fmt.Errorf("a copy-on-write save requires a pages file written from the Sentry's buffers")
	}
	start := gohacks.Nanotime()
	amfs := &asyncMemoryFileSave{f: f, pf: opts.PagesFile}
	if c.opts.CopyDelay != 0 {
		time.Sleep(c.opts.CopyDelay)
	}

	var (
		writtenBytes, baseBytes, zeroBytes, escapes uint64
		firstEscapes                                []uint64
	)
	// process writes the pages of r that hold data that is not in base.
	process := func(r cowRun) {
		release := func(n uint64) { c.written(r, n) }
		var runStart uint64
		var runData []byte
		flush := func() {
			if len(runData) == 0 {
				return
			}
			fr := memmap.FileRange{runStart, runStart + uint64(len(runData))}
			off := opts.PagesFile.enqueue(apsRange{amfs: amfs, FileRange: fr, src: runData, release: release})
			for page := fr.Start; page < fr.End; page += hostarch.PageSize {
				c.results[page/hostarch.PageSize] = 1 + off + (page - fr.Start)
			}
			writtenBytes += fr.Length()
			runData = nil
		}
		var unwritten uint64
		for i := 0; i < len(r.data); i += hostarch.PageSize {
			off := r.fr.Start + uint64(i)
			page := off / hostarch.PageSize
			pg := r.data[i : i+hostarch.PageSize]
			h := xxhash.Sum64(pg)
			c.hashes[page] = h
			if c.tHashes != nil && c.tHashes[page] != h {
				escapes++
				if len(firstEscapes) < maxDirtyEscapesReported {
					firstEscapes = append(firstEscapes, off)
				}
			}
			result := uint64(cowPageUncaptured)
			if c.base != nil {
				if _, hasExtent := c.base.ExtentAt(off); hasExtent {
					if bh, hasHash := c.base.HashAt(off); hasHash && bh == h {
						result = cowPageBase
					}
				}
			}
			if result == cowPageUncaptured && h == zeroPageXXH64 && bytes.Equal(pg, zeroPageBytes[:]) {
				result = cowPageZero
			}
			if result != cowPageUncaptured {
				flush()
				c.results[page] = result
				unwritten += hostarch.PageSize
				if result == cowPageBase {
					baseBytes += hostarch.PageSize
				} else {
					zeroBytes += hostarch.PageSize
				}
				continue
			}
			if len(runData) == 0 {
				runStart = off
				runData = pg
			} else {
				runData = runData[:len(runData)+hostarch.PageSize]
			}
		}
		flush()
		if unwritten != 0 {
			c.written(r, unwritten)
		}
	}

	dataSeeker := f.newHostFileDataSeeker()
	segs := c.meta.MemAcct
	segIndex := 0
	// sweep captures the next pending pages, up to cowSweepBytes, skipping
	// the holes of ranges not known to be committed, which are zero.
	var sweepNext uint64
	sweep := func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		for c.err == nil && c.pendingBytes != 0 {
			var run memmap.FileRange
			c.forEachPendingLocked(memmap.FileRange{sweepNext, c.size}, func(r memmap.FileRange) {
				if run.Length() == 0 {
					run = r
				}
			})
			if run.Length() == 0 {
				panic(fmt.Sprintf("%d bytes pending, none at or after %#x", c.pendingBytes, sweepNext))
			}
			run.End = min(run.End, run.Start+cowSweepBytes)
			for segIndex < len(segs) && segs[segIndex].End <= run.Start {
				segIndex++
			}
			if seg := segs[segIndex]; !seg.KnownCommitted && dataSeeker != nil {
				run.End = min(run.End, seg.End)
				data, err := dataSeeker.dataAtOrAfter(run.Start)
				if err != nil {
					return fmt.Errorf("failed to find data ranges in MemoryFile backing file: %w", err)
				}
				if hole := (memmap.FileRange{run.Start, min(data.Start, run.End)}); hole.Length() != 0 {
					for page := hole.Start / hostarch.PageSize; page < hole.End/hostarch.PageSize; page++ {
						c.pending[page/64] &^= 1 << (page % 64)
						c.results[page] = cowPageZero
						c.hashes[page] = zeroPageXXH64
						if c.tHashes != nil && c.tHashes[page] != zeroPageXXH64 {
							escapes++
							if len(firstEscapes) < maxDirtyEscapesReported {
								firstEscapes = append(firstEscapes, page*hostarch.PageSize)
							}
						}
					}
					c.pendingBytes -= hole.Length()
					zeroBytes += hole.Length()
					sweepNext = hole.End
					if c.pendingBytes == 0 {
						f.cow.Store(nil)
					}
					continue
				}
				run.End = min(run.End, data.End)
			}
			c.captureRunLocked(run, false /* mark */)
			sweepNext = run.End
			return nil
		}
		return nil
	}

	for {
		c.mu.Lock()
		runs := c.captured
		c.captured = nil
		inFlight := c.sweepInFlight
		pendingBytes := c.pendingBytes
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if err := opts.PagesFile.exitError(); err != nil {
			return err
		}
		for _, r := range runs {
			process(r)
		}
		if len(runs) != 0 {
			continue
		}
		if pendingBytes == 0 {
			break
		}
		if inFlight < cowSweepInFlightMax {
			if err := sweep(); err != nil {
				return err
			}
			continue
		}
		<-c.wake
	}
	durCopy := time.Duration(gohacks.Nanotime() - start)

	if escapes != 0 {
		log.Warningf("MemoryFile(%p): copy-on-write save: %d pages written without being captured, starting at %#x", f, escapes, firstEscapes)
		return fmt.Errorf("copy-on-write save of MemoryFile %p: %d %w", f, escapes, ErrCoWEscapes)
	}

	// Build the metadata from the save point's memory accounting and the
	// captured pages, as SaveTo's scan does.
	timeMetadataStart := gohacks.Nanotime()
	img, err := newImageSaver(f, opts, nil)
	if err != nil {
		return err
	}
	pb := c.meta
	memAcct := pb.MemAcct
	pb.MemAcct = nil
	appendMemAcct := func(s *pgallocpb.MemAcctRangeProto, fr memmap.FileRange, committed bool) {
		if n := len(pb.MemAcct); n != 0 {
			last := pb.MemAcct[n-1]
			if last.End == fr.Start && last.Kind == s.Kind && last.MemCgId == s.MemCgId && last.KnownCommitted == committed {
				last.End = fr.End
				return
			}
		}
		pb.MemAcct = append(pb.MemAcct, &pgallocpb.MemAcctRangeProto{
			Start:          fr.Start,
			End:            fr.End,
			Kind:           s.Kind,
			MemCgId:        s.MemCgId,
			KnownCommitted: committed,
		})
	}
	var committedBytes uint64
	for _, s := range memAcct {
		for off := s.Start; off < s.End; off += hostarch.PageSize {
			page := off / hostarch.PageSize
			var (
				e         checkpointimage.Extent
				hasExtent bool
				baseHash  uint64
				hasHash   bool
			)
			if img.base != nil {
				e, hasExtent, baseHash, hasHash = img.baseCursor.Lookup(off)
			}
			clean := img.isClean(off)
			result := c.results[page]
			var zero bool
			if clean {
				zero = !hasExtent
			} else {
				if result == cowPageUncaptured {
					panic(fmt.Sprintf("page %#x of MemoryFile %p is neither clean nor captured", off, f))
				}
				zero = result == cowPageZero
			}
			committed := !zero || (s.KnownCommitted && !opts.ExcludeCommittedZeroPages)
			appendMemAcct(s, memmap.FileRange{off, off + hostarch.PageSize}, committed)
			if !committed {
				continue
			}
			committedBytes += hostarch.PageSize
			pageFR := memmap.FileRange{off, off + hostarch.PageSize}
			switch {
			case clean:
				if !hasHash {
					baseHash = zeroPageXXH64
				}
				img.appendHash(baseHash)
				if hasExtent {
					img.emitBase(e, off)
				}
			case result == cowPageZero:
				img.appendHash(zeroPageXXH64)
			case result == cowPageBase:
				img.appendHash(c.hashes[page])
				if !hasExtent {
					panic(fmt.Sprintf("page %#x of MemoryFile %p refers to a base page that has no extent", off, f))
				}
				img.emitBase(e, off)
			default:
				img.appendHash(c.hashes[page])
				img.appendExtent(pageFR, 0, result-1)
			}
		}
	}
	pb.Extents = img.extents
	pb.PageHashes = img.hashes
	if err := checkpointimage.WriteRecord(w, pb); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	c.mu.Lock()
	markCaptures, markBytes, markWaits, markWaited, markWaitedLongest, stagedPeak := c.markCaptures, c.markBytes, c.markWaits, c.markWaited, c.markWaitedLongest, c.stagedPeak
	c.mu.Unlock()
	log.Infof("MemoryFile(%p): copy-on-write save copied in %s: %d bytes written to the pages file, %d bytes in the base image, %d bytes zero, %d bytes committed; marks captured %d bytes in %d runs, waited %d times for %s, one mark %s at most, staging peak %d bytes; metadata in %s",
		f, durCopy, writtenBytes, baseBytes, zeroBytes, committedBytes, markBytes, markCaptures, markWaits, markWaited, markWaitedLongest, stagedPeak, time.Duration(gohacks.Nanotime()-timeMetadataStart))
	return nil
}

// enqueue enqueues r for writing and returns the offset in the pages file at
// which it will be written.
func (apfs *AsyncPagesFileSave) enqueue(r apsRange) uint64 {
	apfs.mu.Lock()
	off := apfs.saveOff
	apfs.unsaved.PushBack(r)
	apfs.saveOff += r.Length()
	apfs.mu.Unlock()
	apfs.stStatus.Notify(apsSTPending)
	return off
}

// exitError returns the error that ended async page saving early, if it did.
func (apfs *AsyncPagesFileSave) exitError() error {
	apfs.mu.Lock()
	defer apfs.mu.Unlock()
	if apfs.exited && apfs.exitErr != nil {
		return apfs.exitErr
	}
	return nil
}

// Negative controls of copy-on-write saves: as dirty tracking's marks,
// captures can be disabled one path at a time, so that tests check that
// CoWOpts.Verify catches the writes they then miss.

// disabledCoWCapturePath is the DirtyMarkPath whose captures are disabled
// by TestOnlyDisableCoWCapturePath, or DirtyMarkNone.
var disabledCoWCapturePath atomic.Uint32

// TestOnlyDisableCoWCapturePath disables the captures made before the writes
// that p marks, and enables those of every other path; DirtyMarkNone enables
// every path. It must only be used by tests.
func TestOnlyDisableCoWCapturePath(p DirtyMarkPath) {
	disabledCoWCapturePath.Store(uint32(p))
}

// cowCapturePathEnabled returns false if p's captures were disabled by
// TestOnlyDisableCoWCapturePath.
func cowCapturePathEnabled(p DirtyMarkPath) bool {
	d := DirtyMarkPath(disabledCoWCapturePath.Load())
	return d == DirtyMarkNone || d != p
}
