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

// Package platformdesc describes what each platform needs from the host. It
// does not import the platform implementations, so that host-side tools can
// use it without linking them.
package platformdesc

import (
	"fmt"
	"sort"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/fd"
)

// Requirements is used to specify platform specific requirements.
type Requirements struct {
	// RequiresCapSysPtrace indicates that the sandbox has to be started with
	// the CAP_SYS_PTRACE capability.
	RequiresCapSysPtrace bool

	// FrequentHostThreadWakeups indicates that the platform wakes sleeping
	// host threads at a very high rate.
	FrequentHostThreadWakeups bool

	// WriteTracking is how the platform supports write tracking.
	WriteTracking WriteTracking
}

// WriteTracking is how a platform supports write tracking: reporting the
// application's stores to MemoryFiles that track dirty pages (see
// pgalloc.MemoryFile.EnableDirtyTracking), which the Sentry does not see.
type WriteTracking int

const (
	// WriteTrackingUnsupported means that the platform cannot track writes.
	WriteTrackingUnsupported WriteTracking = iota

	// WriteTrackingInternalMappings means that the application writes to
	// MemoryFiles through the Sentry's internal mappings of them, so that
	// tracking the writes through those mappings
	// (pgalloc.EnableInternalWriteTracking), which the Sentry's own writes
	// go through, tracks the application's.
	WriteTrackingInternalMappings

	// WriteTrackingPlatform means that the application writes to
	// MemoryFiles through the platform's own mappings of them, whose writes
	// the platform tracks if it is created with
	// platform.Options.WriteTrackingProcFS; it then implements
	// platform.WriteTracker. The Sentry's own writes are tracked through its
	// internal mappings, as for WriteTrackingInternalMappings.
	WriteTrackingPlatform
)

// Desc describes a platform.
type Desc struct {
	// Device is the default path to the host device used by the
	// platform, or "" if the platform does not use a device.
	Device string

	// Requirements are the platform specific requirements.
	Requirements Requirements
}

// descs contains the descriptions of all available platforms. It must list
// the same platforms that pkg/sentry/platform/platforms registers.
var descs = map[string]Desc{
	"kvm": {
		Device: "/dev/kvm",
		Requirements: Requirements{
			// Guest-physical memory is the Sentry's address space: the
			// application writes to MemoryFiles through the Sentry's
			// mappings.
			WriteTracking: WriteTrackingInternalMappings,
		},
	},
	"ptrace": {
		Requirements: Requirements{
			RequiresCapSysPtrace: true,
		},
	},
	"systrap": {
		Requirements: Requirements{
			RequiresCapSysPtrace:      true,
			FrequentHostThreadWakeups: true,
			WriteTracking:             WriteTrackingPlatform,
		},
	},
}

// List lists available platforms.
func List() (available []string) {
	for name := range descs {
		available = append(available, name)
	}
	sort.Strings(available)
	return
}

// Lookup looks up the platform description by name.
func Lookup(name string) (Desc, error) {
	d, ok := descs[name]
	if !ok {
		return Desc{}, fmt.Errorf("unknown platform: %v", name)
	}
	return d, nil
}

// OpenDevice opens the path to the device used by the platform.
// Passing in an empty string will use the default path for the device,
// e.g. "/dev/kvm" for the KVM platform. It returns nil if the platform does
// not use a device.
func (d Desc) OpenDevice(devicePath string) (*fd.FD, error) {
	if d.Device == "" {
		return nil, nil
	}
	if devicePath == "" {
		devicePath = d.Device
	}
	f, err := fd.Open(devicePath, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("error opening %s: %v", devicePath, err)
	}
	return f, nil
}
