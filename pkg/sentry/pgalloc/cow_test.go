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
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sync"
)

// cowWriter writes pages of fr after a save point, as one of the writers that
// copy-on-write saves capture before, and returns the pages it changed.
type cowWriter struct {
	name string
	path DirtyMarkPath
	// write changes pages of fr, which f allocated, after f was frozen, and
	// returns the offsets of the pages it changed.
	write func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64
}

var cowWriters = []cowWriter{
	{
		// The Sentry's own writes: page cache fills, copy-on-write breaks,
		// CopyOut through MemoryManagers' internal mappings.
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
		// The application's first write to write-protected memory, which
		// the MemoryManager marks before it makes the memory writable.
		name: "fault",
		path: DirtyMarkWriteProtectFault,
		write: func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64 {
			written := pageRange(fr, 16, 16)
			f.MarkDirtyBy(DirtyMarkWriteProtectFault, written)
			for _, off := range pageOffsets(written) {
				fillPage(f, off, 7)
			}
			return pageOffsets(written)
		},
	},
	{
		name: "Decommit",
		path: DirtyMarkDecommit,
		write: func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64 {
			decommitted := pageRange(fr, 40, 2)
			f.Decommit(decommitted)
			return pageOffsets(decommitted)
		},
	},
	{
		// Pages freed after the save point, released, and allocated again.
		name: "release",
		path: DirtyMarkDecommit,
		write: func(t *testing.T, f *MemoryFile, fr memmap.FileRange) []uint64 {
			freed := pageRange(fr, 60, 4)
			f.DecRef(freed)
			waitForRelease(t, f)
			if again := allocate(t, f, freed.Length(), AllocOpts{Kind: usage.Anonymous}); again != freed {
				t.Fatalf("pages %v allocated again as %v", freed, again)
			}
			return pageOffsets(freed)
		},
	},
}

// newCoWTestFile returns an imageTestFile of 64 pages, tracked, as
// copy-on-write saves require.
func newCoWTestFile(t *testing.T) *imageTestFile {
	t.Helper()
	tf := newImageTestFile(t, 64)
	tf.f.EnableDirtyTracking()
	tf.f.SwapDirty(true /* paused */)
	return tf
}

// contents returns a copy of the pages of fr in f.
func contents(f *MemoryFile, fr memmap.FileRange) []byte {
	var b []byte
	f.forEachMappingSlice(fr, func(bs []byte) {
		b = append(b, bs...)
	})
	return b
}

// checkContents checks that the pages of fr in f are want.
func checkContents(t *testing.T, f *MemoryFile, fr memmap.FileRange, want []byte) {
	t.Helper()
	if err := f.AwaitLoadAll(); err != nil {
		t.Fatalf("AwaitLoadAll: %v", err)
	}
	got := contents(f, fr)
	bad := 0
	for i := 0; i < len(want); i += page {
		if !bytes.Equal(got[i:i+page], want[i:i+page]) {
			if bad < 8 {
				t.Errorf("page %#x: got %s, want %s", fr.Start+uint64(i), describePage(got[i:i+page]), describePage(want[i:i+page]))
			}
			bad++
		}
	}
	if bad != 0 {
		t.Errorf("%d pages differ from the save point", bad)
	}
}

// dirtySetOf returns the DirtySet of the pages of f that tf modified.
func dirtySetOf(tf *imageTestFile) *DirtySet {
	s := &DirtySet{words: make([]uint64, len(tf.f.chunksLoad())*dirtyChunkWords)}
	for off := range tf.modified {
		s.add(memmap.FileRange{off, off + page})
	}
	return s
}

