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

import (
	"testing"
	"unsafe"
)

// TestUserfaultfdABI checks the sizes of the userfaultfd and PAGEMAP_SCAN
// structures and the ioctl numbers derived from them against Linux's.
func TestUserfaultfdABI(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof(struct uffdio_api)", unsafe.Sizeof(UffdioAPI{}), SizeOfUffdioAPI},
		{"sizeof(struct uffdio_register)", unsafe.Sizeof(UffdioRegister{}), SizeOfUffdioRegister},
		{"sizeof(struct uffdio_writeprotect)", unsafe.Sizeof(UffdioWriteprotect{}), SizeOfUffdioWriteprotect},
		{"sizeof(struct pm_scan_arg)", unsafe.Sizeof(PMScanArg{}), SizeOfPMScanArg},
		{"sizeof(struct page_region)", unsafe.Sizeof(PageRegion{}), 24},
		// The values of the ioctl numbers on x86 and arm64.
		{"UFFDIO_API", uintptr(UFFDIO_API), 0xc018aa3f},
		{"UFFDIO_REGISTER", uintptr(UFFDIO_REGISTER), 0xc020aa00},
		{"UFFDIO_WRITEPROTECT", uintptr(UFFDIO_WRITEPROTECT), 0xc018aa06},
		{"PAGEMAP_SCAN", uintptr(PAGEMAP_SCAN), 0xc0606610},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %#x, want %#x", tc.name, tc.got, tc.want)
		}
	}
}
