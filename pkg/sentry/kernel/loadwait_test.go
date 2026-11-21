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

package kernel

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
)

// gatedReaderAt reads from f once gate is closed.
type gatedReaderAt struct {
	f    *os.File
	gate chan struct{}
}

// ReadAt implements io.ReaderAt.ReadAt.
func (g *gatedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	<-g.gate
	return g.f.ReadAt(p, off)
}

// lazyLoadImage restores img, saved in imageDir, into a new MemoryFile whose
// pages load in the background once gate is closed, and returns it once its
// loading has begun.
func lazyLoadImage(t *testing.T, ctx context.Context, img *checkpointimage.Image, imageDir string, gate chan struct{}) *pgalloc.MemoryFile {
	t.Helper()
	pages, err := os.Open(filepath.Join(imageDir, checkpointfiles.PagesFileName))
	if err != nil {
		t.Fatalf("opening the pages file: %v", err)
	}
	memfd, err := memutil.CreateMemFD("restored", 0)
	if err != nil {
		t.Fatalf("CreateMemFD: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(os.NewFile(uintptr(memfd), "restored"), pgalloc.MemoryFileOpts{DisableMemoryAccounting: true})
	if err != nil {
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(mf.Destroy)
	reader := stateio.NewIOReader(&gatedReaderAt{f: pages, gate: gate}, 64<<10, 16, 4)
	mfl := NewAsyncMFLoader(img, []stateio.AsyncReader{reader}, mf, pgalloc.PrefetchAuto, nil /* timeline */, pgalloc.AsyncPagesFileLoadOpts{})
	mfl.KickoffPrivate(ctx, nil)
	if err := mfl.WaitMainMFStart(); err != nil {
		t.Fatalf("starting to load %v: %v", img.Digest, err)
	}
	// Before the MemoryFile is destroyed: the test opens the gate.
	t.Cleanup(func() {
		if err := mfl.Wait(); err != nil {
			t.Errorf("loading %v: %v", img.Digest, err)
		}
		pages.Close()
	})
	return mf
}

// TestSaveLoadWait checks the waits of a save with a load wait for the pages
// it reads that are still loading, as a sandbox restored in the background
// has them: before the save, while the sandbox runs, a save that is not
// incremental waits for every page, and an incremental one for the dirty
// pages only, none of which are loading unless something dirtied them as
// they loaded; in the save, the wait for the pages it reads; each fails with
// pgalloc.ErrLoadWaitExpired at its deadline, the save then keeping the pages
// it would have saved dirty for the next.
func TestSaveLoadWait(t *testing.T) {
	ctx := contexttest.Context(t)
	k0, _, fr := dirtyTestKernel(t, ctx, false /* verify */)
	img, dir, err := saveImage(t, ctx, k0, nil, noLimit)
	if err != nil {
		t.Fatalf("saving the image: %v", err)
	}
	gate := make(chan struct{})
	var openGate sync.Once
	defer openGate.Do(func() { close(gate) })
	mf := lazyLoadImage(t, ctx, img, dir, gate)
	k := &Kernel{mf: mf}
	k.SetDirtyTracking(DirtyTrackingOpts{Sources: []DirtySource{&testDirtySource{mf: mf}}})
	k.dirty.last = img
	if err := k.trackDirty(ctx, []*pgalloc.MemoryFile{mf}); err != nil {
		t.Fatalf("trackDirty: %v", err)
	}

	const wait = 100 * time.Millisecond
	expires := func(what string, await func(deadline time.Time) error) error {
		t.Helper()
		start := time.Now()
		err := await(start.Add(wait))
		if !errors.Is(err, pgalloc.ErrLoadWaitExpired) {
			t.Errorf("%s: %v, want pgalloc.ErrLoadWaitExpired", what, err)
		} else if took := time.Since(start); took < wait {
			t.Errorf("%s: expired after %v, before its deadline", what, took)
		}
		return err
	}
	expires("the wait of a save that is not incremental", func(deadline time.Time) error {
		return k.AwaitSaveLoads(ctx, false /* incremental */, deadline)
	})
	if err := k.AwaitSaveLoads(ctx, true /* incremental */, time.Now().Add(10*time.Second)); err != nil {
		t.Errorf("the wait of an incremental save, with no dirty page loading: %v", err)
	}

	// A page dirtied as it loads, as a mark coarser than the writes it
	// records dirties it.
	dirtied := memmap.FileRange{Start: fr.Start + 3*hostarch.PageSize, End: fr.Start + 4*hostarch.PageSize}
	mf.MarkDirty(dirtied)
	expires("the wait of an incremental save, with a dirty page loading", func(deadline time.Time) error {
		return k.AwaitSaveLoads(ctx, true /* incremental */, deadline)
	})
	e, lastRound, delta, err := k.beginSave(ctx, &img.Digest, nil /* precopy */)
	if err != nil {
		t.Fatalf("beginSave: %v", err)
	}
	saveErr := expires("the wait in the incremental save", func(deadline time.Time) error {
		return awaitSaveLoads(k.mf, nil /* mfsToSave */, true /* havePagesFile */, delta, nil /* precopy */, lastRound, deadline)
	})
	if err := k.endDirtySave(ctx, e, []*pgalloc.MemoryFile{mf}, nil /* image */, saveErr); !errors.Is(err, pgalloc.ErrLoadWaitExpired) {
		t.Errorf("endDirtySave: %v, want the save's pgalloc.ErrLoadWaitExpired", err)
	}
	if got, want := mf.DirtyBytes(), dirtied.Length(); got != want {
		t.Errorf("%d bytes are dirty after the save failed, want %d", got, want)
	}

	openGate.Do(func() { close(gate) })
	if err := k.AwaitSaveLoads(ctx, false /* incremental */, time.Now().Add(10*time.Second)); err != nil {
		t.Errorf("the wait of a save once the pages load: %v", err)
	}
	for i := uint64(0); i < 16; i++ {
		if got, want := pread(t, mf, fr.Start+i*hostarch.PageSize), byte(i+1); got != want {
			t.Errorf("page %d holds %d, want %d", i, got, want)
		}
	}
}
