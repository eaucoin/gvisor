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

package hostmm

import (
	"os"
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const pageSize = 4096

func addrOf(m []byte) uintptr {
	return uintptr(unsafe.Pointer(&m[0]))
}

// writeTrackedMemfd returns a write-tracked shared mapping of n pages of a
// new memfd, as the Sentry maps MemoryFiles, and the calling process's
// pagemap.
func writeTrackedMemfd(t *testing.T, n int) ([]byte, int, int) {
	t.Helper()
	uffd, err := NewWPAsyncUserfaultfd()
	if err != nil {
		t.Skipf("write tracking is not available: %v", err)
	}
	t.Cleanup(func() { unix.Close(uffd) })
	memfd, err := unix.MemfdCreate("uffdwp-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatalf("memfd_create: %v", err)
	}
	t.Cleanup(func() { unix.Close(memfd) })
	if err := unix.Ftruncate(memfd, int64(n*pageSize)); err != nil {
		t.Fatalf("ftruncate: %v", err)
	}
	m, err := unix.Mmap(memfd, 0, n*pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	t.Cleanup(func() { unix.Munmap(m) })
	if err := WriteProtectRange(uffd, addrOf(m), uintptr(len(m))); err != nil {
		t.Fatalf("WriteProtectRange: %v", err)
	}
	pagemap, err := unix.Open("/proc/self/pagemap", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("opening pagemap: %v", err)
	}
	t.Cleanup(func() { unix.Close(pagemap) })
	return m, memfd, pagemap
}

// written returns the pages of m that HarvestWritten reports, in order.
func written(t *testing.T, pagemap int, m []byte) []int {
	t.Helper()
	var pages []int
	start := addrOf(m)
	if err := new(PagemapScanBuf).HarvestWritten(pagemap, start, start+uintptr(len(m)), func(s, e uintptr) {
		for a := s; a < e; a += pageSize {
			pages = append(pages, int((a-start)/pageSize))
		}
	}); err != nil {
		t.Fatalf("HarvestWritten: %v", err)
	}
	return pages
}

func checkWritten(t *testing.T, pagemap int, m []byte, want []int) {
	t.Helper()
	if got := written(t, pagemap, m); !slices.Equal(got, want) {
		t.Errorf("written pages: got %v, want %v", got, want)
	}
}

func TestHarvestWritten(t *testing.T) {
	m, memfd, pagemap := writeTrackedMemfd(t, 16)

	// Nothing is written yet, including pages never faulted in.
	checkWritten(t, pagemap, m, nil)

	// User-mode writes.
	m[1*pageSize] = 1
	m[3*pageSize+100] = 1
	m[4*pageSize+pageSize-1] = 1
	checkWritten(t, pagemap, m, []int{1, 3, 4})
	// The harvest write-protected them again.
	checkWritten(t, pagemap, m, nil)
	m[3*pageSize] = 2
	checkWritten(t, pagemap, m, []int{3})

	// A write by the kernel on the process's behalf: read(2) into page 6.
	f, err := os.CreateTemp(t.TempDir(), "data")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("kernel-mode write")); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Pread(int(f.Fd()), m[6*pageSize:7*pageSize], 0); err != nil {
		t.Fatalf("pread: %v", err)
	}
	checkWritten(t, pagemap, m, []int{6})

	// Writes through the file or another mapping of it are not writes
	// through m.
	if _, err := unix.Pwrite(memfd, []byte{3}, 8*pageSize); err != nil {
		t.Fatalf("pwrite: %v", err)
	}
	checkWritten(t, pagemap, m, nil)

	// Zapping the page tables loses neither written state nor
	// write-protection.
	m[10*pageSize] = 4
	if err := unix.Madvise(m, unix.MADV_DONTNEED); err != nil {
		t.Fatalf("madvise: %v", err)
	}
	checkWritten(t, pagemap, m, []int{10})
	sink = m[11*pageSize] // fault in a read-only, write-protected page
	m[12*pageSize] = 5
	checkWritten(t, pagemap, m, []int{12})

	// Punching a hole in the file, as MemoryFiles decommit pages, loses
	// neither.
	m[13*pageSize] = 6
	if err := unix.Fallocate(memfd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 13*pageSize, 2*pageSize); err != nil {
		t.Fatalf("fallocate: %v", err)
	}
	checkWritten(t, pagemap, m, []int{13})
	m[14*pageSize] = 7
	checkWritten(t, pagemap, m, []int{14})
}

// sink keeps reads from being optimized away.
var sink byte

func TestHarvestWrittenUnregistered(t *testing.T) {
	_, _, pagemap := writeTrackedMemfd(t, 1)
	other, err := unix.Mmap(-1, 0, pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer unix.Munmap(other)
	start := addrOf(other)
	if err := new(PagemapScanBuf).HarvestWritten(pagemap, start, start+pageSize, func(uintptr, uintptr) {}); err == nil {
		t.Errorf("HarvestWritten of a range not registered for write tracking succeeded")
	}
}
