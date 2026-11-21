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
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sync"
)

// writeDirty writes generation gen to page i of tf's allocation and marks it
// dirty, as a dirty source reports an application write.
func (tf *imageTestFile) writeDirty(i, gen uint64) {
	tf.write(i, gen)
	off := tf.fr.Start + i*page
	tf.f.MarkDirty(memmap.FileRange{Start: off, End: off + page})
}

// startTestPrecopy starts a pre-copy of f, whose dirty tracking it enables,
// to a pages file in memory.
func startTestPrecopy(t *testing.T, f *MemoryFile) (*Precopy, *testPagesWriter) {
	t.Helper()
	f.EnableDirtyTracking()
	pw := newTestPagesWriter(t)
	p, err := f.StartPrecopy(pw.apfs)
	if err != nil {
		t.Fatalf("StartPrecopy: %v", err)
	}
	return p, pw
}

func precopyWait(t *testing.T, p *Precopy) {
	t.Helper()
	if err := p.Wait(); err != nil {
		t.Fatalf("Precopy.Wait: %v", err)
	}
}

// TestPrecopy checks that a save completing a pre-copy writes only the pages
// dirtied during its last round, refers to the last copy of every other page,
// and restores the MemoryFile's contents.
func TestPrecopy(t *testing.T) {
	const n = 64
	tf := newImageTestFile(t, n)
	p, pw := startTestPrecopy(t, tf.f)

	// Round 0 copies every committed page.
	copied, err := p.CopyAll(time.Time{})
	if err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	if copied != n*page {
		t.Errorf("CopyAll copied %d bytes, want %d", copied, n*page)
	}
	tf.f.SwapDirty(false /* paused */)
	precopyWait(t, p)
	for _, i := range []uint64{1, 2, 3} {
		tf.writeDirty(i, 1)
	}

	// Round 1 copies the pages written during round 0.
	if copied, err := p.Copy(tf.f.SwapDirty(false /* paused */), time.Time{}); err != nil || copied != 3*page {
		t.Errorf("round 1 copied %d bytes, error %v; want %d, nil", copied, err, 3*page)
	}
	precopyWait(t, p)
	for _, i := range []uint64{2, 10} {
		tf.writeDirty(i, 2)
	}

	// The save writes the pages written during round 1.
	last := tf.f.SwapDirty(true /* paused */)
	ti := pw.save(t, tf.f, nil, SaveOpts{
		Precopy:    p,
		Dirty:      last,
		PageHashes: true,
	})
	if got, want := uint64(len(ti.pages)), uint64(n+3+2)*page; got != want {
		t.Errorf("pages file: got %d bytes, want %d", got, want)
	}
	m := ti.mfImage()
	for i := uint64(0); i < n; i++ {
		off := tf.fr.Start + i*page
		e, ok := m.ExtentAt(off)
		if !ok {
			t.Fatalf("page %d has no extent", i)
		}
		// Where the page's last copy is: round 0, round 1 or the save.
		var wantRegion uint64
		switch i {
		case 1, 3:
			wantRegion = 1
		case 2, 10:
			wantRegion = 2
		}
		region := uint64(0)
		if po := e.OffsetOf(off); po >= (n+3)*page {
			region = 2
		} else if po >= n*page {
			region = 1
		}
		if region != wantRegion {
			t.Errorf("page %d is at pages file offset %#x, of round %d; want round %d", i, e.OffsetOf(off), region, wantRegion)
		}
		if h, ok := m.HashAt(off); !ok || h != xxhash.Sum64(tf.f.pageSlice(off)) {
			t.Errorf("page %d: hash %#x (%t), want the hash of its contents", i, h, ok)
		}
	}
	checkSameContents(t, loadTestImage(t, ti), tf.f, tf.fr)
}

