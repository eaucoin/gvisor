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

package kernel

import (
	"fmt"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// WriteProtectDirtySource is a DirtySource that write-protects application
// memory in the Sentry: arming withholds write permission from every pma of
// every MemoryManager that maps a tracked MemoryFile, so that the first write
// to each tracking unit faults into the Sentry, which records it (see
// mm/dirty.go). It needs nothing from the platform or the host.
type WriteProtectDirtySource struct {
	k *Kernel

	// unit is the tracking unit in bytes. unit is protected by k's dirty
	// tracking callers, which are serialized.
	unit uint64
}

// NewWriteProtectDirtySource returns a WriteProtectDirtySource for k that
// tracks writes in units of unit bytes, a power of 2 of at least a page.
func NewWriteProtectDirtySource(k *Kernel, unit uint64) *WriteProtectDirtySource {
	return &WriteProtectDirtySource{k: k, unit: unit}
}

// Name implements DirtySource.Name.
func (*WriteProtectDirtySource) Name() string {
	return "wp"
}

// SetUnit changes the tracking unit from the next call to Arm.
func (s *WriteProtectDirtySource) SetUnit(unit uint64) {
	s.unit = unit
}

// Arm implements DirtySource.Arm. If the kernel is running, Arm pauses it
// while it arms every MemoryManager, so that it can reach every
// MemoryManager, including those of tasks in the middle of execve.
func (s *WriteProtectDirtySource) Arm(ctx context.Context, paused bool) error {
	start := time.Now()
	if !paused {
		s.k.Pause()
		defer s.k.Unpause()
	}
	mms := s.k.memoryManagersPaused()
	if pgalloc.LoggingDirtyAttribution() {
		s.k.logDirtyAttributionPaused(mms)
	}
	for _, m := range mms {
		m.ArmDirtyTracking(s.unit)
	}
	log.Infof("Dirty tracking: armed %d MemoryManagers in %v (unit %d bytes)", len(mms), time.Since(start), s.unit)
	return nil
}

// Harvest implements DirtySource.Harvest. First writes are recorded when they
// fault, so there is nothing to harvest.
func (*WriteProtectDirtySource) Harvest(context.Context) error {
	return nil
}

// memoryManagersPaused returns every MemoryManager of k's tasks, including
// those that tasks in the middle of execve will switch to.
//
// Preconditions: The kernel must be paused.
func (k *Kernel) memoryManagersPaused() []*mm.MemoryManager {
	seen := make(map[*mm.MemoryManager]struct{})
	var mms []*mm.MemoryManager
	add := func(m *mm.MemoryManager) {
		if m == nil {
			return
		}
		if _, ok := seen[m]; ok {
			return
		}
		seen[m] = struct{}{}
		mms = append(mms, m)
	}
	k.tasks.mu.RLock()
	defer k.tasks.mu.RUnlock()
	for t := range k.tasks.Root.tids {
		// We can skip locking Task.mu here since the kernel is paused.
		add(t.image.MemoryManager)
		if r, ok := t.runState.(*runExecveAfterSiblingExitStop); ok {
			add(r.image.MemoryManager)
		}
	}
	return mms
}

// dirtyAttributionVMAs is the number of vmas, largest first, that
// logDirtyAttributionPaused names for each MemoryManager.
const dirtyAttributionVMAs = 16

// logDirtyAttributionPaused logs, for each of mms, the thread groups that use
// it and what writes disarmed in its vmas since dirty tracking was last armed
// (a measurement build's, while pgalloc.LoggingDirtyAttribution).
//
// Preconditions: The kernel must be paused.
func (k *Kernel) logDirtyAttributionPaused(mms []*mm.MemoryManager) {
	users := make(map[*mm.MemoryManager][]string)
	k.tasks.mu.RLock()
	for tg, id := range k.tasks.Root.tgids {
		leader := tg.leader
		if leader == nil || leader.image.MemoryManager == nil {
			continue
		}
		m := leader.image.MemoryManager
		users[m] = append(users[m], fmt.Sprintf("%d %q", id, leader.Name()))
	}
	k.tasks.mu.RUnlock()
	for _, m := range mms {
		entries, total := m.TakeDirtyAttribution()
		if total == 0 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Dirty attribution: MemoryManager %p of thread groups [%s] disarmed %d bytes since the last arm, in %d vmas; largest:", m, strings.Join(users[m], ", "), total, len(entries))
		for i, e := range entries {
			if i == dirtyAttributionVMAs {
				break
			}
			fmt.Fprintf(&b, " [%#x-%#x %s %q: %d]", e.Start, e.End, e.Perms, e.Name, e.Bytes)
		}
		log.Infof("%s", b.String())
	}
}
