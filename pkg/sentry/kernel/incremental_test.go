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
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

// noLimit tells saveImage not to limit the size of the pages file.
const noLimit = -1

// fullDiskWriter writes to f until it has written limit bytes, then fails
// with ENOSPC, as writes to a full disk do.
type fullDiskWriter struct {
	f     *os.File
	limit int
}

// Write implements io.Writer.Write.
func (w *fullDiskWriter) Write(b []byte) (int, error) {
	if len(b) <= w.limit {
		n, err := w.f.Write(b)
		w.limit -= n
		return n, err
	}
	n, err := w.f.Write(b[:w.limit])
	w.limit -= n
	if err == nil {
		err = unix.ENOSPC
	}
	return n, err
}

// Close implements io.Closer.Close.
func (w *fullDiskWriter) Close() error {
	return w.f.Close()
}

// saveImage saves k's MemoryFile as an image with a pages file in a new
// directory, incrementally if parent is not nil, as Kernel.saveToLocked does.
// Unless limit is noLimit, writing more than limit bytes to the pages file
// fails with ENOSPC, the pages being written one at a time. It returns the
// image, its directory and the error of the save.
func saveImage(t *testing.T, ctx context.Context, k *Kernel, parent *checkpointimage.Digest, limit int) (*checkpointimage.Image, string, error) {
	t.Helper()
	return saveImageOpts(t, ctx, k, testSaveOpts{parent: parent, fullDisk: limit != noLimit, limit: limit})
}

// testSaveOpts configures saveImageOpts. Its zero value is a full save of k's
// MemoryFile to a pages file without limit.
type testSaveOpts struct {
	// parent is the parent of an incremental save.
	parent *checkpointimage.Digest

	// If fullDisk is true, writing more than limit bytes to the pages file
	// fails with ENOSPC, the pages being written one at a time.
	fullDisk bool
	limit    int

	// If writeRate is not 0, the pages file is written at writeRate bytes
	// per second.
	writeRate uint64

	// privates are the private MemoryFiles that the save saves, by owner,
	// after k's.
	privates map[checkpoint.ResourceID]*pgalloc.MemoryFile

	// If precopy is not nil, the save pre-copies memory first: k's
	// MemoryFile and the private MemoryFiles in precopyPrivates. If stats is
	// not nil, it receives the pre-copy's.
	precopy         *PrecopyOpts
	precopyPrivates []*pgalloc.MemoryFile
	stats           *precopyStats
}

