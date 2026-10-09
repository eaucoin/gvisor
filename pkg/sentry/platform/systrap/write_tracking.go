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

package systrap

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sync"
)

// Write tracking (platform.WriteTracker).
//
// Application stores through a stub's mappings of a MemoryFile change its
// pages without the Sentry seeing them. With write tracking, the writes
// through each stub's mappings of MemoryFiles that track dirty pages are
// tracked with userfaultfd write-protection in asynchronous mode (hostmm). A
// userfaultfd tracks the address space of the process that creates it, so
// each stub creates its own, by an injected userfaultfd(2), before its first
// mapping of a MemoryFile; stubs share the Sentry's FD table, so the Sentry
// uses it directly. The Sentry reads which pages a stub wrote from the stub's
// /proc/PID/pagemap, which it opens through a procfs directory FD that it
// opened at startup: /proc is not mounted in the sandbox after startup, and
// stubs never open files.
//
// A mapping of a MemoryFile that tracks dirty pages is write-protected when
// it is mapped, before it is writable; others are by Systrap.ArmWrites once
// their MemoryFile tracks dirty pages, which happens with the kernel paused.
// The writes through a mapping are harvested by Systrap.HarvestWrites, and
// before the mapping is unmapped or replaced, which loses its
// write-protection state; it is made inaccessible first, so that no write
// lands between the harvest and the unmapping.

// writeTracking is the state of write tracking.
var writeTracking = struct {
	// procFD is a directory FD of a procfs (platform.Options.WriteTrackingProcFS)
	// of the Sentry's PID namespace or of an ancestor, or -1 if write
	// tracking is disabled. procSelf is the Sentry's PID in that procfs.
	// Both are set by New before any stub is created, and are immutable
	// after.
	procFD   int
	procSelf int

	// mu protects stubs.
	mu sync.Mutex

	// stubs are the live stubs that have set up write tracking.
	stubs map[*subprocess]struct{}
}{procFD: -1}

// writeTrackingEnabled returns true if write tracking is enabled.
func writeTrackingEnabled() bool {
	return writeTracking.procFD >= 0
}

// enableWriteTracking enables write tracking with procFD, a directory FD of a
// procfs of the Sentry's PID namespace or of an ancestor. It must be called
// before any stub is created.
func enableWriteTracking(procFD int) error {
	buf := make([]byte, 32)
	n, err := unix.Readlinkat(procFD, "self", buf)
	if err != nil {
		return fmt.Errorf("reading self in procfs: %w", err)
	}
	pid, err := strconv.Atoi(string(buf[:n]))
	if err != nil {
		return fmt.Errorf("self in procfs is %q: %w", buf[:n], err)
	}
	writeTracking.procFD = procFD
	writeTracking.procSelf = pid
	writeTracking.stubs = make(map[*subprocess]struct{})
	return nil
}

// stubWriteTracking is the write tracking state of a stub.
type stubWriteTracking struct {
	// mu serializes changes to the stub's mappings of MemoryFiles with
	// harvests, and protects the fields below. mu is ordered before
	// subprocess.aliveMu and subprocess.syscallThreadMu.
	mu sync.Mutex

	// uffd and pagemap are the stub's userfaultfd and pagemap file, or -1
	// before they are set up.
	uffd    int
	pagemap int

	// mappings are the stub's mappings of MemoryFiles, sorted by address
	// and non-overlapping.
	mappings []trackedMapping

	// scanBuf is the buffer of harvests, allocated with uffd.
	scanBuf *hostmm.PagemapScanBuf
}

// trackedMapping is a stub's mapping of a MemoryFile.
type trackedMapping struct {
	start, end uintptr
	mf         *pgalloc.MemoryFile
	off        uint64

	// armed is true if the mapping is write-protected for tracking.
	armed bool
}

