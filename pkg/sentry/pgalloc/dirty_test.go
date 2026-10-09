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

//go:build !pagesize_64k

package pgalloc

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sync"
)

// pageRange returns the range of n pages starting at page i of fr.
func pageRange(fr memmap.FileRange, i, n uint64) memmap.FileRange {
	return memmap.FileRange{fr.Start + i*hostarch.PageSize, fr.Start + (i+n)*hostarch.PageSize}
}

// dirtyRanges returns the ranges of s within fr.
func dirtyRanges(s *DirtySet, fr memmap.FileRange) []memmap.FileRange {
	var frs []memmap.FileRange
	s.ForEachRange(fr, func(r memmap.FileRange) bool {
		frs = append(frs, r)
		return true
	})
	return frs
}

// checkDirty checks that the pages of fr in s are exactly want.
func checkDirty(t *testing.T, what string, s *DirtySet, fr memmap.FileRange, want ...memmap.FileRange) {
	t.Helper()
	if got := dirtyRanges(s, fr); !slices.Equal(got, want) {
		t.Errorf("%s: dirty ranges %v, want %v", what, got, want)
	}
}

func TestDirtySet(t *testing.T) {
	const pages = 3 * 64
	page := func(i uint64) uint64 { return i * hostarch.PageSize }
	for _, tc := range []struct {
		name  string
		marks []memmap.FileRange
	}{
		{name: "empty"},
		{name: "one page", marks: []memmap.FileRange{{page(5), page(6)}}},
		{name: "unaligned range", marks: []memmap.FileRange{{page(5) + 1, page(6) + 1}}},
		{name: "first and last pages", marks: []memmap.FileRange{{0, page(1)}, {page(pages - 1), page(pages)}}},
		{name: "a word", marks: []memmap.FileRange{{page(64), page(128)}}},
		{name: "across words", marks: []memmap.FileRange{{page(60), page(130)}}},
		{name: "everything", marks: []memmap.FileRange{{0, page(pages)}}},
		{name: "several", marks: []memmap.FileRange{{page(1), page(3)}, {page(3), page(4)}, {page(63), page(65)}, {page(100), page(101)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b dirtyBitmap
			b.growLocked(1)
			want := make(map[uint64]bool)
			for _, fr := range tc.marks {
				b.mark(fr)
				for p := fr.Start / hostarch.PageSize; p*hostarch.PageSize < fr.End; p++ {
					want[p] = true
				}
			}
			s := &DirtySet{words: make([]uint64, dirtyChunkWords)}
			for i := range s.words {
				s.words[i] = b.load()[0][i].Load()
			}
			if got, want := s.Bytes(), uint64(len(want))*hostarch.PageSize; got != want {
				t.Errorf("Bytes() = %d, want %d", got, want)
			}
			for p := uint64(0); p < pages; p++ {
				if got := s.Contains(page(p)); got != want[p] {
					t.Errorf("Contains(page %d) = %v, want %v", p, got, want[p])
				}
			}
			// ForEachRange over every subrange of the first pages must
			// return the maximal runs of the model.
			for start := uint64(0); start < pages; start += 7 {
				for end := start + 1; end <= pages; end += 11 {
					fr := memmap.FileRange{page(start), page(end)}
					var wantRanges []memmap.FileRange
					for p := start; p < end; p++ {
						if !want[p] {
							continue
						}
						if n := len(wantRanges); n != 0 && wantRanges[n-1].End == page(p) {
							wantRanges[n-1].End = page(p + 1)
						} else {
							wantRanges = append(wantRanges, memmap.FileRange{page(p), page(p + 1)})
						}
					}
					checkDirty(t, fmt.Sprintf("ForEachRange(%v)", fr), s, fr, wantRanges...)
				}
			}
		})
	}
}

// newTrackedMemoryFile returns a MemoryFile with dirty tracking enabled and
// an allocation of 64 pages, with the empty dirty set swapped out.
func newTrackedMemoryFile(t *testing.T) (*MemoryFile, memmap.FileRange) {
	t.Helper()
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocate(t, f, 64*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(fr) })
	f.EnableDirtyTracking()
	if s := f.SwapDirty(true); s.Bytes() != 0 {
		t.Fatalf("pages dirty right after tracking started: %v", dirtyRanges(s, fr))
	}
	return f, fr
}

