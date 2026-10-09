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
	"fmt"
	"testing"

	"github.com/cespare/xxhash/v2"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
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
	copied, err := p.CopyAll()
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
	if copied := p.Copy(tf.f.SwapDirty(false /* paused */)); copied != 3*page {
		t.Errorf("round 1 copied %d bytes, want %d", copied, 3*page)
	}
	precopyWait(t, p)
	for _, i := range []uint64{2, 10} {
		tf.writeDirty(i, 2)
	}

	// The save writes the pages written during round 1.
	last := tf.f.SwapDirty(true /* paused */)
	ti := pw.save(t, tf.f, nil, SaveOpts{
		Precopy:    p,
		Clean:      func(off uint64) bool { return !last.Contains(off) },
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
	copied, err := p.CopyAll()
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
				p.Copy(f.SwapDirty(false /* paused */))
			} else {
				if _, err := p.CopyAll(); err != nil {
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
			p.Copy(dirty)
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
				Clean:      func(off uint64) bool { return !last.Contains(off) },
				PageHashes: true,
			})
			checkSameContents(t, loadTestImage(t, ti), f, again)
		})
	}
}
