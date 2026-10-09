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
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/hostmm/hostmmtest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// enableInternalWriteTracking enables write tracking of internal mappings for
// the duration of the test, or ends the test if the host lacks it
// (hostmmtest.Unavailable).
func enableInternalWriteTracking(t *testing.T) {
	t.Helper()
	uffd, err := hostmm.NewWPAsyncUserfaultfd()
	if err != nil {
		hostmmtest.Unavailable(t, err)
	}
	pagemap, err := unix.Open("/proc/self/pagemap", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(uffd)
		t.Fatalf("opening pagemap: %v", err)
	}
	EnableInternalWriteTracking(uffd, pagemap)
	t.Cleanup(func() {
		EnableInternalWriteTracking(-1, -1)
		unix.Close(uffd)
		unix.Close(pagemap)
	})
}

// writeThroughMappings writes to the pages of fr through f's internal
// mappings without MapInternal, as the application writes on kvm.
func writeThroughMappings(f *MemoryFile, fr memmap.FileRange) {
	f.forEachMappingSlice(fr, func(bs []byte) {
		for i := 0; i < len(bs); i += hostarch.PageSize {
			bs[i]++
		}
	})
}

func TestInternalWriteTracking(t *testing.T) {
	enableInternalWriteTracking(t)
	f := newTestMemoryFile(t, testMemoryFileOpts{})
	fr := allocate(t, f, 64*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(fr) })
	f.EnableDirtyTracking()
	if err := f.ArmInternalWrites(); err != nil {
		t.Fatalf("ArmInternalWrites: %v", err)
	}
	harvestAndSwap := func() *DirtySet {
		t.Helper()
		if err := f.HarvestInternalWrites(); err != nil {
			t.Fatalf("HarvestInternalWrites: %v", err)
		}
		return f.SwapDirty(true /* paused */)
	}
	checkDirty(t, "after arming", harvestAndSwap(), fr)

	writeThroughMappings(f, pageRange(fr, 3, 2))
	writeThroughMappings(f, pageRange(fr, 40, 1))
	checkDirty(t, "after writes", harvestAndSwap(), fr, pageRange(fr, 3, 2), pageRange(fr, 40, 1))
	// The harvest write-protected the written pages again.
	checkDirty(t, "after a harvest", harvestAndSwap(), fr)
	writeThroughMappings(f, pageRange(fr, 4, 1))
	checkDirty(t, "after a second write", harvestAndSwap(), fr, pageRange(fr, 4, 1))

	// A chunk added after arming is write-protected as it is mapped.
	big := allocate(t, f, chunkSize, AllocOpts{Kind: usage.Anonymous})
	t.Cleanup(func() { f.DecRef(big) })
	if got := len(f.chunksLoad()); got < 2 {
		t.Fatalf("the MemoryFile has %d chunks after a chunk-sized allocation", got)
	}
	last := pageRange(big, big.Length()/hostarch.PageSize-1, 1)
	writeThroughMappings(f, last)
	checkDirty(t, "after a write to a new chunk", harvestAndSwap(), big, last)

	// Negative control: without the harvest's marks, writes are lost.
	TestOnlyDisableDirtyMarkPath(DirtyMarkUffdInternal)
	t.Cleanup(func() { TestOnlyDisableDirtyMarkPath(DirtyMarkNone) })
	writeThroughMappings(f, pageRange(fr, 5, 1))
	checkDirty(t, "after a write with DirtyMarkUffdInternal disabled", harvestAndSwap(), fr)
}

// TestInternalWriteTrackingIgnoresAsyncLoading checks that pages loaded by
// async page loading after write tracking is armed, which restore the
// contents of the image being loaded, are not reported as written.
func TestInternalWriteTrackingIgnoresAsyncLoading(t *testing.T) {
	enableInternalWriteTracking(t)
	fr, gens, img := loaderTestImage(t, 32*hostarch.PageSize)
	r := newTestPagesFile(t, img.pages, testPagesFileOpts{
		maxReadBytes: 4 * hostarch.PageSize,
		maxParallel:  1,
		latency:      time.Millisecond,
		manual:       true,
	})
	restored := newTestMemoryFile(t, testMemoryFileOpts{})
	l := startLoad(t, img, r, restored)
	t.Cleanup(func() { releaseAll(t, restored) })
	// Tracking starts while pages are still loading, as after a restore with
	// --background.
	r.waitInflight(1)
	restored.EnableDirtyTracking()
	if err := restored.ArmInternalWrites(); err != nil {
		t.Fatalf("ArmInternalWrites: %v", err)
	}
	r.finish()
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	gens.checkPages(t, restored, fr)
	if err := restored.HarvestInternalWrites(); err != nil {
		t.Fatalf("HarvestInternalWrites: %v", err)
	}
	checkDirty(t, "after loading", restored.SwapDirty(true /* paused */), fr)
}

// BenchmarkHarvestInternalWrites measures a harvest of a MemoryFile with one
// chunk (1 GiB), 64 MiB of which is allocated and committed, after writes to
// the first 0, 1 or 64 MiB.
func BenchmarkHarvestInternalWrites(b *testing.B) {
	for _, written := range []uint64{0, 1 << 20, 64 << 20} {
		b.Run(fmt.Sprintf("written=%dMiB", written>>20), func(b *testing.B) {
			uffd, err := hostmm.NewWPAsyncUserfaultfd()
			if err != nil {
				hostmmtest.Unavailable(b, err)
			}
			pagemap, err := unix.Open("/proc/self/pagemap", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				b.Fatal(err)
			}
			EnableInternalWriteTracking(uffd, pagemap)
			defer func() {
				EnableInternalWriteTracking(-1, -1)
				unix.Close(uffd)
				unix.Close(pagemap)
			}()
			f := newTestMemoryFile(b, testMemoryFileOpts{})
			fr := allocate(b, f, 64<<20, AllocOpts{Kind: usage.Anonymous, Mode: AllocateAndWritePopulate})
			defer f.DecRef(fr)
			f.EnableDirtyTracking()
			if err := f.ArmInternalWrites(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				writeThroughMappings(f, memmap.FileRange{fr.Start, fr.Start + written})
				b.StartTimer()
				if err := f.HarvestInternalWrites(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
