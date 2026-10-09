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
	"path/filepath"

	"github.com/google/subcommands"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/runsc/flag"
)

// layers implements subcommands.Command for the "image layers" command.
type layers struct{}

// Name implements subcommands.Command.
func (*layers) Name() string {
	return "layers"
}

// Synopsis implements subcommands.Command.
func (*layers) Synopsis() string {
	return "print the digests of the images that a checkpoint image refers to"
}

// Usage implements subcommands.Command.
func (*layers) Usage() string {
	return `layers IMAGE - print the digest of each image whose pages file holds data of the checkpoint image IMAGE, one per line, for an image store's garbage collection: an image can be deleted when no image kept lists it.
`
}

// SetFlags implements subcommands.Command.
func (*layers) SetFlags(*flag.FlagSet) {}

// Execute implements subcommands.Command.
func (*layers) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() != 1 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	img, err := checkpointimage.ReadMetadataFile(filepath.Join(f.Arg(0), checkpointfiles.PagesMetadataFileName))
	if err != nil {
		return exitStatus(err)
	}
	for _, l := range img.Layers()[1:] {
		fmt.Println(l.Digest)
	}
	return subcommands.ExitSuccess
}
