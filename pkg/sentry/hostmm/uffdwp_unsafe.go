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
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

// UserfaultfdAPI performs the UFFDIO_API handshake on the userfaultfd fd for
// write tracking. It returns an error if the kernel lacks the features of
// write tracking.
func UserfaultfdAPI(fd int) error {
	api := linux.UffdioAPI{
		API:      linux.UFFD_API,
		Features: WPAsyncFeatures,
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(linux.UFFDIO_API), uintptr(unsafe.Pointer(&api))); errno != 0 {
		return fmt.Errorf("UFFDIO_API(features %#x): %w", uint64(WPAsyncFeatures), errno)
	}
	if api.Features&WPAsyncFeatures != WPAsyncFeatures {
		return fmt.Errorf("UFFDIO_API: features %#x lack %#x", api.Features, uint64(WPAsyncFeatures))
	}
	return nil
}

// WriteProtectRange registers [start, start+length) of the mm of the
// userfaultfd fd for write-protection, and write-protects it, so that the
// next write to each of its pages is recorded. Pages not yet faulted in are
// write-protected too (UFFD_FEATURE_WP_UNPOPULATED).
func WriteProtectRange(fd int, start, length uintptr) error {
	reg := linux.UffdioRegister{
		Range: linux.UffdioRange{Start: uint64(start), Len: uint64(length)},
		Mode:  linux.UFFDIO_REGISTER_MODE_WP,
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(linux.UFFDIO_REGISTER), uintptr(unsafe.Pointer(&reg))); errno != 0 {
		return fmt.Errorf("UFFDIO_REGISTER(%#x-%#x): %w", start, start+length, errno)
	}
	wp := linux.UffdioWriteprotect{
		Range: linux.UffdioRange{Start: uint64(start), Len: uint64(length)},
		Mode:  linux.UFFDIO_WRITEPROTECT_MODE_WP,
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(linux.UFFDIO_WRITEPROTECT), uintptr(unsafe.Pointer(&wp))); errno != 0 {
		return fmt.Errorf("UFFDIO_WRITEPROTECT(%#x-%#x): %w", start, start+length, errno)
	}
	return nil
}

// PagemapScanBuf is a buffer for the results of PAGEMAP_SCAN. The kernel
// writes to it through its address, so it must be heap-allocated (new) and
// is reused across scans.
type PagemapScanBuf struct {
	vec [pagemapScanVecLen]linux.PageRegion
}

// HarvestWritten calls fn on each range of [start, end) written since it was
// write-protected, in the mm of the pagemap file pagemapFD, and
// write-protects those ranges again. fn may be called with adjacent ranges.
//
// It uses only the masks that PAGEMAP_SCAN's fast path handles (category and
// return masks PAGE_IS_WRITTEN), the only ones correct on kernels without
// commits 07b4377bdbe7 ("fix PAGEMAP_SCAN written state for unpopulated
// ptes") and 40de8160ca7f ("fix PAGEMAP_SCAN written state for PMD holes"),
// such as Ubuntu's 6.8: there, other masks report a zapped written page as
// clean. With PM_SCAN_CHECK_WPASYNC, the scan fails rather than skipping a
// range that is not registered for write tracking.
func (b *PagemapScanBuf) HarvestWritten(pagemapFD int, start, end uintptr, fn func(start, end uintptr)) error {
	for start < end {
		arg := linux.PMScanArg{
			Size:         linux.SizeOfPMScanArg,
			Flags:        linux.PM_SCAN_WP_MATCHING | linux.PM_SCAN_CHECK_WPASYNC,
			Start:        uint64(start),
			End:          uint64(end),
			Vec:          uint64(uintptr(unsafe.Pointer(&b.vec[0]))),
			VecLen:       pagemapScanVecLen,
			CategoryMask: linux.PAGE_IS_WRITTEN,
			ReturnMask:   linux.PAGE_IS_WRITTEN,
		}
		n, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(pagemapFD), uintptr(linux.PAGEMAP_SCAN), uintptr(unsafe.Pointer(&arg)))
		runtime.KeepAlive(b)
		if errno != 0 {
			return fmt.Errorf("PAGEMAP_SCAN(%#x-%#x): %w", start, end, errno)
		}
		for _, r := range b.vec[:n] {
			fn(uintptr(r.Start), uintptr(r.End))
		}
		// The scan stops early when vec is full; walk_end is where it
		// stopped.
		if uintptr(arg.WalkEnd) <= start {
			return fmt.Errorf("PAGEMAP_SCAN(%#x-%#x) made no progress", start, end)
		}
		start = uintptr(arg.WalkEnd)
	}
	return nil
}
