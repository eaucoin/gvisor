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

	"github.com/cespare/xxhash/v2"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

// Pre-copy.
//
// A pre-copy writes the pages of a MemoryFile to a pages file while the
// MemoryFile is in use, in rounds, so that the save that follows writes only
// the pages written since their last copy, and refers to the copies for the
// others (SaveOpts.Precopy). Each round copies a set of pages: every page that
// may hold data, or the pages that dirty tracking reports written since the
// previous round, whose sources the caller re-armed before the round. A page
// written after its copy was read is therefore dirty again, and a page that
// the save finds clean holds the contents of its last copy. Copies that a
// later round or the save supersedes are unreferenced bytes of the pages
// file, which stays a stream that object stores accept.
//
// Copies read the MemoryFile while it is in use, so they may race with
// writes, frees and reallocations of the pages they read. Every change to a
// page's contents dirties it (see dirty.go), so the copy of a page that
// changed while it was read is never referred to. The pre-copy therefore
// takes no references on the pages it copies, unlike async page loading,
// which writes into them.

// A Precopy is the state of the pre-copy of a MemoryFile.
type Precopy struct {
	amfs *asyncMemoryFileSave

	// limit is the size of the MemoryFile when the pre-copy started. Pages
	// at or beyond limit are never copied: they were allocated during the
	// pre-copy, and the save reads them.
	limit uint64

	// copies holds, for each page below limit, 1 + the pages file offset of
	// its last copy; 0 if it was never copied and holds no data; or
	// copyUnknown.
	copies []uint64

	// hashes holds, for each copied page, its hash, computed after its last
	// copy was enqueued: the hash of the copy if the page was not written
	// since.
	hashes []uint64

	// unhashed are the ranges copied since the last call to Wait.
	unhashed []memmap.FileRange
}

// copyUnknown is the Precopy.copies entry of a page whose contents the
// pre-copy did not copy since it was last dirtied, because it was not
// allocated then: the save reads it.
const copyUnknown = ^uint64(0)

// StartPrecopy starts a pre-copy of f to pf, which a SaveTo of f to pf
// completes with SaveOpts.Precopy.
//
// Preconditions: Dirty tracking is enabled for f.
func (f *MemoryFile) StartPrecopy(pf *AsyncPagesFileSave) (*Precopy, error) {
	if !f.DirtyTracked() {
		return nil, fmt.Errorf("pre-copy requires dirty tracking")
	}
	limit := uint64(len(f.chunksLoad())) * chunkSize
	var sf stateio.SourceFile
	if pf.aw.NeedRegisterSourceFD() {
		var err error
		sf, err = pf.aw.RegisterSourceFD(int32(f.file.Fd()), limit, f.getClientFileRangeSettings(limit))
		if err != nil {
			return nil, fmt.Errorf("failed to register MemoryFile with pages file: %w", err)
		}
	}
	return &Precopy{
		amfs:   &asyncMemoryFileSave{f: f, pf: pf, sf: sf},
		limit:  limit,
		copies: make([]uint64, limit/hostarch.PageSize),
		hashes: make([]uint64, limit/hostarch.PageSize),
	}, nil
}

// CopyAll copies every allocated page of f that may hold data: the
// known-committed ranges, and the data ranges of the backing file in the
// others, so that holes are not read (and committed). It returns the number
// of bytes enqueued for copying. CopyAll waits for async page loading to
// complete first.
func (p *Precopy) CopyAll() (uint64, error) {
	f := p.amfs.f
	if err := f.AwaitLoadAll(); err != nil {
		return 0, fmt.Errorf("previous async page loading failed: %w", err)
	}
	var (
		n      uint64
		seeker = f.newHostFileDataSeeker()
	)
	err := p.forEachAllocated(memmap.FileRange{Start: 0, End: p.limit}, func(fr memmap.FileRange, knownCommitted bool) error {
		if knownCommitted || seeker == nil {
			n += p.copy(fr)
			return nil
		}
		for fr.Length() != 0 {
			data, err := seeker.dataAtOrAfter(fr.Start)
			if err != nil {
				return err
			}
			data = data.Intersect(fr)
			if data.Length() == 0 {
				return nil
			}
			n += p.copy(data)
			fr.Start = data.End
		}
		return nil
	})
	return n, err
}

