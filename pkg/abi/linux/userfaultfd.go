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

package linux

import "structs"

// userfaultfd(2) flags, from include/uapi/linux/userfaultfd.h.
const (
	// UFFD_USER_MODE_ONLY makes a userfaultfd handle only faults taken in
	// user mode.
	UFFD_USER_MODE_ONLY = 1
)

// UFFD_API is the userfaultfd API version, from
// include/uapi/linux/userfaultfd.h.
const UFFD_API = 0xAA

// userfaultfd features, from include/uapi/linux/userfaultfd.h.
const (
	UFFD_FEATURE_WP_UNPOPULATED = 1 << 13
	UFFD_FEATURE_WP_ASYNC       = 1 << 15
)

// UFFDIO is the ioctl type of userfaultfd ioctls.
const UFFDIO = 0xAA

// userfaultfd ioctls, from include/uapi/linux/userfaultfd.h.
var (
	UFFDIO_API          = IOWR(UFFDIO, 0x3F, SizeOfUffdioAPI)
	UFFDIO_REGISTER     = IOWR(UFFDIO, 0x00, SizeOfUffdioRegister)
	UFFDIO_WRITEPROTECT = IOWR(UFFDIO, 0x06, SizeOfUffdioWriteprotect)
)

// Modes of UFFDIO_REGISTER and UFFDIO_WRITEPROTECT.
const (
	UFFDIO_REGISTER_MODE_WP     = 1 << 1
	UFFDIO_WRITEPROTECT_MODE_WP = 1 << 0
)

// UffdioAPI is struct uffdio_api.
type UffdioAPI struct {
	_        structs.HostLayout
	API      uint64
	Features uint64
	Ioctls   uint64
}

// UffdioRange is struct uffdio_range.
type UffdioRange struct {
	_     structs.HostLayout
	Start uint64
	Len   uint64
}

// UffdioRegister is struct uffdio_register.
type UffdioRegister struct {
	_      structs.HostLayout
	Range  UffdioRange
	Mode   uint64
	Ioctls uint64
}

// UffdioWriteprotect is struct uffdio_writeprotect.
type UffdioWriteprotect struct {
	_     structs.HostLayout
	Range UffdioRange
	Mode  uint64
}

// Sizes of userfaultfd structures.
const (
	SizeOfUffdioAPI          = 24
	SizeOfUffdioRegister     = 32
	SizeOfUffdioWriteprotect = 24
)

// PAGEMAP_SCAN is the ioctl on /proc/PID/pagemap that reports and
// write-protects pages by category, from include/uapi/linux/fs.h.
var PAGEMAP_SCAN = IOWR('f', 16, SizeOfPMScanArg)

// PAGEMAP_SCAN page categories.
const (
	PAGE_IS_WPALLOWED = 1 << 0
	PAGE_IS_WRITTEN   = 1 << 1
)

// PAGEMAP_SCAN flags.
const (
	PM_SCAN_WP_MATCHING   = 1 << 0
	PM_SCAN_CHECK_WPASYNC = 1 << 1
)

// PageRegion is struct page_region.
type PageRegion struct {
	_          structs.HostLayout
	Start      uint64
	End        uint64
	Categories uint64
}

// PMScanArg is struct pm_scan_arg.
type PMScanArg struct {
	_                 structs.HostLayout
	Size              uint64
	Flags             uint64
	Start             uint64
	End               uint64
	WalkEnd           uint64
	Vec               uint64
	VecLen            uint64
	MaxPages          uint64
	CategoryInverted  uint64
	CategoryMask      uint64
	CategoryAnyofMask uint64
	ReturnMask        uint64
}

// SizeOfPMScanArg is the size of struct pm_scan_arg.
const SizeOfPMScanArg = 96