// saveImageOpts is saveImage with the options in opts.
func saveImageOpts(t *testing.T, ctx context.Context, k *Kernel, opts testSaveOpts) (*checkpointimage.Image, string, error) {
	t.Helper()
	dir := t.TempDir()
	meta, err := os.Create(filepath.Join(dir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatalf("creating the pages metadata file: %v", err)
	}
	pagesPath := filepath.Join(dir, checkpointfiles.PagesFileName)
	var pages stateio.AsyncWriter
	if !opts.fullDisk {
		pagesFD, err := unix.Open(pagesPath, unix.O_WRONLY|unix.O_CREAT|unix.O_CLOEXEC, 0644)
		if err != nil {
			t.Fatalf("creating the pages file: %v", err)
		}
		pages = stateio.NewPagesFileFDWriterDefault(int32(pagesFD))
	} else {
		pagesFile, err := os.Create(pagesPath)
		if err != nil {
			t.Fatalf("creating the pages file: %v", err)
		}
		pages = stateio.NewIOWriter(&fullDiskWriter{f: pagesFile, limit: opts.limit}, hostarch.PageSize, 1 /* maxRanges */, 1 /* maxParallel */)
	}
	if opts.writeRate != 0 {
		pages = stateio.NewRateLimitedWriter(pages, opts.writeRate)
	}

	// As state.SaveOpts.Save and Kernel.SaveTo do.
	var precopy *Precopy
	if opts.precopy != nil {
		precopy, err = k.precopy(ctx, pages, opts.parent, opts.precopyPrivates, *opts.precopy) // transfers ownership of pages
		if err != nil {
			meta.Close()
			return nil, dir, err
		}
		pages = nil
		if opts.stats != nil {
			// After Release, which completes them.
			defer func() { *opts.stats = precopy.stats }()
		}
		defer precopy.Release()
	}
	saved := []*pgalloc.MemoryFile{k.mf}
	for _, mf := range opts.privates {
		saved = append(saved, mf)
	}
	saveEpoch, lastRound, delta, err := k.beginSave(ctx, opts.parent, precopy)
	var img *checkpointimage.Image
	if err == nil {
		img, err = k.saveMemoryFiles(ctx, nil, meta, pages, opts.privates, false /* appMFExcludeCommittedZeroPages */, delta, precopy, lastRound)
	} else {
		meta.Close()
		if pages != nil {
			pages.Close()
		}
	}
	return img, dir, k.endDirtySave(ctx, saveEpoch, saved, img, err)
}

// loadImage restores img, whose layers after the first are in the image
// directories layerDirs, into a new MemoryFile as a restore does, and returns
// the MemoryFile once every page is loaded.
func loadImage(t *testing.T, ctx context.Context, img *checkpointimage.Image, imageDir string, layerDirs []string) *pgalloc.MemoryFile {
	t.Helper()
	dirs, err := checkpointimage.FindLayers(img, imageDir, layerDirs)
	if err != nil {
		t.Fatalf("FindLayers: %v", err)
	}
	var pagesFiles []stateio.AsyncReader
	for _, dir := range append([]string{imageDir}, dirs...) {
		fd, err := unix.Open(filepath.Join(dir, checkpointfiles.PagesFileName), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("opening the pages file of %s: %v", dir, err)
		}
		pagesFiles = append(pagesFiles, stateio.NewPagesFileFDReaderDefault(int32(fd)))
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
	mfl := NewAsyncMFLoader(img, pagesFiles, mf, pgalloc.PrefetchAuto, nil /* timeline */)
	mfl.KickoffPrivate(ctx, nil)
	if err := mfl.Wait(); err != nil {
		t.Fatalf("loading %v: %v", img.Digest, err)
	}
	return mf
}

// pread returns the first byte of the page at off in mf.
func pread(t *testing.T, mf *pgalloc.MemoryFile, off uint64) byte {
	t.Helper()
	b := make([]byte, 1)
	if _, err := unix.Pread(mf.FD(), b, int64(off)); err != nil {
		t.Fatalf("Pread: %v", err)
	}
	return b[0]
}

// pageImages returns, for each page of fr, the digest of the image whose
// pages file holds its data in img, which is img's own digest for the pages
// that img holds itself.
func pageImages(t *testing.T, img *checkpointimage.Image, fr memmap.FileRange) []checkpointimage.Digest {
	t.Helper()
	m := img.MemoryFileImage(0)
	layers := img.Layers()
	var ds []checkpointimage.Digest
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		e, ok := m.ExtentAt(off)
		switch {
		case !ok:
			t.Fatalf("page %#x of %v has no data", off, img.Digest)
		case e.Layer == 0:
			ds = append(ds, img.Digest)
		default:
			ds = append(ds, layers[e.Layer].Digest)
		}
	}
	return ds
}

// write writes b to page i of fr, and has src report it.
func (s *testDirtySource) write(t *testing.T, fr memmap.FileRange, i uint64, b byte) {
	t.Helper()
	off := fr.Start + i*hostarch.PageSize
	pwrite(t, s.mf, off, b)
	s.writes = append(s.writes, memmap.FileRange{off, off + hostarch.PageSize})
}

func TestIncrementalSave(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, true /* verify */)
	full, _, err := saveImage(t, ctx, k, nil, noLimit)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}
	if got, ok := k.LastImageDigest(); !ok || got != full.Digest {
		t.Fatalf("LastImageDigest after a full save: got %v, %t; want %v", got, ok, full.Digest)
	}

	// A delta writes the pages written since its parent and refers the others
	// to the parent, which is its layer 1.
	src.write(t, fr, 3, 0xaa)
	src.write(t, fr, 7, 0xbb)
	delta, _, err := saveImage(t, ctx, k, &full.Digest, noLimit)
	if err != nil {
		t.Fatalf("incremental save: %v", err)
	}
	if got, want := delta.Layers()[0].PagesSize, uint64(2*hostarch.PageSize); got != want {
		t.Errorf("delta's pages: got %d bytes, want %d", got, want)
	}
	want := slices.Repeat([]checkpointimage.Digest{full.Digest}, 16)
	want[3], want[7] = delta.Digest, delta.Digest
	if got := pageImages(t, delta, fr); !slices.Equal(got, want) {
		t.Errorf("images of the delta's pages: got %v, want %v", got, want)
	}

	// A delta of the delta refers each page to the image that holds it, which
	// may be the parent's parent.
	src.write(t, fr, 7, 0xcc)
	delta2, _, err := saveImage(t, ctx, k, &delta.Digest, noLimit)
	if err != nil {
		t.Fatalf("second incremental save: %v", err)
	}
	want[7] = delta2.Digest
	if got := pageImages(t, delta2, fr); !slices.Equal(got, want) {
		t.Errorf("images of the second delta's pages: got %v, want %v", got, want)
	}
	if got := len(delta2.Layers()); got != 3 {
		t.Errorf("second delta's layers: got %d, want 3", got)
	}

	// Rewriting the pages that remain in an image drops it from the chain.
	for _, i := range []uint64{0, 1, 2, 4, 5, 6, 8, 9, 10, 11, 12, 13, 14, 15} {
		src.write(t, fr, i, 0xdd)
	}
	delta3, _, err := saveImage(t, ctx, k, &delta2.Digest, noLimit)
	if err != nil {
		t.Fatalf("third incremental save: %v", err)
	}
	for i := range want {
		want[i] = delta3.Digest
	}
	want[3], want[7] = delta.Digest, delta2.Digest
	if got := pageImages(t, delta3, fr); !slices.Equal(got, want) {
		t.Errorf("images of the third delta's pages: got %v, want %v", got, want)
	}
	for _, l := range delta3.Layers() {
		if l.Digest == full.Digest {
			t.Errorf("third delta refers to %v, which holds none of its pages", full.Digest)
		}
	}
}

