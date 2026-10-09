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

package boot

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// setUpWriteTracking prepares the host side of the "uffd" dirty source
// (kernel.NewUffdDirtySource) for the platform platformName. It must be
// called before the platform and any MemoryFile are created, while /proc is
// mounted.
//
// The writes through the Sentry's internal mappings of MemoryFiles are always
// tracked (pgalloc.EnableInternalWriteTracking): the Sentry writes to
// application memory through internal mappings that it does not mark dirty
// (pgalloc.MemoryFile.MapInternalUntracked), and on kvm the application does
// too. For a platform that also tracks the writes through its own mappings
// (platform.WriteTrackingPlatform), setUpWriteTracking returns the procfs FD
// to create the platform with (platform.Options.WriteTrackingProcFS);
// otherwise it returns nil.
//
// It returns an error if the platform or the host cannot track writes.
func setUpWriteTracking(platformName string) (*fd.FD, error) {
	p, err := platform.Lookup(platformName)
	if err != nil {
		return nil, err
	}
	var procFS *fd.FD
	switch p.Requirements().WriteTracking {
	case platform.WriteTrackingInternalMappings:
	case platform.WriteTrackingPlatform:
		// The root of the procfs that /proc/self is in: in the sandbox's
		// chroot, /proc only links to it. Without a chroot, it may be a
		// procfs of an ancestor PID namespace.
		procFD, err := unix.Open("/proc/self/..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("opening procfs: %w", err)
		}
		procFS = fd.New(procFD)
	default:
		return nil, fmt.Errorf("platform %s cannot track writes", platformName)
	}
	// The platform's stubs, if any, create their userfaultfds as the Sentry
	// does.
	uffd, err := hostmm.NewWPAsyncUserfaultfd()
	if err != nil {
		if procFS != nil {
			procFS.Close()
		}
		return nil, err
	}
	pagemap, err := unix.Open("/proc/self/pagemap", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(uffd)
		if procFS != nil {
			procFS.Close()
		}
		return nil, fmt.Errorf("opening /proc/self/pagemap: %w", err)
	}
	pgalloc.EnableInternalWriteTracking(uffd, pagemap)
	return procFS, nil
}