// Copy copies the pages of f in s that are allocated, and records that the
// others have unknown contents. It returns the number of bytes enqueued for
// copying.
func (p *Precopy) Copy(s *DirtySet) uint64 {
	var n uint64
	s.ForEachRange(memmap.FileRange{Start: 0, End: p.limit}, func(dirty memmap.FileRange) bool {
		for page := dirty.Start / hostarch.PageSize; page < dirty.End/hostarch.PageSize; page++ {
			p.copies[page] = copyUnknown
		}
		p.forEachAllocated(dirty, func(fr memmap.FileRange, _ bool) error {
			n += p.copy(fr)
			return nil
		})
		return true
	})
	return n
}

// forEachAllocated calls fn on each allocated range of f that overlaps fr,
// intersected with fr, in increasing order, with f.mu locked and whether the
// range is known-committed. It stops at the first error fn returns.
func (p *Precopy) forEachAllocated(fr memmap.FileRange, fn func(fr memmap.FileRange, knownCommitted bool) error) error {
	f := p.amfs.f
	for fr.Length() != 0 {
		// Unlock f.mu between segments, so that allocations and frees are
		// not delayed for the whole walk.
		f.mu.Lock()
		seg := f.memAcct.LowerBoundSegment(fr.Start)
		if !seg.Ok() || seg.Start() >= fr.End {
			f.mu.Unlock()
			return nil
		}
		segFR := seg.Range().Intersect(fr)
		var err error
		if ma := seg.ValuePtr(); !ma.wasteOrReleasing {
			err = fn(segFR, ma.knownCommitted)
		}
		f.mu.Unlock()
		if err != nil {
			return err
		}
		fr.Start = segFR.End
	}
	return nil
}

// copy enqueues the pages in fr for copying and returns fr's length.
func (p *Precopy) copy(fr memmap.FileRange) uint64 {
	pf := p.amfs.pf
	pf.mu.Lock()
	off := pf.saveOff
	pf.unsaved.PushBack(apsRange{amfs: p.amfs, FileRange: fr})
	pf.saveOff += fr.Length()
	pf.mu.Unlock()
	pf.stStatus.Notify(apsSTPending)
	for page := fr.Start / hostarch.PageSize; page < fr.End/hostarch.PageSize; page++ {
		p.copies[page] = 1 + off + page*hostarch.PageSize - fr.Start
	}
	p.unhashed = append(p.unhashed, fr)
	return fr.Length()
}

// Wait waits for every copy enqueued so far to be written, then hashes the
// pages copied since the last call to Wait.
func (p *Precopy) Wait() error {
	if err := p.amfs.pf.Flush(); err != nil {
		return err
	}
	f := p.amfs.f
	for _, fr := range p.unhashed {
		page := fr.Start / hostarch.PageSize
		f.forEachMappingSlice(fr, func(bs []byte) {
			for ; len(bs) != 0; bs = bs[hostarch.PageSize:] {
				p.hashes[page] = xxhash.Sum64(bs[:hostarch.PageSize])
				page++
			}
		})
	}
	p.unhashed = p.unhashed[:0]
	return nil
}

// lookup returns what the pre-copy holds for the page at off, if the page
// was not written since its last copy: the pages file offset and hash of its
// copy and true; or false if it holds no data. It returns known false if the
// pre-copy does not know the page's contents.
func (p *Precopy) lookup(off uint64) (pagesOff, hash uint64, hasCopy, known bool) {
	if off >= p.limit {
		return 0, 0, false, false
	}
	page := off / hostarch.PageSize
	switch c := p.copies[page]; c {
	case 0:
		return 0, 0, false, true
	case copyUnknown:
		return 0, 0, false, false
	default:
		return c - 1, p.hashes[page], true, true
	}
}