func TestDirtyUntracked(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocate(t, f, 4*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(fr) })
	f.MarkDirty(fr)
	if _, err := f.MapInternal(fr, hostarch.Write); err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	if f.DirtyTracked() {
		t.Errorf("DirtyTracked() = true before EnableDirtyTracking")
	}
	f.EnableDirtyTracking()
	checkDirty(t, "writes before tracking started", f.SwapDirty(true), fr)
}

// TestDirtyMarks checks each path by which a MemoryFile marks the pages it
// sees written.
func TestDirtyMarks(t *testing.T) {
	t.Run("MapInternal", func(t *testing.T) {
		f, fr := newTrackedMemoryFile(t)
		if _, err := f.MapInternal(pageRange(fr, 1, 2), hostarch.Read); err != nil {
			t.Fatalf("MapInternal(Read): %v", err)
		}
		if _, err := f.MapInternal(pageRange(fr, 4, 3), hostarch.Write); err != nil {
			t.Fatalf("MapInternal(Write): %v", err)
		}
		if _, err := f.MapInternal(memmap.FileRange{pageRange(fr, 9, 1).Start + 10, pageRange(fr, 9, 1).Start + 20}, hostarch.ReadWrite); err != nil {
			t.Fatalf("MapInternal(ReadWrite): %v", err)
		}
		checkDirty(t, "after MapInternal", f.SwapDirty(true), fr, pageRange(fr, 4, 3), pageRange(fr, 9, 1))
	})

	t.Run("MarkDirty", func(t *testing.T) {
		f, fr := newTrackedMemoryFile(t)
		f.MarkDirty(pageRange(fr, 63, 1))
		checkDirty(t, "after MarkDirty", f.SwapDirty(true), fr, pageRange(fr, 63, 1))
	})

	t.Run("Decommit", func(t *testing.T) {
		f, fr := newTrackedMemoryFile(t)
		f.Decommit(pageRange(fr, 10, 4))
		checkDirty(t, "after Decommit", f.SwapDirty(true), fr, pageRange(fr, 10, 4))
	})

	t.Run("release", func(t *testing.T) {
		f, fr := newTrackedMemoryFile(t)
		freed := allocate(t, f, 8*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
		writePattern(t, f, freed, 1)
		f.SwapDirty(true)
		f.DecRef(freed)
		waitForRelease(t, f)
		checkDirty(t, "after release", f.SwapDirty(true), memmap.FileRange{fr.Start, freed.End}, freed)
	})

	t.Run("recycled pages", func(t *testing.T) {
		// Pages freed and allocated again, recycled or released in between,
		// have their contents zeroed, which marks them.
		f, _ := newTrackedMemoryFile(t)
		freed := allocate(t, f, 8*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
		writePattern(t, f, freed, 1)
		f.DecRef(freed)
		f.SwapDirty(true)
		again := allocate(t, f, 8*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
		t.Cleanup(func() { f.DecRef(again) })
		waitForRelease(t, f)
		s := f.SwapDirty(true)
		for off := again.Start; off < again.End; off += hostarch.PageSize {
			if !s.Contains(off) {
				t.Errorf("page %#x of the new allocation is not dirty", off)
			}
		}
	})

	t.Run("new chunk", func(t *testing.T) {
		// A chunk added while tracking is enabled is tracked.
		f, _ := newTrackedMemoryFile(t)
		big := allocate(t, f, chunkSize, AllocOpts{Kind: usage.Anonymous})
		t.Cleanup(func() { f.DecRef(big) })
		if big.End <= chunkSize {
			t.Fatalf("allocation %v did not extend the file", big)
		}
		last := memmap.FileRange{big.End - hostarch.PageSize, big.End}
		if _, err := f.MapInternal(last, hostarch.Write); err != nil {
			t.Fatalf("MapInternal: %v", err)
		}
		checkDirty(t, "after MapInternal in a new chunk", f.SwapDirty(true), big, last)
	})

	t.Run("enabled before loading", func(t *testing.T) {
		// Tracking enabled before a MemoryFile's metadata is loaded covers
		// the chunks loaded.
		fr, _, img := loaderTestImage(t, 4*hostarch.PageSize)
		restored := newTestMemoryFile(t, testMemoryFileOpts{})
		restored.EnableDirtyTracking()
		loadImage(t, img, restored)
		t.Cleanup(func() { releaseAll(t, restored) })
		if _, err := restored.MapInternal(pageRange(fr, 1, 1), hostarch.Write); err != nil {
			t.Fatalf("MapInternal: %v", err)
		}
		checkDirty(t, "after loading and MapInternal", restored.SwapDirty(true), fr, pageRange(fr, 1, 1))
	})

	t.Run("async page loading", func(t *testing.T) {
		// Loading the pages of an image does not mark them: they are the
		// image's.
		fr, _, img := loaderTestImage(t, 32*hostarch.PageSize)
		r := newTestPagesFile(t, img.pages, testPagesFileOpts{
			maxReadBytes: 4 * hostarch.PageSize,
			maxParallel:  1,
			latency:      time.Millisecond,
			manual:       true,
		})
		restored := newTestMemoryFile(t, testMemoryFileOpts{})
		l := startLoad(t, img, r, restored)
		t.Cleanup(func() { releaseAll(t, restored) })
		restored.EnableDirtyTracking()
		r.finish()
		if err := l.wait(t); err != nil {
			t.Fatalf("async page loading: %v", err)
		}
		checkDirty(t, "after loading", restored.SwapDirty(true), fr)
	})
}

// waitForRelease waits for f's releaser to release every waste page.
func waitForRelease(t *testing.T, f *MemoryFile) {
	t.Helper()
	deadline := time.Now().Add(testWaitTimeout)
	for {
		f.mu.Lock()
		haveWaste := f.haveWaste
		f.mu.Unlock()
		if !haveWaste {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for waste pages to be released")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSwapDirtyCarriesInternalMarks(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			f, fr := newTrackedMemoryFile(t)
			internal := pageRange(fr, 2, 2)
			marked := pageRange(fr, 8, 1)
			if _, err := f.MapInternal(internal, hostarch.Write); err != nil {
				t.Fatalf("MapInternal: %v", err)
			}
			f.MarkDirty(marked)
			checkDirty(t, "first swap", f.SwapDirty(paused), fr, internal, marked)
			// A writer that marked its pages with MapInternal before a swap
			// with writers running may write after it: the next swap
			// reports its pages again, unless writers were paused.
			if paused {
				checkDirty(t, "second swap", f.SwapDirty(true), fr)
			} else {
				checkDirty(t, "second swap", f.SwapDirty(true), fr, internal)
			}
			checkDirty(t, "third swap", f.SwapDirty(true), fr)
		})
	}
}

func TestAlwaysDirty(t *testing.T) {
	f, fr := newTrackedMemoryFile(t)
	ring := pageRange(fr, 16, 4)
	f.MarkAlwaysDirty(ring)
	f.MarkAlwaysDirty(ring)
	for i := 0; i < 3; i++ {
		checkDirty(t, fmt.Sprintf("swap %d", i), f.SwapDirty(i%2 == 0), fr, ring)
	}
	f.ClearAlwaysDirty(ring)
	checkDirty(t, "swap after one of two registrations is cleared", f.SwapDirty(true), fr, ring)
	f.ClearAlwaysDirty(ring)
	// The last writes may have happened just before the range was cleared.
	checkDirty(t, "swap after the last registration is cleared", f.SwapDirty(true), fr, ring)
	checkDirty(t, "next swap", f.SwapDirty(true), fr)
}

// TestUnswapDirty checks that a dirty set returned after a failed save is
// reported again, with the pages dirtied since, as Firecracker's
// test_snapshot_not_losing_dirty_pages checks for its dirty bitmap.
func TestUnswapDirty(t *testing.T) {
	f, fr := newTrackedMemoryFile(t)
	f.MarkDirty(pageRange(fr, 0, 2))
	s := f.SwapDirty(true)
	f.MarkDirty(pageRange(fr, 10, 1))
	f.UnswapDirty(s)
	checkDirty(t, "swap after unswap", f.SwapDirty(true), fr, pageRange(fr, 0, 2), pageRange(fr, 10, 1))
	checkDirty(t, "next swap", f.SwapDirty(true), fr)
}

// TestSwapDirtyConcurrentMarks checks that no mark is lost when swaps race
// with marks.
func TestSwapDirtyConcurrentMarks(t *testing.T) {
	f, fr := newTrackedMemoryFile(t)
	const (
		markers = 4
		marks   = 20000
	)
	var (
		wg     sync.WaitGroup
		marked [markers][]uint64
	)
	for m := 0; m < markers; m++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(m)))
			for i := 0; i < marks; i++ {
				p := uint64(rng.Intn(64))
				marked[m] = append(marked[m], p)
				f.MarkDirty(pageRange(fr, p, 1))
			}
		}()
	}
	var swapped []*DirtySet
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		swapped = append(swapped, f.SwapDirty(false))
	}
	swapped = append(swapped, f.SwapDirty(true))
	for m := range marked {
		for _, p := range marked[m] {
			off := pageRange(fr, p, 1).Start
			if !slices.ContainsFunc(swapped, func(s *DirtySet) bool { return s.Contains(off) }) {
				t.Fatalf("page %d marked by marker %d is in no swapped set", p, m)
			}
		}
	}
}