// TestPrecopyCopyAllSkipsHoles checks that the first round does not copy the
// holes of possibly-committed ranges.
func TestPrecopyCopyAllSkipsHoles(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr, err := f.Allocate(16*page, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	fillPage(f, fr.Start+5*page, 0)
	p, _ := startTestPrecopy(t, f)
	copied, err := p.CopyAll(time.Time{})
	if err != nil {
		t.Fatalf("CopyAll: %v", err)
	}
	if copied != page {
		t.Errorf("CopyAll copied %d bytes, want %d", copied, page)
	}
	precopyWait(t, p)
}

// TestPrecopyReallocatedPage checks that a page dirtied while it was not
// allocated, so that no round copied its contents, is read by the save, even
// if nothing dirtied it since: here, it is reallocated and only read, which
// commits it. With a parent image, the page is not in any round's copy, and
// must not refer to the parent either.
func TestPrecopyReallocatedPage(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		t.Run(fmt.Sprintf("incremental=%t", incremental), func(t *testing.T) {
			tf := newImageTestFile(t, 4)
			f := tf.f
			freed, err := f.Allocate(page, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
			if err != nil {
				t.Fatalf("Allocate: %v", err)
			}
			fillPage(f, freed.Start, 7)
			f.EnableDirtyTracking()
			var parent *testImage
			if incremental {
				parent = saveTestImage(t, f, nil, nil)
			}
			p, pw := startTestPrecopy(t, f)
			if incremental {
				if _, err := p.Copy(f.SwapDirty(false /* paused */), time.Time{}); err != nil {
					t.Fatalf("Copy: %v", err)
				}
			} else {
				if _, err := p.CopyAll(time.Time{}); err != nil {
					t.Fatalf("CopyAll: %v", err)
				}
				f.SwapDirty(false /* paused */)
			}
			precopyWait(t, p)

			// Free the page; round 1 finds it dirty and not allocated.
			f.DecRef(freed)
			waitForRelease(t, f)
			dirty := f.SwapDirty(false /* paused */)
			if !dirty.Contains(freed.Start) {
				t.Fatalf("freed page %#x is not dirty", freed.Start)
			}
			if _, err := p.Copy(dirty, time.Time{}); err != nil {
				t.Fatalf("Copy: %v", err)
			}
			precopyWait(t, p)

			// Allocate it again and read it, which commits it without
			// writing it; UpdateUsage then finds it committed.
			again, err := f.Allocate(page, AllocOpts{Kind: usage.Anonymous})
			if err != nil {
				t.Fatalf("Allocate: %v", err)
			}
			if again != freed {
				t.Fatalf("Allocate returned %v, want the freed page %v", again, freed)
			}
			if b := f.pageSlice(again.Start)[0]; b != 0 {
				t.Fatalf("reallocated page starts with %#x, want 0", b)
			}
			if err := f.UpdateUsage(nil); err != nil {
				t.Fatalf("UpdateUsage: %v", err)
			}
			f.mu.Lock()
			seg := f.memAcct.FindSegment(again.Start)
			committed := seg.Ok() && seg.ValuePtr().knownCommitted
			f.mu.Unlock()
			if !committed {
				t.Fatalf("reallocated page %#x is not known-committed after being read: the test needs it to be", again.Start)
			}
			last := f.SwapDirty(true /* paused */)
			if last.Contains(again.Start) {
				t.Fatalf("reallocated page %#x is dirty: the test needs it clean", again.Start)
			}
			ti := pw.save(t, f, parent, SaveOpts{
				Precopy:    p,
				Dirty:      last,
				PageHashes: true,
			})
			checkSameContents(t, loadTestImage(t, ti), f, again)
		})
	}
}

// TestPrecopyDuringLoadCopiesDirtyPagesLoaded checks that a round of a
// pre-copy of a MemoryFile that is still loading the image it was restored
// from copies the dirty pages that have not been loaded once they are: a page
// dirtied by a mark coarser than the writes it records, which holds the data
// loading writes, and a page decommitted while it loads, which holds zeros.
// The save that completes the pre-copy refers to their copies.
func TestPrecopyDuringLoadCopiesDirtyPagesLoaded(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	const (
		marked      = 1
		decommitted = 2
	)
	tf.zero(decommitted)
	pageAt := func(i uint64) memmap.FileRange {
		return memmap.FileRange{Start: tf.fr.Start + i*page, End: tf.fr.Start + (i+1)*page}
	}

	gate := &gatedReader{r: bytes.NewReader(parent.layers[0]), gate: make(chan struct{})}
	f2 := loadTestImage(t, parent, stateio.NewIOReader(gate, 64<<10, 16, 4))
	var openGate sync.Once
	defer openGate.Do(func() { close(gate.gate) })
	p, pw := startTestPrecopy(t, f2)
	f2.MarkDirty(pageAt(marked))
	f2.Decommit(pageAt(decommitted))
	dirty := f2.SwapDirty(false /* paused */)
	for _, i := range []uint64{marked, decommitted} {
		if !dirty.Contains(pageAt(i).Start) {
			t.Fatalf("page %d is not dirty", i)
		}
	}
	copied := make(chan uint64, 1)
	go func() {
		n, _ := p.Copy(dirty, time.Time{})
		copied <- n
	}()
	select {
	case <-copied:
		t.Fatalf("Precopy.Copy returned before the pages it copies were loaded")
	case <-time.After(50 * time.Millisecond):
	}
	openGate.Do(func() { close(gate.gate) })
	select {
	case n := <-copied:
		if n != 2*page {
			t.Errorf("Precopy.Copy copied %d bytes, want %d", n, 2*page)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Precopy.Copy did not return after loading")
	}
	precopyWait(t, p)

	last := f2.SwapDirty(true /* paused */)
	ti := pw.save(t, f2, parent, SaveOpts{
		Precopy:    p,
		Dirty:      last,
		PageHashes: true,
	})
	m := ti.mfImage()
	for _, i := range []uint64{marked, decommitted} {
		e, ok := m.ExtentAt(pageAt(i).Start)
		if !ok || e.Layer != 0 {
			t.Errorf("page %d: extent %+v (%t), want its copy in layer 0", i, e, ok)
		}
	}
	checkSameContents(t, loadTestImage(t, ti), tf.f, tf.fr)
	checkSameContents(t, f2, tf.f, tf.fr)
}

// TestPrecopyForEachUnclean checks that the pages a save completing a
// pre-copy finds unclean, range by range, as it does among those still
// loading, are those it finds unclean page by page: the dirty pages, and the
// pages whose contents the pre-copy does not know, among them those beyond
// its limit.
func TestPrecopyForEachUnclean(t *testing.T) {
	const pages = 1024
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 50; round++ {
		limit := uint64(rng.Intn(pages)) * page
		p := &Precopy{limit: limit, copies: make([]uint64, limit/page), hashes: make([]uint64, limit/page)}
		for i := range p.copies {
			switch rng.Intn(3) {
			case 0:
				p.copies[i] = copyUnknown
			case 1:
				p.copies[i] = 1 + uint64(i)*page
			}
		}
		dirty := &DirtySet{words: make([]uint64, pages/64)}
		for i := 0; i < pages/8; i++ {
			off := uint64(rng.Intn(pages)) * page
			dirty.add(memmap.FileRange{Start: off, End: off + page})
		}
		s := &imageSaver{dirty: dirty, precopy: p}
		fr := memmap.FileRange{Start: uint64(rng.Intn(pages/2)) * page, End: uint64(pages/2+rng.Intn(pages/2)) * page}
		var got []uint64
		last := fr.Start
		s.forEachUnclean(fr, func(ufr memmap.FileRange) {
			if ufr.Start < last || ufr.Length() == 0 || !fr.IsSupersetOf(ufr) {
				t.Fatalf("round %d: range %v out of order, empty or outside %v", round, ufr, fr)
			}
			last = ufr.End
			for off := ufr.Start; off < ufr.End; off += page {
				got = append(got, off)
			}
		})
		var want []uint64
		for off := fr.Start; off < fr.End; off += page {
			if !s.isClean(off) {
				want = append(want, off)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("round %d: unclean pages %v, want %v", round, got, want)
		}
	}
}

// TestPrecopyLoadWait checks that a pre-copy round whose wait for the dirty
// pages it copies that are still loading reaches its deadline fails with
// ErrLoadWaitExpired, copying nothing, and that a round once they have loaded
// copies them; and that a first round's wait for every page fails likewise.
func TestPrecopyLoadWait(t *testing.T) {
	tf := newImageTestFile(t, 8)
	parent := saveTestImage(t, tf.f, nil, nil)
	pageAt := func(i uint64) memmap.FileRange {
		return memmap.FileRange{Start: tf.fr.Start + i*page, End: tf.fr.Start + (i+1)*page}
	}
	gate := &gatedReader{r: bytes.NewReader(parent.layers[0]), gate: make(chan struct{})}
	f2 := loadTestImage(t, parent, stateio.NewIOReader(gate, 64<<10, 16, 4))
	var openGate sync.Once
	defer openGate.Do(func() { close(gate.gate) })
	p, _ := startTestPrecopy(t, f2)

	if n, err := p.CopyAll(time.Now().Add(100 * time.Millisecond)); !errors.Is(err, ErrLoadWaitExpired) || n != 0 {
		t.Errorf("CopyAll of a MemoryFile loading: copied %d bytes, error %v; want 0, ErrLoadWaitExpired", n, err)
	}
	f2.MarkDirty(pageAt(1))
	dirty := f2.SwapDirty(false /* paused */)
	if n, err := p.Copy(dirty, time.Now().Add(100*time.Millisecond)); !errors.Is(err, ErrLoadWaitExpired) || n != 0 {
		t.Errorf("Copy of a dirty page loading: copied %d bytes, error %v; want 0, ErrLoadWaitExpired", n, err)
	}
	openGate.Do(func() { close(gate.gate) })
	if n, err := p.Copy(dirty, time.Now().Add(10*time.Second)); err != nil || n != page {
		t.Errorf("Copy once loaded: copied %d bytes, error %v; want %d, nil", n, err, page)
	}
	precopyWait(t, p)
}
