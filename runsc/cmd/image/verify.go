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

package image

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/subcommands"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/version"
)

// verify implements subcommands.Command for the "image verify" command.
type verify struct {
	layerPaths stringSlice
	pages      bool
	host       bool
}

// Name implements subcommands.Command.
func (*verify) Name() string {
	return "verify"
}

// Synopsis implements subcommands.Command.
func (*verify) Synopsis() string {
	return "check that a checkpoint image is intact and restorable"
}

// Usage implements subcommands.Command.
func (*verify) Usage() string {
	return `verify [--layer-path=DIR]... [--pages] [--host] IMAGE - check the checkpoint image IMAGE, an image directory or a container engine's checkpoint archive of a runsc container:
the checksums and consistency of its pages metadata file, the identity and size of each of its layers,
and with --pages, the contents of every page against its hash;
with --host, that this runsc can restore it on this host: same runsc version, every CPU feature it was saved with, and the platform it was saved on available (restore uses the platform it is given).
`
}

// SetFlags implements subcommands.Command.
func (v *verify) SetFlags(f *flag.FlagSet) {
	f.Var(&v.layerPaths, "layer-path", "directory in which to look for the image's layers besides IMAGE/layers, as for runsc restore; can be repeated, or given comma-separated")
	f.BoolVar(&v.pages, "pages", false, "read every page and check it against its hash")
	f.BoolVar(&v.host, "host", false, "check that this runsc can restore the image on this host")
}

// Execute implements subcommands.Command.
func (v *verify) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() != 1 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	d, closeImage, err := openImage(f.Arg(0), v.layerPaths, v.pages)
	if err != nil {
		return exitStatus(err)
	}
	defer closeImage()
	files, err := d.openLayers()
	if err != nil {
		return exitStatus(err)
	}
	defer closeAll(files)
	if a := d.archive; a != nil {
		fmt.Printf("Checkpoint of %s container %q in %s\n", a.container.Engine, a.container.Name, a.path)
	}
	fmt.Printf("Image %v: metadata and %d layers OK\n", d.img.Digest, len(files))
	if v.pages {
		if err := checkpointimage.VerifyPages(d.img, readersOf(files)); err != nil {
			return exitStatus(err)
		}
		fmt.Printf("Pages OK\n")
	}
	if v.host {
		if err := checkHost(d.state); err != nil {
			fmt.Printf("Not restorable here: %v\n", err)
			return exitNotRestorable
		}
		fmt.Printf("Restorable by this runsc on this host\n")
	}
	return subcommands.ExitSuccess
}

// checkHost checks that this runsc binary can restore, on this host, the
// image whose state file metadata is state.
func checkHost(state map[string]string) error {
	if got, want := state[boot.VersionKey], version.Version(); got != want {
		return fmt.Errorf("saved by runsc %q, this is runsc %q", got, want)
	}
	if p := state[boot.PlatformKey]; p != "" {
		c, err := platform.Lookup(p)
		if err != nil {
			return fmt.Errorf("platform %q: %w", p, err)
		}
		dev, err := c.OpenDevice("")
		if err != nil {
			return fmt.Errorf("platform %q: %w", p, err)
		}
		if dev != nil {
			dev.Close()
		}
	}
	if fs, ok := state[boot.CPUFeaturesKey]; ok {
		cpuid.Initialize()
		host := cpuid.HostFeatureSet().Fixed()
		var missing []string
		for _, name := range strings.Split(fs, ",") {
			if name == "" {
				continue
			}
			f, ok := cpuid.FeatureFromString(name)
			if !ok || !host.HasFeature(f) {
				missing = append(missing, name)
			}
		}
		if len(missing) != 0 {
			return fmt.Errorf("the host lacks CPU features %s", strings.Join(missing, ","))
		}
	}
	return nil
}