// TestVerifyDirtyPossiblyCommitted checks that verification hashes the pages
// that hold data without being known to be committed, as when tracking starts
// while tasks run, before any save: saving them, which makes them
// known-committed, does not change them.
func TestVerifyDirtyPossiblyCommitted(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocate(t, f, 4*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	patternPage(f.pageSlice(fr.Start+hostarch.PageSize), fr.Start+hostarch.PageSize, 1)
	f.EnableDirtyTracking()
	if err := f.RecordPageHashes(); err != nil {
		t.Fatalf("RecordPageHashes: %v", err)
	}
	if err := f.SaveTo(context.Background(), io.Discard, &SaveOpts{}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	v, err := f.VerifyDirty(f.SwapDirty(true))
	if err != nil {
		t.Fatalf("VerifyDirty: %v", err)
	}
	if v.Escapes != 0 {
		t.Errorf("%d escapes %v, want none", v.Escapes, v.First)
	}
}

// newVerifiedMemoryFile returns a MemoryFile with dirty tracking enabled and
// an allocation of 64 pages, written with a pattern, saved, and at the start
// of a verification epoch.
func newVerifiedMemoryFile(t *testing.T) (*MemoryFile, memmap.FileRange) {
	t.Helper()
	f, fr := newTrackedMemoryFile(t)
	writePattern(t, f, fr, 1)
	if err := f.SaveTo(context.Background(), io.Discard, &SaveOpts{}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	f.SwapDirty(true)
	if err := f.RecordPageHashes(); err != nil {
		t.Fatalf("RecordPageHashes: %v", err)
	}
	return f, fr
}

// verifyEpoch ends the verification epoch of f as a save does: it saves f,
// swaps its dirty set with writers paused, and verifies it. It returns the
// offsets of the escapes, and starts the next epoch.
func verifyEpoch(t *testing.T, f *MemoryFile) []uint64 {
	t.Helper()
	if err := f.SaveTo(context.Background(), io.Discard, &SaveOpts{}); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	v, err := f.VerifyDirty(f.SwapDirty(true))
	if err != nil {
		t.Fatalf("VerifyDirty: %v", err)
	}
	if v.Escapes > maxDirtyEscapesReported {
		t.Fatalf("%d escapes, more than the %d reported", v.Escapes, maxDirtyEscapesReported)
	}
	var escapes []uint64
	for _, e := range v.First {
		escapes = append(escapes, e.Offset)
		if e.Kind != usage.Anonymous {
			t.Errorf("escape %v has kind %v, want %v", e, e.Kind, usage.Anonymous)
		}
	}
	v.Commit()
	return escapes
}

// pageOffsets returns the offset of every page of fr.
func pageOffsets(fr memmap.FileRange) []uint64 {
	var offs []uint64
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		offs = append(offs, off)
	}
	return offs
}

// disableDirtyMarkPath disables p until the end of the test.
func disableDirtyMarkPath(t *testing.T, p DirtyMarkPath) {
	t.Helper()
	TestOnlyDisableDirtyMarkPath(p)
	t.Cleanup(func() { TestOnlyDisableDirtyMarkPath(DirtyMarkNone) })
}

func TestVerifyDirty(t *testing.T) {
	f, fr := newVerifiedMemoryFile(t)
	saveAndVerify := func(what string, wantEscapes ...uint64) {
		t.Helper()
		if got := verifyEpoch(t, f); !slices.Equal(got, wantEscapes) {
			t.Errorf("%s: escapes %#x, want %#x", what, got, wantEscapes)
		}
	}
	saveAndVerify("no writes")

	// Writes through MapInternal and decommits are not escapes.
	ims, err := f.MapInternal(pageRange(fr, 3, 1), hostarch.Write)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	if _, err := safemem.ZeroSeq(ims); err != nil {
		t.Fatalf("ZeroSeq: %v", err)
	}
	f.Decommit(pageRange(fr, 4, 1))
	saveAndVerify("marked writes")

	// A write the MemoryFile does not see, through its file, is an escape
	// unless marked.
	pwritePage(t, f, fr, 5, 1)
	pwritePage(t, f, fr, 6, 1)
	f.MarkDirty(pageRange(fr, 6, 1))
	saveAndVerify("unmarked write", pageRange(fr, 5, 1).Start)

	// The unmarked write's page was recorded with its new contents: an
	// unchanged page is not an escape again.
	saveAndVerify("after the escape")

	// A page allocated and written during an epoch without a mark is an
	// escape: it was zero when the epoch started.
	extra := allocate(t, f, 2*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(extra) })
	if _, err := unix.Pwrite(f.FD(), []byte{1}, int64(extra.Start+hostarch.PageSize)); err != nil {
		t.Fatalf("Pwrite: %v", err)
	}
	saveAndVerify("unmarked write to a new allocation", extra.Start+hostarch.PageSize)
}

// TestDirtyMarkNegativeControls checks that verification catches the writes
// of each path by which a MemoryFile marks pages when the path is disabled,
// and only then: each mark is needed, and its absence would not go unnoticed.
func TestDirtyMarkNegativeControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		path DirtyMarkPath
		// write changes pages of fr, which f allocated, and returns the
		// offsets of the pages it changed.
		write func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64
	}{
		{
			name: "MapInternal",
			path: DirtyMarkMapInternal,
			write: func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64 {
				written := pageRange(fr, 3, 2)
				ims, err := f.MapInternal(written, hostarch.Write)
				if err != nil {
					t.Fatalf("MapInternal: %v", err)
				}
				if _, err := safemem.ZeroSeq(ims); err != nil {
					t.Fatalf("ZeroSeq: %v", err)
				}
				return pageOffsets(written)
			},
		},
		{
			name: "Decommit",
			path: DirtyMarkDecommit,
			write: func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64 {
				decommitted := pageRange(fr, 10, 2)
				f.Decommit(decommitted)
				return pageOffsets(decommitted)
			},
		},
		{
			// Pages released and allocated again read as zeroes, as on a
			// workload that frees memory and maps new memory that it only
			// reads.
			name: "release",
			path: DirtyMarkDecommit,
			write: func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64 {
				freed := pageRange(fr, 60, 4)
				f.DecRef(freed)
				waitForRelease(t, f)
				// fr's cleanup releases the pages again.
				if again := allocate(t, f, freed.Length(), AllocOpts{Kind: usage.Anonymous}); again != freed {
					t.Fatalf("pages %v allocated again as %v", freed, again)
				}
				return pageOffsets(freed)
			},
		},
	} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disabled=%v", tc.name, disabled), func(t *testing.T) {
				f, fr := newVerifiedMemoryFile(t)
				if disabled {
					disableDirtyMarkPath(t, tc.path)
				}
				written := tc.write(t, f, fr)
				var want []uint64
				if disabled {
					want = written
				}
				if got := verifyEpoch(t, f); !slices.Equal(got, want) {
					t.Errorf("escapes %#x, want %#x", got, want)
				}
			})
		}
	}
}

// BenchmarkMapInternal measures the cost of dirty tracking on MapInternal,
// the hottest path through pgalloc.
func BenchmarkMapInternal(b *testing.B) {
	for _, bc := range []struct {
		name    string
		at      hostarch.AccessType
		tracked bool
	}{
		{"read", hostarch.Read, false},
		{"write", hostarch.Write, false},
		{"read tracked", hostarch.Read, true},
		{"write tracked", hostarch.Write, true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			f := newTestMemoryFile(b, testMemoryFileOpts{})
			fr := allocate(b, f, 64*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
			b.Cleanup(func() { f.DecRef(fr) })
			if bc.tracked {
				f.EnableDirtyTracking()
			}
			page := memmap.FileRange{fr.Start, fr.Start + hostarch.PageSize}
			for b.Loop() {
				if _, err := f.MapInternal(page, bc.at); err != nil {
					b.Fatalf("MapInternal: %v", err)
				}
			}
		})
	}
}
