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
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/mm"
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
	for _, m := range mms {
		m.ArmDirtyTracking(s.unit)
	}
	log.Infof("Dirty tracking: armed %d MemoryManagers in %v (unit %d bytes)", len(mms), time.Since(start), s.unit)
	return nil
}

// preArmStep is the most memory that preArm arms at once in a MemoryManager,
// holding its locks: at the 37-96 ms per GiB that arming costs on systrap, a
// task of the MemoryManager that faults meanwhile waits at most a few
// milliseconds.
const preArmStep = 64 << 20

// preArm arms, while tasks run, every MemoryManager that it reaches, each
// under its own locks, preArmStep bytes at a time, and without pausing the
// kernel. MemoryManagers that it does not reach, such as those of tasks in the
// middle of execve, are armed by the next Arm. It returns how many it armed,
// in how many steps, and the longest step.
func (s *WriteProtectDirtySource) preArm(ctx context.Context) (n, steps int, longest time.Duration) {
	seen := make(map[*mm.MemoryManager]struct{})
	var mms []*mm.MemoryManager
	s.k.tasks.mu.RLock()
	for t := range s.k.tasks.Root.tids {
		t.WithMuLocked(func(t *Task) {
			m := t.image.MemoryManager
			if m == nil {
				return
			}
			if _, ok := seen[m]; ok || !m.IncUsers() {
				return
			}
			seen[m] = struct{}{}
			mms = append(mms, m)
		})
	}
	s.k.tasks.mu.RUnlock()
	for _, m := range mms {
		mSteps, mLongest := m.ArmDirtyTrackingInSteps(s.unit, preArmStep)
		steps += mSteps
		longest = max(longest, mLongest)
		m.DecUsers(ctx)
	}
	return len(mms), steps, longest
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
