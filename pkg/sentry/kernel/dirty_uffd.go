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
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// uffdDirtySource is the "uffd" dirty source: it reports writes recorded by
// the host's userfaultfd write-protection in asynchronous mode and read with
// PAGEMAP_SCAN (pkg/sentry/hostmm), through the Sentry's internal mappings of
// MemoryFiles (pgalloc's write tracking), which the Sentry's writes to
// application memory go through and, on kvm, the application's, and through
// the platform's own mappings if it tracks them (platform.WriteTracker, on
// systrap).
//
// Harvesting write-protects the pages it reports again in the same page
// table walk, so Harvest re-arms what it harvests and Arm only arms what is
// not armed yet: MemoryFiles that became tracked, and their mappings.
type uffdDirtySource struct {
	k *Kernel

	// platform is the platform's write tracker, or nil.
	platform platform.WriteTracker
}

// NewUffdDirtySource returns the "uffd" dirty source of k. Write tracking of
// internal mappings must be enabled (pgalloc.EnableInternalWriteTracking).
// tracker is the platform's write tracker if the platform's write tracking
// is platform.WriteTrackingPlatform (it was created with
// platform.Options.WriteTrackingProcFS), and nil otherwise.
func NewUffdDirtySource(k *Kernel, tracker platform.WriteTracker) (DirtySource, error) {
	if !pgalloc.InternalWriteTrackingEnabled() {
		return nil, fmt.Errorf("write tracking is not enabled")
	}
	return &uffdDirtySource{k: k, platform: tracker}, nil
}

// Name implements DirtySource.Name.
func (*uffdDirtySource) Name() string {
	return "uffd"
}

// Arm implements DirtySource.Arm.
func (s *uffdDirtySource) Arm(ctx context.Context, paused bool) error {
	for _, mf := range s.k.dirty.mfs {
		if err := mf.ArmInternalWrites(); err != nil {
			return err
		}
	}
	if s.platform != nil {
		return s.platform.ArmWrites()
	}
	return nil
}

// Harvest implements DirtySource.Harvest.
func (s *uffdDirtySource) Harvest(ctx context.Context) error {
	start := time.Now()
	defer func() {
		log.Infof("Dirty tracking: harvested %d MemoryFiles in %v", len(s.k.dirty.mfs), time.Since(start))
	}()
	for _, mf := range s.k.dirty.mfs {
		// A MemoryFile that fails to harvest a chunk reports all of it
		// dirty: the harvest is still complete.
		if err := mf.HarvestInternalWrites(); err != nil {
			log.Warningf("Dirty source uffd: %v", err)
		}
	}
	if s.platform != nil {
		return s.platform.HarvestWrites()
	}
	return nil
}
