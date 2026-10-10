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
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// TestMapFileDirtyTracking checks that mapping pages of a MemoryFile that
// tracks dirty pages into the guest, writable, does not report them dirty:
// the application's writes to them are recorded by dirty tracking's source.
func TestMapFileDirtyTracking(t *testing.T) {
	deviceFile, err := OpenDevice("")
	if err != nil {
		t.Fatalf("error opening device file: %v", err)
	}
	k, err := New(deviceFile, Config{})
	if err != nil {
		t.Fatalf("error creating KVM instance: %v", err)
	}
	defer k.machine.Destroy()
	as, err := k.NewAddressSpace()
	if err != nil {
		t.Fatalf("error creating address space: %v", err)
	}
	defer as.Release()

	memfd, err := memutil.CreateMemFD("kvm-dirty-test", 0)
	if err != nil {
		t.Fatalf("error creating memfd: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(os.NewFile(uintptr(memfd), "kvm-dirty-test"), pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatalf("error creating MemoryFile: %v", err)
	}
	defer mf.Destroy()
	fr, err := mf.Allocate(4*hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("error allocating: %v", err)
	}
	defer mf.DecRef(fr)

	mf.EnableDirtyTracking()
	const addr = hostarch.Addr(0x400000)
	if err := as.MapFile(addr, mf, fr, hostarch.ReadWrite, false /* precommit */); err != nil {
		t.Fatalf("MapFile: %v", err)
	}
	defer as.Unmap(addr, fr.Length())
	mf.SwapDirty(true /* paused */).ForEachRange(fr, func(dirty memmap.FileRange) bool {
		t.Errorf("pages %v of the MemoryFile reported dirty after MapFile, want none", dirty)
		return true
	})
}