// setUpWriteTrackingLocked creates the stub's userfaultfd and opens its
// pagemap, if not done yet.
//
// Preconditions: s.writes.mu must be locked.
func (s *subprocess) setUpWriteTrackingLocked() error {
	w := &s.writes
	if w.uffd >= 0 {
		return nil
	}
	fd, err := s.syscall(unix.SYS_USERFAULTFD, arch.SyscallArgument{Value: hostmm.UserfaultfdFlags})
	if err != nil {
		return fmt.Errorf("creating the stub's userfaultfd: %w", err)
	}
	if err := hostmm.UserfaultfdAPI(int(fd)); err != nil {
		unix.Close(int(fd))
		return err
	}
	pid, err := procPID(int(s.syscallThread.thread.tgid))
	if err != nil {
		unix.Close(int(fd))
		return err
	}
	pagemap, err := unix.Openat(writeTracking.procFD, fmt.Sprintf("%d/pagemap", pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(int(fd))
		return fmt.Errorf("opening the pagemap of stub %d: %w", pid, err)
	}
	w.uffd, w.pagemap = int(fd), pagemap
	w.scanBuf = new(hostmm.PagemapScanBuf)
	writeTracking.mu.Lock()
	writeTracking.stubs[s] = struct{}{}
	writeTracking.mu.Unlock()
	return nil
}

// procPID returns the PID in write tracking's procfs of the process whose PID
// in the Sentry's PID namespace is pid. If the procfs is that of an ancestor
// namespace, as when the sandbox runs without its own procfs, procfs shows it
// in the fdinfo of a pidfd of the process.
func procPID(pid int) (int, error) {
	if writeTracking.procSelf == os.Getpid() {
		return pid, nil
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return 0, fmt.Errorf("pidfd_open(%d): %w", pid, err)
	}
	defer unix.Close(pidfd)
	f, err := unix.Openat(writeTracking.procFD, fmt.Sprintf("%d/fdinfo/%d", writeTracking.procSelf, pidfd), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("opening the fdinfo of a pidfd: %w", err)
	}
	defer unix.Close(f)
	buf := make([]byte, 512)
	n, err := unix.Read(f, buf)
	if err != nil {
		return 0, fmt.Errorf("reading the fdinfo of a pidfd: %w", err)
	}
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if v, ok := strings.CutPrefix(line, "Pid:\t"); ok {
			if p, err := strconv.Atoi(v); err == nil && p > 0 {
				return p, nil
			}
			return 0, fmt.Errorf("process %d has PID %q in procfs", pid, v)
		}
	}
	return 0, fmt.Errorf("no PID in the fdinfo of a pidfd of process %d", pid)
}

// releaseWriteTracking releases the write tracking state of s, which is dead,
// after reporting the writes through its mappings: their written pages cannot
// be read anymore, so they are reported written whole.
func (s *subprocess) releaseWriteTracking() {
	writeTracking.mu.Lock()
	delete(writeTracking.stubs, s)
	writeTracking.mu.Unlock()
	w := &s.writes
	w.mu.Lock()
	defer w.mu.Unlock()
	s.harvestLocked(0, len(w.mappings))
	if w.uffd >= 0 {
		unix.Close(w.uffd)
		unix.Close(w.pagemap)
		w.uffd, w.pagemap = -1, -1
	}
	w.mappings = nil
}

// overlapping returns the indexes [i, j) of the mappings that overlap
// [start, end).
//
// Preconditions: w.mu must be locked.
func (w *stubWriteTracking) overlapping(start, end uintptr) (int, int) {
	i := sort.Search(len(w.mappings), func(i int) bool { return w.mappings[i].end > start })
	j := i
	for j < len(w.mappings) && w.mappings[j].start < end {
		j++
	}
	return i, j
}

// harvestLocked reports the writes through the mappings in [i, j) of
// MemoryFiles that track dirty pages to the MemoryFiles. A mapping that is
// not write-protected, because arming it failed, or whose written pages
// cannot be read is reported written whole. So are all mappings if the stub
// died: the address space of a dead process has no pages to scan.
//
// Preconditions: s.writes.mu must be locked.
func (s *subprocess) harvestLocked(i, j int) {
	w := &s.writes
	for k := i; k < j; k++ {
		m := &w.mappings[k]
		if !m.mf.DirtyTracked() {
			continue
		}
		if !m.armed || s.dead.Load() {
			m.mf.MarkDirty(m.fileRange())
			continue
		}
		if err := w.scanBuf.HarvestWritten(w.pagemap, m.start, m.end, func(start, end uintptr) {
			m.mf.MarkDirty(memmap.FileRange{Start: m.off + uint64(start-m.start), End: m.off + uint64(end-m.start)})
		}); err != nil || s.dead.Load() {
			if err != nil {
				log.Warningf("Harvesting the writes through stub mapping %#x-%#x: %v; reporting all of it as written", m.start, m.end, err)
			}
			m.mf.MarkDirty(m.fileRange())
		}
	}
}

// fileRange returns the range of m's MemoryFile that m maps.
func (m *trackedMapping) fileRange() memmap.FileRange {
	return memmap.FileRange{Start: m.off, End: m.off + uint64(m.end-m.start)}
}

// removeLocked harvests and forgets the mappings in [start, end), which are
// about to be unmapped or replaced. It first makes them inaccessible, so
// that no write through them lands between the harvest and their removal.
//
// Preconditions: s.writes.mu must be locked.
func (s *subprocess) removeLocked(start, end uintptr) {
	w := &s.writes
	i, j := w.overlapping(start, end)
	if i == j {
		return
	}
	for k := i; k < j; k++ {
		m := &w.mappings[k]
		if !m.armed {
			continue
		}
		rs, re := max(m.start, start), min(m.end, end)
		if _, err := s.syscall(unix.SYS_MPROTECT, arch.SyscallArgument{Value: rs}, arch.SyscallArgument{Value: re - rs}, arch.SyscallArgument{Value: unix.PROT_NONE}); err != nil {
			// Writes may still land: report the range written.
			m.mf.MarkDirty(memmap.FileRange{Start: m.off + uint64(rs-m.start), End: m.off + uint64(re-m.start)})
		}
	}
	s.harvestLocked(i, j)
	// Keep the parts of the first and last mappings outside [start, end);
	// they stay registered and write-protected.
	var keep []trackedMapping
	if first := w.mappings[i]; first.start < start {
		first.end = start
		keep = append(keep, first)
	}
	if last := w.mappings[j-1]; last.end > end {
		last.off += uint64(end - last.start)
		last.start = end
		keep = append(keep, last)
	}
	w.mappings = append(w.mappings[:i], append(keep, w.mappings[j:]...)...)
}

