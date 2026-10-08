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
	"fmt"
	"io"
	"testing"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func allocateSaveTestPages(t *testing.T, f *MemoryFile) memmap.FileRange {
	t.Helper()
	fr := allocate(t, f, 16*hostarch.PageSize, AllocOpts{Mode: AllocateUncommitted})
	t.Cleanup(func() { f.DecRef(fr) })
	return fr
}

func pwritePage(t *testing.T, f *MemoryFile, fr memmap.FileRange, page int, b byte) {
	t.Helper()
	buf := make([]byte, hostarch.PageSize)
	buf[17] = b
	if _, err := unix.Pwrite(f.FD(), buf, int64(fr.Start)+int64(page*hostarch.PageSize)); err != nil {
		t.Fatalf("Pwrite page %d: %v", page, err)
	}
}

func TestHostFileDataSeeker(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocateSaveTestPages(t, f)
	pwritePage(t, f, fr, 2, 1)
	pwritePage(t, f, fr, 3, 1)
	pwritePage(t, f, fr, 9, 1)
	pwritePage(t, f, fr, 12, 0)
	page := func(n uint64) uint64 { return fr.Start + n*hostarch.PageSize }
	d := f.newHostFileDataSeeker()
	for _, call := range []struct {
		off  uint64
		want memmap.FileRange
	}{
		{off: page(0), want: memmap.FileRange{page(2), page(4)}},
		{off: page(3), want: memmap.FileRange{page(3), page(4)}},
		{off: page(4), want: memmap.FileRange{page(9), page(10)}},
		{off: page(10), want: memmap.FileRange{page(12), page(13)}},
		{off: page(13), want: memmap.FileRange{f.TotalSize(), f.TotalSize()}},
		{off: page(15), want: memmap.FileRange{f.TotalSize(), f.TotalSize()}},
	} {
		got, err := d.dataAtOrAfter(call.off)
		if err != nil || got != call.want {
			t.Errorf("dataAtOrAfter(%#x): got (%v, %v), want (%v, nil)", call.off, got, err, call.want)
		}
	}
}

func TestSaveToPreservesContents(t *testing.T) {
	for _, tc := range []struct {
		name          string
		diskBacked    bool
		exclude       bool
		wantCommitted uint64
	}{
		{name: "memfd", wantCommitted: 2 * hostarch.PageSize},
		{name: "memfd excluding committed zero pages", exclude: true, wantCommitted: hostarch.PageSize},
		{name: "disk-backed", diskBacked: true, wantCommitted: 2 * hostarch.PageSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestMemoryFile(t, testMemoryFileOpts{diskBacked: tc.diskBacked})
			fr := allocateSaveTestPages(t, f)
			pwritePage(t, f, fr, 2, 1)
			pwritePage(t, f, fr, 13, 2)
			if err := f.SaveTo(context.Background(), io.Discard, &SaveOpts{}); err != nil {
				t.Fatalf("first SaveTo: %v", err)
			}
			pwritePage(t, f, fr, 9, 0)
			pwritePage(t, f, fr, 13, 0)
			want := make([]byte, fr.Length())
			want[2*hostarch.PageSize+17] = 1

			var checkpoint bytes.Buffer
			if err := f.SaveTo(context.Background(), &checkpoint, &SaveOpts{ExcludeCommittedZeroPages: tc.exclude}); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			if got := f.knownCommittedBytes; got != tc.wantCommitted {
				t.Errorf("knownCommittedBytes: got %d, want %d", got, tc.wantCommitted)
			}

			restored := newTestMemoryFile(t, testMemoryFileOpts{diskBacked: tc.diskBacked})
			if err := restored.LoadFrom(context.Background(), &checkpoint, &LoadOpts{}); err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			t.Cleanup(func() { restored.DecRef(fr) })
			restored.forEachMappingSlice(fr, func(bs []byte) {
				if !bytes.Equal(bs, want) {
					t.Error("restored contents differ from saved contents")
				}
			})
		})
	}
}

