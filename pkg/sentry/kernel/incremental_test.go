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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

// saveImage saves k's MemoryFile as an image with a pages file in a new
// directory, incrementally if parent is not nil, as Kernel.saveToLocked does;
// if failPages is true, writing the pages file fails. It returns the image and
// the error of the save.
func saveImage(t *testing.T, ctx context.Context, k *Kernel, parent *checkpointimage.Digest, failPages bool) (*checkpointimage.Image, error) {
	t.Helper()
	img, _, err := saveImageOpts(t, ctx, k, testSaveOpts{parent: parent, failPages: failPages})
	return img, err
}

// testSaveOpts configures saveImageOpts.
type testSaveOpts struct {
	// parent is the parent of an incremental save.
	parent *checkpointimage.Digest

	// If failPages is true, writing the pages file fails.
	failPages bool

	// If precopy is not nil, the save pre-copies memory first.
	precopy *PrecopyOpts
}

// saveImageOpts is saveImage with the options in opts. It also returns the
// image's directory.
func saveImageOpts(t *testing.T, ctx context.Context, k *Kernel, opts testSaveOpts) (*checkpointimage.Image, string, error) {
	t.Helper()
	dir := t.TempDir()
	meta, err := os.Create(filepath.Join(dir, "pages_meta.img"))
	if err != nil {
		t.Fatalf("creating the pages metadata file: %v", err)
	}
	pagesPath := filepath.Join(dir, "pages.img")
	if err := os.WriteFile(pagesPath, nil, 0644); err != nil {
		t.Fatalf("creating the pages file: %v", err)
	}
	flags := unix.O_WRONLY
	if opts.failPages {
		flags = unix.O_RDONLY
	}
	pagesFD, err := unix.Open(pagesPath, flags, 0)
	if err != nil {
		t.Fatalf("opening the pages file: %v", err)
	}
	var pages stateio.AsyncWriter = stateio.NewPagesFileFDWriterDefault(int32(pagesFD))

	// As Kernel.SaveTo does with a pre-copy.
	var precopy *Precopy
	if opts.precopy != nil {
		precopy, err = k.Precopy(ctx, pages, opts.parent, *opts.precopy) // transfers ownership of pages
		if err != nil {
			meta.Close()
			return nil, dir, err
		}
		pages = nil
		defer precopy.Release()
	}

	e, err := k.beginDirtySave(ctx)
	if err != nil {
		t.Fatalf("beginDirtySave: %v", err)
	}
	var (
		delta     *incrementalSave
		img       *checkpointimage.Image
		saveEpoch = e
		lastRound *pgalloc.DirtySet
	)
	if precopy != nil {
		lastRound = e.Sets[k.mf]
		saveEpoch = precopy.epochSince(e)
	}
	if opts.parent != nil {
		delta, err = k.beginIncrementalSave(*opts.parent, e)
	}
	if err == nil {
		img, err = k.saveMemoryFiles(ctx, nil, meta, pages, map[checkpoint.ResourceID]*pgalloc.MemoryFile{}, false /* appMFExcludeCommittedZeroPages */, delta, precopy, lastRound)
	} else {
		meta.Close()
		if pages != nil {
			pages.Close()
		}
	}
	return img, dir, k.endDirtySave(ctx, saveEpoch, []*pgalloc.MemoryFile{k.mf}, img, err)
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
	full, err := saveImage(t, ctx, k, nil, false)
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
	delta, err := saveImage(t, ctx, k, &full.Digest, false)
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
	delta2, err := saveImage(t, ctx, k, &delta.Digest, false)
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
	delta3, err := saveImage(t, ctx, k, &delta2.Digest, false)
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
	if _, err := saveImage(t, ctx, k, &d, false); err == nil || !strings.Contains(err.Error(), "no parent image") {
		t.Errorf("incremental save without a last image: got %v, want a missing parent", err)
	}

	first, err := saveImage(t, ctx, k, nil, false)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}
	src.write(t, fr, 1, 0xaa)
	if _, err := saveImage(t, ctx, k, nil, false); err != nil {
		t.Fatalf("second full save: %v", err)
	}
	// first is no longer the image k was last saved to.
	if _, err := saveImage(t, ctx, k, &first.Digest, false); err == nil || !strings.Contains(err.Error(), "is not the image the sandbox was last saved to") {
		t.Errorf("incremental save of an older image: got %v, want a wrong parent", err)
	}
}

// TestIncrementalSaveFailureKeepsParent checks that a failed incremental save
// leaves its parent the parent of the next, which writes the pages written
// before the failed save.
func TestIncrementalSaveFailureKeepsParent(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, false /* verify */)
	full, err := saveImage(t, ctx, k, nil, false)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}
	src.write(t, fr, 2, 0xaa)
	if _, err := saveImage(t, ctx, k, &full.Digest, true /* failPages */); err == nil {
		t.Fatalf("incremental save to an unwritable pages file succeeded")
	}
	if got, ok := k.LastImageDigest(); !ok || got != full.Digest {
		t.Fatalf("LastImageDigest after a failed save: got %v, %t; want %v", got, ok, full.Digest)
	}
	src.write(t, fr, 9, 0xbb)
	delta, err := saveImage(t, ctx, k, &full.Digest, false)
	if err != nil {
		t.Fatalf("incremental save after a failed one: %v", err)
	}
	want := slices.Repeat([]checkpointimage.Digest{full.Digest}, 16)
	want[2], want[9] = delta.Digest, delta.Digest
	if got := pageImages(t, delta, fr); !slices.Equal(got, want) {
		t.Errorf("images of the delta's pages: got %v, want %v", got, want)
	}
}
