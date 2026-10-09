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

package kvm

import (
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/ring0"
	"gvisor.dev/gvisor/pkg/ring0/pagetables"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/kvm/testutil"
)

// TestWriteTracking checks that the application's stores to memory that the
// Sentry maps, write-tracked with userfaultfd write-protection in
// asynchronous mode (platform.WriteTrackingInternalMappings), are recorded in
// the Sentry's page tables: KVM resolves the guest's write through a host
// write fault, which write tracking records.
func TestWriteTracking(t *testing.T) {
	uffd, err := hostmm.NewWPAsyncUserfaultfd()
	if err != nil {
		t.Skipf("write tracking is not available: %v", err)
	}
	defer unix.Close(uffd)
	pagemap, err := unix.Open("/proc/self/pagemap", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("opening pagemap: %v", err)
	}
	defer unix.Close(pagemap)
	memfd, err := unix.MemfdCreate("kvm-write-tracking-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatalf("memfd_create: %v", err)
	}
	defer unix.Close(memfd)
	const pages = 8
	if err := unix.Ftruncate(memfd, pages*hostarch.PageSize); err != nil {
		t.Fatalf("ftruncate: %v", err)
	}
	m, err := unix.Mmap(memfd, 0, pages*hostarch.PageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer unix.Munmap(m)
	start := uintptr(unsafe.Pointer(&m[0]))
	end := start + uintptr(len(m))
	// Fault the pages in writable before arming, as after earlier writes.
	for i := 0; i < pages; i++ {
		m[i*hostarch.PageSize] = 1
	}
	if err := hostmm.WriteProtectRange(uffd, start, end-start); err != nil {
		t.Fatalf("WriteProtectRange: %v", err)
	}
	written := func() []int {
		var got []int
		if err := new(hostmm.PagemapScanBuf).HarvestWritten(pagemap, start, end, func(s, e uintptr) {
			for a := s; a < e; a += hostarch.PageSize {
				got = append(got, int((a-start)/hostarch.PageSize))
			}
		}); err != nil {
			t.Fatalf("HarvestWritten: %v", err)
		}
		return got
	}
	if got := written(); len(got) != 0 {
		t.Fatalf("pages %v written before any store", got)
	}

	for _, page := range []int{2, 5} {
		applicationTest(t, true, testutil.AddrOfStore(), func(c *vCPU, regs *arch.Registers, pt *pagetables.PageTables) bool {
			testutil.SetStoreTarget(regs, start+uintptr(page)*hostarch.PageSize)
			var si linux.SignalInfo
			if _, err := c.SwitchToUser(ring0.SwitchOpts{
				Registers:          regs,
				FloatingPointState: &dummyFPState,
				PageTables:         pt,
				FullRestore:        true,
			}, &si); err == platform.ErrContextInterrupt {
				return true // Retry.
			} else if err != nil {
				t.Errorf("application store got %v, expected a syscall", err)
			}
			return false
		})
		if got, want := written(), []int{page}; !slices.Equal(got, want) {
			t.Errorf("after the application stored to page %d: written pages %v, want %v", page, got, want)
		}
	}
}
