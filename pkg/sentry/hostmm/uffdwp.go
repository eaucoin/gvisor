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

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/seccomp"
)

// Write tracking with userfaultfd write-protection in asynchronous mode
// (Linux 6.7 and later).
//
// A userfaultfd tracks the address space (mm) of the process that created
// it. Registering a range with it for write-protection, with
// UFFD_FEATURE_WP_ASYNC, makes the kernel resolve write faults on
// write-protected pages itself, by clearing their write-protect bit, without
// any message: the bit then records "written since armed". PAGEMAP_SCAN, an
// ioctl on the mm's /proc/PID/pagemap, reports the written pages of a range
// and write-protects them again in the same walk (PM_SCAN_WP_MATCHING), so
// that no write between the report and the re-arming is lost.
// Write-protection survives zaps of the page tables as PTE markers, but not
// munmap: a range must be harvested before it is unmapped.
//
// Writes made by the kernel on behalf of the process (e.g. read(2) into the
// range) are tracked too, also with UFFD_USER_MODE_ONLY, which only affects
// the faults that are delivered to userspace (none in asynchronous mode).

// WPAsyncFeatures are the userfaultfd features of write tracking.
const WPAsyncFeatures = linux.UFFD_FEATURE_WP_ASYNC | linux.UFFD_FEATURE_WP_UNPOPULATED

// UserfaultfdFlags are the flags of userfaultfds used for write tracking.
// UFFD_USER_MODE_ONLY lets processes without CAP_SYS_PTRACE create them when
// vm.unprivileged_userfaultfd is 0.
const UserfaultfdFlags = unix.O_CLOEXEC | unix.O_NONBLOCK | linux.UFFD_USER_MODE_ONLY

// NewWPAsyncUserfaultfd returns a userfaultfd for the calling process's mm,
// set up for write tracking.
func NewWPAsyncUserfaultfd() (int, error) {
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, UserfaultfdFlags, 0, 0)
	if errno != 0 {
		return -1, fmt.Errorf("userfaultfd: %w", errno)
	}
	if err := UserfaultfdAPI(int(fd)); err != nil {
		unix.Close(int(fd))
		return -1, err
	}
	return int(fd), nil
}

// pagemapScanVecLen is the number of ranges that each PAGEMAP_SCAN returns
// at most.
const pagemapScanVecLen = 512

// WriteTrackingSyscallRules returns the seccomp rules that allow write
// tracking with userfaultfds and pagemap files that are already open: the
// ioctls above, with these commands only.
func WriteTrackingSyscallRules() seccomp.SyscallRules {
	return seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
		unix.SYS_IOCTL: seccomp.Or{
			seccomp.PerArg{seccomp.NonNegativeFD{}, seccomp.EqualTo(linux.UFFDIO_API)},
			seccomp.PerArg{seccomp.NonNegativeFD{}, seccomp.EqualTo(linux.UFFDIO_REGISTER)},
			seccomp.PerArg{seccomp.NonNegativeFD{}, seccomp.EqualTo(linux.UFFDIO_WRITEPROTECT)},
			seccomp.PerArg{seccomp.NonNegativeFD{}, seccomp.EqualTo(linux.PAGEMAP_SCAN)},
		},
	})
}