func TestIncrementalSaveRequiresLastImage(t *testing.T) {
	ctx := contexttest.Context(t)
	// dirtyTestKernel's save has no pages file, so its image cannot be a
	// parent.
	k, src, fr := dirtyTestKernel(t, ctx, false /* verify */)
	if _, ok := k.LastImageDigest(); ok {
		t.Errorf("LastImageDigest after a save without a pages file: got an image, want none")
	}
	var d checkpointimage.Digest
	if _, _, err := saveImage(t, ctx, k, &d, noLimit); err == nil || !strings.Contains(err.Error(), "no parent image") {
		t.Errorf("incremental save without a last image: got %v, want a missing parent", err)
	}

	first, _, err := saveImage(t, ctx, k, nil, noLimit)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}
	src.write(t, fr, 1, 0xaa)
	if _, _, err := saveImage(t, ctx, k, nil, noLimit); err != nil {
		t.Fatalf("second full save: %v", err)
	}
	// first is no longer the image k was last saved to.
	if _, _, err := saveImage(t, ctx, k, &first.Digest, noLimit); err == nil || !strings.Contains(err.Error(), "is not the image the sandbox was last saved to") {
		t.Errorf("incremental save of an older image: got %v, want a wrong parent", err)
	}
}

// TestIncrementalSaveFailureKeepsParent checks that a failed incremental save
// leaves its parent the parent of the next, which writes the pages written
// before the failed save.
func TestIncrementalSaveFailureKeepsParent(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, false /* verify */)
	full, _, err := saveImage(t, ctx, k, nil, noLimit)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}
	src.write(t, fr, 2, 0xaa)
	if _, _, err := saveImage(t, ctx, k, &full.Digest, 0 /* limit */); err == nil {
		t.Fatalf("incremental save to an unwritable pages file succeeded")
	}
	if got, ok := k.LastImageDigest(); !ok || got != full.Digest {
		t.Fatalf("LastImageDigest after a failed save: got %v, %t; want %v", got, ok, full.Digest)
	}
	src.write(t, fr, 9, 0xbb)
	delta, _, err := saveImage(t, ctx, k, &full.Digest, noLimit)
	if err != nil {
		t.Fatalf("incremental save after a failed one: %v", err)
	}
	want := slices.Repeat([]checkpointimage.Digest{full.Digest}, 16)
	want[2], want[9] = delta.Digest, delta.Digest
	if got := pageImages(t, delta, fr); !slices.Equal(got, want) {
		t.Errorf("images of the delta's pages: got %v, want %v", got, want)
	}
}

// TestIncrementalSaveENOSPC is Firecracker's test that a failed diff snapshot
// loses no dirty page (test_snapshot_not_losing_dirty_pages.py), on an
// incremental save whose pages file fills the disk after some of its pages:
// the save fails, and the next one, of the same parent, writes both the pages
// written before the failed save and those written since, so that the chain
// restores the sandbox's memory.
func TestIncrementalSaveENOSPC(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, true /* verify */)
	full, fullDir, err := saveImage(t, ctx, k, nil, noLimit)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}

	// want[i] is the first byte of page i of fr, which dirtyTestKernel wrote
	// as i+1.
	var want [16]byte
	for i := range want {
		want[i] = byte(i + 1)
	}
	write := func(i uint64, b byte) {
		src.write(t, fr, i, b)
		want[i] = b
	}
	for i := uint64(0); i < 8; i++ {
		write(i, 0xa0+byte(i))
	}
	// The disk is full after 3 of the 8 pages.
	if _, _, err := saveImage(t, ctx, k, &full.Digest, 3*hostarch.PageSize); !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("incremental save on a disk that fills: got %v, want ENOSPC", err)
	}

	write(5, 0xb5)
	write(12, 0xbc)
	delta, deltaDir, err := saveImage(t, ctx, k, &full.Digest, noLimit)
	if err != nil {
		t.Fatalf("incremental save after the failed one: %v", err)
	}
	if got, want := delta.Layers()[0].PagesSize, uint64(9*hostarch.PageSize); got != want {
		t.Errorf("delta's pages: got %d bytes, want %d (pages 0 to 7 and 12)", got, want)
	}
	mf := loadImage(t, ctx, delta, deltaDir, []string{fullDir})
	for i, w := range want {
		if got := pread(t, mf, fr.Start+uint64(i)*hostarch.PageSize); got != w {
			t.Errorf("restored page %d: got %#x, want %#x", i, got, w)
		}
	}
}
