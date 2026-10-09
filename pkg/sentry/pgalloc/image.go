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
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sync"
)

// zeroPageXXH64 is the page hash (XXH64) of a zero-filled page.
var zeroPageXXH64 = xxhash.Sum64(make([]byte, hostarch.PageSize))

// imageSaver builds the extents and page hashes of a MemoryFile saved to a
// pages file (checkpointimage.MemoryFileImage).
type imageSaver struct {
	f *MemoryFile

	// write enqueues the pages in a range for writing to the pages file and
	// returns the pages file offset at which they will be written.
	write func(fr memmap.FileRange) uint64

	// If base is not nil, pages that clean reports as unchanged since base was
	// saved, and pages whose hash is unchanged since then, refer to base's
	// data in layer baseLayers[layer] rather than being written.
	base       *checkpointimage.MemoryFileImage
	baseCursor checkpointimage.Cursor
	baseLayers []uint32
	clean      func(off uint64) bool

	// If precopy is not nil, pages that clean reports as unchanged since
	// their last copy by precopy refer to the copy, in layer 0.
	precopy *Precopy

	// extents are the extents of the pages emitted so far.
	extents []*pgallocpb.ExtentProto

	// If hash is true, hashes holds the page hash of each page emitted so
	// far (SaveOpts.PageHashes).
	hash   bool
	hashes []byte

	// hashLater are ranges of pages written to the pages file whose hashes
	// are computed by finish(), and the index in hashes of their first page.
	hashLater []hashRange

	// Counters for logging.
	writtenBytes uint64
	baseBytes    uint64
	refinedBytes uint64
	precopyBytes uint64
	zeroBytes    uint64
}

type hashRange struct {
	memmap.FileRange
	index uint64
}

func newImageSaver(f *MemoryFile, opts *SaveOpts, write func(memmap.FileRange) uint64) (*imageSaver, error) {
	s := &imageSaver{
		f:     f,
		write: write,
		hash:  opts.PageHashes,
	}
	if opts.Base != nil {
		if !opts.Base.HasHashes() {
			return nil, fmt.Errorf("base image has no page hashes")
		}
		for _, e := range opts.Base.Extents {
			if e.Layer >= uint32(len(opts.BaseLayers)) {
				return nil, fmt.Errorf("base image extent %#x-%#x is in layer %d, which BaseLayers (%d layers) does not map", e.Start, e.End, e.Layer, len(opts.BaseLayers))
			}
		}
		s.base = opts.Base
		s.baseCursor = opts.Base.Cursor()
		s.baseLayers = opts.BaseLayers
	}
	if opts.Precopy != nil {
		if opts.Precopy.amfs.pf != opts.PagesFile {
			return nil, fmt.Errorf("pre-copy is to another pages file")
		}
		s.precopy = opts.Precopy
	}
	if s.base != nil || s.precopy != nil {
		s.clean = opts.Clean
	}
	return s, nil
}

// isClean returns true if the page at off is unchanged since base was saved,
// or since its last copy by precopy, and its contents are in either.
func (s *imageSaver) isClean(off uint64) bool {
	if s.clean == nil || !s.clean(off) {
		return false
	}
	if s.precopy != nil {
		// A page dirtied when it was not allocated has contents that the
		// pre-copy does not know.
		_, _, _, known := s.precopy.lookup(off)
		return known
	}
	return true
}

// cleanHasData returns true if the clean page at off has data in precopy or
// base; it is zero otherwise.
//
// Preconditions: s.isClean(off).
func (s *imageSaver) cleanHasData(off uint64) bool {
	if s.precopy != nil {
		if _, _, hasCopy, _ := s.precopy.lookup(off); hasCopy {
			return true
		}
	}
	if s.base != nil {
		_, hasExtent := s.base.ExtentAt(off)
		return hasExtent
	}
	return false
}

