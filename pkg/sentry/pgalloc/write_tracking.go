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

package pgalloc

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sync"
)

// Write tracking of internal mappings.
//
// Some writes reach a MemoryFile's pages through the Sentry's internal
// mappings of it without being marked dirty: the Sentry's writes to
// application memory through mappings that the memory manager caches
// (MapInternalUntracked), and, on platforms that map application memory
// through the Sentry's mappings (kvm, whose guest-physical memory is the
// Sentry's address space), the application's stores. Write tracking finds
// them with userfaultfd write-protection in asynchronous mode (hostmm) on the
// internal mappings of every MemoryFile that tracks dirty pages:
// ArmInternalWrites write-protects them, and HarvestInternalWrites marks the
// pages written since as dirty and write-protects them again. Chunks added
// to an armed MemoryFile are write-protected as they are mapped.
//
// Write tracking sees every write through the internal mappings, the
// Sentry's own included, so async page loading writes the pages it loads
// through a second mapping of the file, which is not write-protected: the
// pages it loads are the contents of the image being loaded, not writes.

// internalWriteTracking holds the Sentry's userfaultfd and pagemap file for
// write tracking of internal mappings. Both are -1 unless
// EnableInternalWriteTracking was called; they are set before any
// MemoryFile is created or loaded and are immutable after.
var internalWriteTracking = struct {
	uffd    int
	pagemap int
}{-1, -1}

// EnableInternalWriteTracking enables write tracking of the internal mappings
// of MemoryFiles, with uffd, a userfaultfd of the Sentry's address space set
// up for write tracking (hostmm.NewWPAsyncUserfaultfd), and pagemap, the
// Sentry's /proc/self/pagemap. It must be called before any MemoryFile is
// created or loaded.
func EnableInternalWriteTracking(uffd, pagemap int) {
	internalWriteTracking.uffd = uffd
	internalWriteTracking.pagemap = pagemap
}

// InternalWriteTrackingEnabled returns true if EnableInternalWriteTracking was
// called.
func InternalWriteTrackingEnabled() bool {
	return internalWriteTracking.uffd >= 0
}

// ArmInternalWrites write-protects the internal mappings of f's chunks that
// are not write-protected yet, so that writes through them are reported by
// HarvestInternalWrites, and makes f write-protect the chunks it adds from
// then on.
//
// Preconditions:
//   - InternalWriteTrackingEnabled() == true.
//   - Dirty tracking is enabled for f.
func (f *MemoryFile) ArmInternalWrites() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty.writesArmed = true
	return f.armInternalWritesLocked(f.chunksLoad())
}

// armInternalWritesLocked write-protects the mappings of chunks, f's chunks
// to be, from the first that is not write-protected yet. If it fails, the
// chunks it did not write-protect are reported written whole by harvests.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) armInternalWritesLocked(chunks []chunkInfo) error {
	for i := f.dirty.armedChunks; i < len(chunks); i++ {
		if err := hostmm.WriteProtectRange(internalWriteTracking.uffd, chunks[i].mapping, chunkSize); err != nil {
			return fmt.Errorf("MemoryFile(%p): write-protecting chunk %d: %w", f, i, err)
		}
		f.dirty.armedChunks = i + 1
	}
	return nil
}

// HarvestInternalWrites marks as dirty the pages of f written through its
// internal mappings since they were write-protected or last harvested, and
// write-protects them again. It marks chunks that are not write-protected,
// and chunks whose written pages it cannot read, dirty whole; it returns the
// first error of the latter.
//
// Preconditions: As for ArmInternalWrites.
func (f *MemoryFile) HarvestInternalWrites() error {
	f.mu.Lock()
	chunks := f.chunksLoad()
	armed := f.dirty.armedChunks
	f.mu.Unlock()
	buf := pagemapScanBufPool.Get().(*hostmm.PagemapScanBuf)
	defer pagemapScanBufPool.Put(buf)
	var firstErr error
	for i := range chunks {
		base := uint64(i) * chunkSize
		if i >= armed {
			f.MarkDirtyBy(DirtyMarkUffdInternal, memmap.FileRange{Start: base, End: base + chunkSize})
			continue
		}
		m := chunks[i].mapping
		if err := buf.HarvestWritten(internalWriteTracking.pagemap, m, m+chunkSize, func(start, end uintptr) {
			f.MarkDirtyBy(DirtyMarkUffdInternal, memmap.FileRange{Start: base + uint64(start-m), End: base + uint64(end-m)})
		}); err != nil {
			f.MarkDirtyBy(DirtyMarkUffdInternal, memmap.FileRange{Start: base, End: base + chunkSize})
			if firstErr == nil {
				firstErr = fmt.Errorf("MemoryFile(%p): harvesting writes to chunk %d: %w", f, i, err)
			}
		}
	}
	return firstErr
}

// pagemapScanBufPool holds *hostmm.PagemapScanBuf.
var pagemapScanBufPool = sync.Pool{
	New: func() any { return new(hostmm.PagemapScanBuf) },
}

// mapLoadMapping returns the address of a second mapping of f's file of
// fileSize bytes, madvised as f's chunks are, through which async page
// loading writes, or 0 if internal mappings are not write-tracked: then
// async page loading writes through the internal mappings.
func (f *MemoryFile) mapLoadMapping(chunks []chunkInfo, fileSize uint64) (uintptr, error) {
	if !InternalWriteTrackingEnabled() || fileSize == 0 {
		return 0, nil
	}
	m, _, errno := unix.Syscall6(unix.SYS_MMAP, 0, uintptr(fileSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED, f.file.Fd(), 0)
	if errno != 0 {
		return 0, fmt.Errorf("failed to mmap MemoryFile for async page loading: %w", errno)
	}
	for i := range chunks {
		f.madviseChunkMapping(m+uintptr(i)*chunkSize, chunkSize, chunks[i].huge)
	}
	return m, nil
}

// unmapLoadMapping unmaps a mapping returned by mapLoadMapping.
func unmapLoadMapping(m uintptr, fileSize uint64) {
	if _, _, errno := unix.Syscall(unix.SYS_MUNMAP, m, uintptr(fileSize), 0); errno != 0 {
		log.Warningf("Failed to unmap the async page loading mapping %#x-%#x: %v", m, m+uintptr(fileSize), errno)
	}
}