// freezeCoW freezes tf.f for a copy-on-write save, a delta of parent if parent
// is not nil, and returns the options to save it with.
func freezeCoW(t *testing.T, tf *imageTestFile, parent *testImage, opts CoWOpts) SaveOpts {
	t.Helper()
	saveOpts := SaveOpts{PageHashes: true}
	var dirty *DirtySet
	if parent != nil {
		saveOpts.Clean = tf.clean
		saveOpts.Base = parent.mfImage()
		dirty = dirtySetOf(tf)
	}
	if err := tf.f.FreezeForCoWSave(&saveOpts, dirty, opts); err != nil {
		t.Fatalf("FreezeForCoWSave: %v", err)
	}
	return saveOpts
}

// TestCoWSaveIsSavePoint checks that a copy-on-write save, whole or a delta,
// saves the memory of its save point although every kind of writer changes
// it before the copier reads it, and that its image is the image that a save
// of the same moment writes, but for where in its pages file it puts pages.
func TestCoWSaveIsSavePoint(t *testing.T) {
	for _, delta := range []bool{false, true} {
		t.Run(fmt.Sprintf("delta=%v", delta), func(t *testing.T) {
			tf := newCoWTestFile(t)
			var parent *testImage
			if delta {
				parent = saveTestImage(t, tf.f, nil, nil)
				tf.write(1, 1)
				tf.write(3, 1)
				tf.zero(4)
				tf.rewrite(5)
				tf.write(20, 1)
				tf.write(61, 1)
			}
			// A save of the save point, for comparison, and its memory.
			var reference *testImage
			if delta {
				reference = saveTestImage(t, tf.f, parent, tf.clean)
			} else {
				reference = saveTestImage(t, tf.f, nil, nil)
			}
			want := contents(tf.f, tf.fr)

			opts := freezeCoW(t, tf, parent, CoWOpts{StagingMax: math.MaxUint64, Verify: true})
			var written []uint64
			for _, w := range cowWriters {
				written = append(written, w.write(t, tf.f, tf.fr)...)
			}
			ti := newTestPagesWriter(t).save(t, tf.f, parent, opts)
			// The next epoch holds every page written after the save point,
			// those freed, released and allocated again included, whether the
			// copier or a writer captured them.
			next := tf.f.SwapDirty(true /* paused */)
			for _, off := range written {
				if !next.Contains(off) {
					t.Errorf("page %#x, written after the save point, is not dirty in the next epoch", off)
				}
			}

			restored := loadTestImage(t, ti)
			checkContents(t, restored, tf.fr, want)
			gm, wm := ti.mfImage(), reference.mfImage()
			for off := tf.fr.Start; off < tf.fr.End; off += page {
				gh, gok := gm.HashAt(off)
				wh, wok := wm.HashAt(off)
				if gok != wok || gh != wh {
					t.Errorf("page %#x: hash %#x, %t; the reference's %#x, %t", off, gh, gok, wh, wok)
				}
				ge, gext := gm.ExtentAt(off)
				we, wext := wm.ExtentAt(off)
				if gext != wext || (gext && ge.Layer != we.Layer) {
					t.Errorf("page %#x: extent %+v, %t; the reference's %+v, %t", off, ge, gext, we, wext)
				}
			}
			if got, want := committedOf(ti), committedOf(reference); !slices.Equal(got, want) {
				t.Errorf("known-committed ranges %v, the reference's %v", got, want)
			}
			if got, max := uint64(len(ti.pages)), uint64(len(reference.pages)); got != max {
				t.Errorf("wrote %d bytes of pages, the reference %d", got, max)
			}
		})
	}
}

// committedOf returns the known-committed ranges of ti's MemoryFile, merged.
func committedOf(ti *testImage) []memmap.FileRange {
	var frs []memmap.FileRange
	for _, s := range ti.img.MemoryFiles[0].MemAcct {
		if !s.KnownCommitted {
			continue
		}
		if n := len(frs); n != 0 && frs[n-1].End == s.Start {
			frs[n-1].End = s.End
		} else {
			frs = append(frs, memmap.FileRange{s.Start, s.End})
		}
	}
	return frs
}