// emit records that the known-committed pages in fr are saved.
//
// Preconditions:
//   - fr is page-aligned and non-empty.
//   - Successive calls are in increasing offset order and do not overlap.
func (s *imageSaver) emit(fr memmap.FileRange) {
	if s.base == nil && s.precopy == nil {
		s.emitWrite(fr, nil)
		return
	}
	// Each page is written to the pages file, refers to its copy by precopy
	// or to base, or is zero (has no extent). Pages to be written are written
	// in runs, whose hashes are collected in run.
	var run []byte
	runStart := fr.Start
	flushRun := func(end uint64) {
		if runStart < end {
			s.emitWrite(memmap.FileRange{Start: runStart, End: end}, run)
		}
		run = run[:0]
		runStart = end + hostarch.PageSize
	}
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		var (
			e         checkpointimage.Extent
			hasExtent bool
			hash      uint64
			hasHash   bool
		)
		if s.base != nil {
			e, hasExtent, hash, hasHash = s.baseCursor.Lookup(off)
		}
		if s.isClean(off) {
			flushRun(off)
			if s.precopy != nil {
				if pagesOff, h, hasCopy, _ := s.precopy.lookup(off); hasCopy {
					s.appendHash(h)
					s.appendExtent(memmap.FileRange{Start: off, End: off + hostarch.PageSize}, 0, pagesOff)
					s.precopyBytes += hostarch.PageSize
					continue
				}
			}
			if !hasHash {
				// Not committed in base, hence zero then and now.
				hash = zeroPageXXH64
			}
			s.appendHash(hash)
			if hasExtent {
				s.emitBase(e, off)
				s.baseBytes += hostarch.PageSize
			} else {
				s.zeroBytes += hostarch.PageSize
			}
			continue
		}
		pg := s.f.pageSlice(off)
		h := xxhash.Sum64(pg)
		switch {
		case hasExtent && hasHash && h == hash:
			// Written since base, but with the same contents.
			flushRun(off)
			s.appendHash(h)
			s.emitBase(e, off)
			s.refinedBytes += hostarch.PageSize
		case h == zeroPageXXH64 && bytes.Equal(pg, zeroPageBytes[:]):
			flushRun(off)
			s.appendHash(h)
			s.zeroBytes += hostarch.PageSize
		default:
			run = binary.LittleEndian.AppendUint64(run, h)
		}
	}
	flushRun(fr.End)
}

// emitWrite emits the pages in fr as written to the pages file. hashes holds
// their page hashes, or is empty if finish() is to compute them.
func (s *imageSaver) emitWrite(fr memmap.FileRange, hashes []byte) {
	off := s.write(fr)
	s.appendExtent(fr, 0, off)
	s.writtenBytes += fr.Length()
	if !s.hash {
		return
	}
	if len(hashes) != 0 {
		s.hashes = append(s.hashes, hashes...)
	} else {
		s.hashLater = append(s.hashLater, hashRange{fr, uint64(len(s.hashes)) / 8})
		s.hashes = append(s.hashes, make([]byte, fr.Length()/hostarch.PageSize*8)...)
	}
}

// emitBase emits the page at off as referring to its data in base, which is
// in the extent e.
func (s *imageSaver) emitBase(e checkpointimage.Extent, off uint64) {
	s.appendExtent(memmap.FileRange{Start: off, End: off + hostarch.PageSize}, s.baseLayers[e.Layer], e.OffsetOf(off))
}

func (s *imageSaver) appendExtent(fr memmap.FileRange, layer uint32, off uint64) {
	if n := len(s.extents); n != 0 {
		last := s.extents[n-1]
		if last.End == fr.Start && last.Layer == layer && last.Offset+(last.End-last.Start) == off {
			last.End = fr.End
			return
		}
	}
	s.extents = append(s.extents, &pgallocpb.ExtentProto{
		Start:  fr.Start,
		End:    fr.End,
		Layer:  layer,
		Offset: off,
	})
}

func (s *imageSaver) appendHash(h uint64) {
	if !s.hash {
		return
	}
	s.hashes = binary.LittleEndian.AppendUint64(s.hashes, h)
}

// hashChunkSize is the amount of memory hashed by one goroutine at a time.
const hashChunkSize = 4 << 20

// finish computes the remaining page hashes, if any, using up to GOMAXPROCS
// goroutines, and stores the extents and page hashes in pb.
func (s *imageSaver) finish(pb *pgallocpb.MemoryFileMetadataProto) {
	// Split the ranges to hash into chunks of at most hashChunkSize bytes.
	var chunks []hashRange
	for _, hr := range s.hashLater {
		for start := hr.Start; start < hr.End; start += hashChunkSize {
			end := min(start+hashChunkSize, hr.End)
			chunks = append(chunks, hashRange{
				FileRange: memmap.FileRange{Start: start, End: end},
				index:     hr.index + (start-hr.Start)/hostarch.PageSize,
			})
		}
	}
	var (
		next atomic.Int64
		wg   sync.WaitGroup
	)
	workers := min(runtime.GOMAXPROCS(0), len(chunks))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= int64(len(chunks)) {
					return
				}
				c := chunks[i]
				index := c.index
				s.f.forEachMappingSlice(c.FileRange, func(bs []byte) {
					for len(bs) != 0 {
						binary.LittleEndian.PutUint64(s.hashes[index*8:], xxhash.Sum64(bs[:hostarch.PageSize]))
						bs = bs[hostarch.PageSize:]
						index++
					}
				})
			}
		}()
	}
	wg.Wait()
	pb.Extents = s.extents
	pb.PageHashes = s.hashes
}

// pageSlice returns the page at off.
func (f *MemoryFile) pageSlice(off uint64) []byte {
	var pg []byte
	f.forEachMappingSlice(memmap.FileRange{Start: off, End: off + hostarch.PageSize}, func(bs []byte) {
		pg = bs
	})
	return pg
}

// zeroPageBytes is a zero-filled page.
var zeroPageBytes [hostarch.PageSize]byte