// addLocked records m, a new mapping of the stub.
//
// Preconditions:
//   - w.mu must be locked.
//   - No recorded mapping overlaps m.
func (w *stubWriteTracking) addLocked(m trackedMapping) {
	i := sort.Search(len(w.mappings), func(i int) bool { return w.mappings[i].start >= m.start })
	w.mappings = append(w.mappings, trackedMapping{})
	copy(w.mappings[i+1:], w.mappings[i:])
	w.mappings[i] = m
}

// mapFileTracked maps fr of f at addr, as MapFile does, tracking the writes
// through the new mapping if f is a MemoryFile.
func (s *subprocess) mapFileTracked(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	w := &s.writes
	w.mu.Lock()
	defer w.mu.Unlock()
	mf, ok := f.(*pgalloc.MemoryFile)
	if ok {
		if err := s.setUpWriteTrackingLocked(); err != nil {
			return err
		}
	}
	m := trackedMapping{
		start: uintptr(addr),
		end:   uintptr(addr) + uintptr(fr.Length()),
		mf:    mf,
		off:   fr.Start,
	}
	// The new mapping replaces the mappings in its range.
	s.removeLocked(m.start, m.end)
	if !ok || !mf.DirtyTracked() {
		if err := s.mapFile(addr, f, fr, at, precommit); err != nil {
			return err
		}
		if ok {
			w.addLocked(m)
		}
		return nil
	}
	// Map the pages without write access until they are write-protected,
	// so that no write lands before: a concurrent write faults into the
	// Sentry, which retries it once this mapping is complete.
	ro := at
	ro.Write = false
	if err := s.mapFile(addr, f, fr, ro, precommit); err != nil {
		return err
	}
	err := hostmm.WriteProtectRange(w.uffd, m.start, m.end-m.start)
	if err == nil && at.Write {
		_, err = s.syscall(unix.SYS_MPROTECT, arch.SyscallArgument{Value: m.start}, arch.SyscallArgument{Value: m.end - m.start}, arch.SyscallArgument{Value: uintptr(at.Prot())})
	}
	if err != nil {
		s.munmap(addr, fr.Length())
		return fmt.Errorf("tracking the writes through stub mapping %#x-%#x: %w", m.start, m.end, err)
	}
	m.armed = true
	w.addLocked(m)
	return nil
}

// unmapTracked unmaps [addr, addr+length), as Unmap does, after harvesting
// the writes through the mappings there.
func (s *subprocess) unmapTracked(addr hostarch.Addr, length uint64) {
	w := &s.writes
	w.mu.Lock()
	defer w.mu.Unlock()
	s.removeLocked(uintptr(addr), uintptr(addr)+uintptr(length))
	s.munmap(addr, length)
}

// ArmWrites implements platform.WriteTracker.ArmWrites.
func (*Systrap) ArmWrites() error {
	return forEachWriteTrackingStub(func(s *subprocess) error {
		w := &s.writes
		for k := range w.mappings {
			m := &w.mappings[k]
			if m.armed || !m.mf.DirtyTracked() {
				continue
			}
			if err := hostmm.WriteProtectRange(w.uffd, m.start, m.end-m.start); err != nil {
				// Harvests report the mapping written whole until
				// arming it succeeds.
				return err
			}
			m.armed = true
		}
		return nil
	})
}

// HarvestWrites implements platform.WriteTracker.HarvestWrites.
func (*Systrap) HarvestWrites() error {
	return forEachWriteTrackingStub(func(s *subprocess) error {
		s.harvestLocked(0, len(s.writes.mappings))
		return nil
	})
}

// forEachWriteTrackingStub calls fn on each stub that set up write tracking
// and was not released, with s.writes.mu locked.
func forEachWriteTrackingStub(fn func(s *subprocess) error) error {
	writeTracking.mu.Lock()
	stubs := make([]*subprocess, 0, len(writeTracking.stubs))
	for s := range writeTracking.stubs {
		stubs = append(stubs, s)
	}
	writeTracking.mu.Unlock()
	for _, s := range stubs {
		s.writes.mu.Lock()
		err := fn(s)
		s.writes.mu.Unlock()
		if err != nil && !s.dead.Load() {
			return fmt.Errorf("stub %d: %w", s.syscallThread.thread.tgid, err)
		}
	}
	return nil
}