// TestCoWSaveNegativeControls checks that CoWOpts.Verify catches the writes
// of each writer when its captures are disabled, and only then.
func TestCoWSaveNegativeControls(t *testing.T) {
	for _, w := range cowWriters {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disabled=%v", w.name, disabled), func(t *testing.T) {
				tf := newCoWTestFile(t)
				// Every written page holds data at the save point.
				for _, off := range pageOffsets(tf.fr) {
					fillPage(tf.f, off, 2)
				}
				opts := freezeCoW(t, tf, nil, CoWOpts{StagingMax: math.MaxUint64, Verify: true})
				if disabled {
					TestOnlyDisableCoWCapturePath(w.path)
					t.Cleanup(func() { TestOnlyDisableCoWCapturePath(DirtyMarkNone) })
				}
				w.write(t, tf.f, tf.fr)
				TestOnlyDisableCoWCapturePath(DirtyMarkNone)
				pw := newTestPagesWriter(t)
				ip := &pgallocpb.ImageProto{Layers: []*pgallocpb.LayerProto{{}}, PageHash: checkpointimage.PageHashXXH64}
				opts.PagesFile = pw.apfs
				err := tf.f.SaveTo(context.Background(), checkpointimage.NewWriter(&bytes.Buffer{}, ip), &opts)
				pw.apfs.MemoryFilesDone()
				<-pw.done
				if disabled != errors.Is(err, ErrCoWEscapes) {
					t.Errorf("SaveTo: %v, want ErrCoWEscapes: %t", err, disabled)
				}
			})
		}
	}
}

// TestCoWSaveStagingMax checks that writers that copy more than
// CoWOpts.StagingMax wait for the copier to write what they staged, and that
// the save is still of its save point.
func TestCoWSaveStagingMax(t *testing.T) {
	tf := newCoWTestFile(t)
	want := contents(tf.f, tf.fr)
	// The copier starts late, so that the writer stages pages and waits for
	// them to be written.
	opts := freezeCoW(t, tf, nil, CoWOpts{StagingMax: 2 * page, Verify: true, CopyDelay: 100 * time.Millisecond})
	c := tf.f.cow.Load()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, off := range pageOffsets(tf.fr) {
			tf.f.MarkDirtyBy(DirtyMarkWriteProtectFault, memmap.FileRange{off, off + page})
			fillPage(tf.f, off, 9)
		}
	}()
	ti := newTestPagesWriter(t).save(t, tf.f, nil, opts)
	wg.Wait()
	c.mu.Lock()
	peak, waits := c.stagedPeak, c.markWaits
	c.mu.Unlock()
	if peak > 2*page {
		t.Errorf("staging peak %d bytes, want at most %d", peak, 2*page)
	}
	if waits == 0 {
		t.Errorf("the writer never waited for staged pages to be written")
	}
	checkContents(t, loadTestImage(t, ti), tf.fr, want)
}

// TestCoWSaveCancel checks that a cancelled copy-on-write save stops
// capturing, and that writers waiting for staging to drain return.
func TestCoWSaveCancel(t *testing.T) {
	tf := newCoWTestFile(t)
	freezeCoW(t, tf, nil, CoWOpts{StagingMax: page})
	// The first write is staged; the second waits for the copier, which
	// never runs.
	tf.f.MarkDirtyBy(DirtyMarkWriteProtectFault, pageRange(tf.fr, 0, 1))
	done := make(chan struct{})
	go func() {
		tf.f.MarkDirtyBy(DirtyMarkWriteProtectFault, pageRange(tf.fr, 1, 1))
		close(done)
	}()
	tf.f.CancelCoWSave()
	<-done
	if c := tf.f.cow.Load(); c != nil {
		t.Errorf("writers still capture after the save was cancelled")
	}
	// A save can start again.
	freezeCoW(t, tf, nil, CoWOpts{StagingMax: page})
	tf.f.CancelCoWSave()
}