// testMemory is the memory of a MemoryFile built by buildTestMemory, with the
// page contents it is expected to have.
type testMemory struct {
	// live are the ranges that are allocated when buildTestMemory returns.
	live []memmap.FileRange
	gens pageGens
	// nonZeroBytes is the number of bytes of non-zero pages in live.
	nonZeroBytes uint64
}

// buildTestMemory allocates and writes, in f, memory of every kind a saved
// MemoryFile may hold: pages written with data, pages written with zeros
// (committed but zero), pages never touched (uncommitted), data in huge
// pages, pages freed before saving (waste), memory of several accounting
// kinds and cgroups, and memory committed with fallocate. It returns what f
// is expected to contain.
func buildTestMemory(t *testing.T, f *MemoryFile, gen uint64) *testMemory {
	t.Helper()
	m := &testMemory{gens: make(pageGens)}
	page := func(fr memmap.FileRange, i, n uint64) memmap.FileRange {
		return memmap.FileRange{fr.Start + i*hostarch.PageSize, fr.Start + (i+n)*hostarch.PageSize}
	}
	write := func(fr memmap.FileRange, gen uint64) {
		m.gens.write(t, f, fr, gen)
		if gen != 0 {
			m.nonZeroBytes += fr.Length()
		}
	}

	// Anonymous memory: data, explicit zeros and untouched pages.
	anon := allocate(t, f, 64*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	write(page(anon, 0, 8), gen)
	write(page(anon, 8, 4), 0)
	write(page(anon, 20, 1), gen)
	write(page(anon, 40, 24), gen)
	m.live = append(m.live, anon)

	// Huge-page-backed memory, sparsely written.
	huge := allocate(t, f, 2*hostarch.HugePageSize, AllocOpts{Kind: usage.Anonymous, Huge: true})
	write(page(huge, 0, 1), gen)
	write(page(huge, 511, 2), gen)
	write(page(huge, 1023, 1), gen)
	m.live = append(m.live, huge)

	// Page cache memory accounted to a memory cgroup, committed by
	// fallocate(2) and only partly written.
	cache := allocate(t, f, 16*hostarch.PageSize, AllocOpts{Kind: usage.PageCache, MemCgID: 7, Mode: AllocateAndCommit})
	write(page(cache, 4, 4), gen)
	m.live = append(m.live, cache)

	// Tmpfs memory, allocated write-populated as copy-on-write copies and
	// file data are.
	tmpfs := allocate(t, f, 8*hostarch.PageSize, AllocOpts{Kind: usage.Tmpfs, Mode: AllocateAndWritePopulate})
	write(tmpfs, gen)
	m.live = append(m.live, tmpfs)

	// Memory written and then freed before saving.
	freed := allocate(t, f, 16*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	writePattern(t, f, freed, gen)
	f.DecRef(freed)

	t.Cleanup(func() {
		for _, fr := range m.live {
			f.DecRef(fr)
		}
	})
	return m
}

// check checks that f, loaded from an image of m's MemoryFile, has m's
// contents.
func (m *testMemory) check(t *testing.T, f *MemoryFile) {
	t.Helper()
	for _, fr := range m.live {
		m.gens.checkPages(t, f, fr)
	}
}

func TestSaveLoad(t *testing.T) {
	for _, tc := range []struct {
		name       string
		diskBacked bool
		pagesFile  bool
		exclude    bool
	}{
		{name: "pages file", pagesFile: true},
		{name: "pages file excluding committed zero pages", pagesFile: true, exclude: true},
		{name: "pages file, disk-backed", pagesFile: true, diskBacked: true},
		{name: "inline"},
		{name: "inline, disk-backed", diskBacked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mfOpts := testMemoryFileOpts{diskBacked: tc.diskBacked, expectHugepages: true}
			f := newTestMemoryFile(t, mfOpts)
			m := buildTestMemory(t, f, 1)

			saveOpts := SaveOpts{ExcludeCommittedZeroPages: tc.exclude}
			var img *testImage
			if tc.pagesFile {
				img = saveImage(t, saveOpts, f)
			} else {
				var buf bytes.Buffer
				if err := f.SaveTo(context.Background(), &buf, &saveOpts); err != nil {
					t.Fatalf("SaveTo: %v", err)
				}
				img = &testImage{meta: buf.Bytes()}
			}
			checkInvariants(t, f)
			// Only pages with data are saved; the scan decommits the rest
			// and freed memory was released.
			if got := f.knownCommittedBytes; got != m.nonZeroBytes {
				t.Errorf("saved MemoryFile: knownCommittedBytes = %d, want %d", got, m.nonZeroBytes)
			}
			if tc.pagesFile && uint64(len(img.pages)) != m.nonZeroBytes {
				t.Errorf("pages file has %d bytes, want %d", len(img.pages), m.nonZeroBytes)
			}
			saved := f.exportMetadataProto()

			restored := newTestMemoryFile(t, mfOpts)
			if tc.pagesFile {
				loadImage(t, img, restored)
			} else if err := restored.LoadFrom(context.Background(), bytes.NewReader(img.meta), &LoadOpts{}); err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			t.Cleanup(func() { releaseAll(t, restored) })
			checkInvariants(t, restored)
			if got := restored.exportMetadataProto(); !proto.Equal(got, saved) {
				t.Errorf("restored metadata differs from saved metadata:\ngot:  %v\nwant: %v", got, saved)
			}
			if got := restored.knownCommittedBytes; got != m.nonZeroBytes {
				t.Errorf("restored MemoryFile: knownCommittedBytes = %d, want %d", got, m.nonZeroBytes)
			}
			m.check(t, restored)
			// The saved MemoryFile is unchanged by saving.
			m.check(t, f)
		})
	}
}

// TestSaveLoadPrivateMemoryFiles saves several MemoryFiles to one pages file,
// as the kernel saves the application MemoryFile and private MemoryFiles, and
// loads them back in order.
func TestSaveLoadPrivateMemoryFiles(t *testing.T) {
	const n = 3
	var (
		fs       [n]*MemoryFile
		restored [n]*MemoryFile
		ms       [n]*testMemory
	)
	for i := range fs {
		fs[i] = newTestMemoryFile(t, testMemoryFileOpts{expectHugepages: true})
		ms[i] = buildTestMemory(t, fs[i], uint64(i+1))
		restored[i] = newTestMemoryFile(t, testMemoryFileOpts{expectHugepages: true})
	}
	img := saveImage(t, SaveOpts{}, fs[:]...)
	loadImage(t, img, restored[:]...)
	for i := range restored {
		t.Run(fmt.Sprintf("MemoryFile %d", i), func(t *testing.T) {
			t.Cleanup(func() { releaseAll(t, restored[i]) })
			checkInvariants(t, restored[i])
			ms[i].check(t, restored[i])
		})
	}
}

// TestSaveDecommittedPagesOfLiveAllocation checks that pages decommitted
// while their allocation stays live, which the application then reads as
// zero, are saved as zero rather than with their contents at a previous save.
func TestSaveDecommittedPagesOfLiveAllocation(t *testing.T) {
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocate(t, f, 8*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(fr) })
	gens := make(pageGens)
	gens.write(t, f, fr, 1)
	saveImage(t, SaveOpts{}, f)
	if got, want := f.knownCommittedBytes, fr.Length(); got != want {
		t.Fatalf("after the first save: knownCommittedBytes = %d, want %d", got, want)
	}

	decommitted := memmap.FileRange{fr.Start + 2*hostarch.PageSize, fr.Start + 4*hostarch.PageSize}
	f.Decommit(decommitted)
	gens.set(decommitted, 0)
	rewritten := memmap.FileRange{fr.Start + 5*hostarch.PageSize, fr.Start + 6*hostarch.PageSize}
	gens.write(t, f, rewritten, 2)
	img := saveImage(t, SaveOpts{}, f)
	checkInvariants(t, f)
	if got, want := uint64(len(img.pages)), fr.Length()-decommitted.Length(); got != want {
		t.Errorf("second image: pages file has %d bytes, want %d", got, want)
	}

	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	loadImage(t, img, restored)
	t.Cleanup(func() { releaseAll(t, restored) })
	checkInvariants(t, restored)
	gens.checkPages(t, restored, fr)
}
