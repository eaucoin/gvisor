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

// Package image implements the "runsc image" command group, which reads,
// checks and rewrites checkpoint images without a sandbox.
package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
)

// Exit statuses of the image commands, beyond subcommands.ExitSuccess,
// ExitFailure (an I/O or other error) and ExitUsageError.
const (
	// exitInvalid means that the image is malformed, inconsistent or
	// corrupt.
	exitInvalid subcommands.ExitStatus = 3

	// exitLayerNotFound means that a layer of the image was not found.
	exitLayerNotFound subcommands.ExitStatus = 4

	// exitNotRestorable means that this runsc binary cannot restore the
	// image on this host.
	exitNotRestorable subcommands.ExitStatus = 5
)

// Image implements subcommands.Command for the "image" command.
type Image struct{}

// Name implements subcommands.Command.
func (*Image) Name() string {
	return "image"
}

// Synopsis implements subcommands.Command.
func (*Image) Synopsis() string {
	return "inspect, verify and rewrite checkpoint images"
}

// Usage implements subcommands.Command.
func (*Image) Usage() string {
	buf := bytes.Buffer{}
	buf.WriteString(`Usage: image <subcommand> <subcommand args>

The image commands work on uncompressed checkpoint images (directories written
by "runsc checkpoint --image-path") without a sandbox. They never modify an
image: commands that rewrite one write a new image directory.

inspect, verify and layers also read the checkpoint archives that container
engines (containerd, CRI-O, Podman) make of runsc containers, as checkpointctl
does: tar files, uncompressed or compressed with gzip or zstd, whose
checkpoint/ directory is the image. Installed as checkpointctl-runsc (a link to
runsc named so) in PATH, runsc is a checkpointctl plugin: "checkpointctl runsc
inspect ARCHIVE" runs "runsc image inspect ARCHIVE".

Exit status: 0 on success, 2 on a usage error, 3 if the image is invalid or
corrupt, 4 if a layer of the image is missing, 5 if this runsc cannot restore
the image on this host (verify --host), 1 on any other error.

`)
	cdr := createCommander(&flag.FlagSet{})
	cdr.VisitGroups(func(grp *subcommands.CommandGroup) {
		cdr.ExplainGroup(&buf, grp)
	})
	return buf.String()
}

// SetFlags implements subcommands.Command.
func (*Image) SetFlags(*flag.FlagSet) {}

// FetchSpec implements util.SubCommand.FetchSpec.
func (*Image) FetchSpec(*config.Config, *flag.FlagSet) (string, *specs.Spec, error) {
	// No container is involved.
	return "", nil, nil
}

// Execute implements subcommands.Command.
func (*Image) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	status := createCommander(f).Execute(ctx, args...)
	// runsc exits with a status of its own when a command fails, so the
	// command's status, which tells failures apart, is runsc's exit status
	// through the wait status, as for commands that run a container.
	waitStatus := args[1].(*unix.WaitStatus)
	*waitStatus = unix.WaitStatus(status << 8)
	return subcommands.ExitSuccess
}

func createCommander(f *flag.FlagSet) *subcommands.Commander {
	cdr := subcommands.NewCommander(f, "image")
	cdr.Register(cdr.HelpCommand(), "")
	cdr.Register(new(inspect), "")
	cdr.Register(new(verify), "")
	cdr.Register(new(flatten), "")
	cdr.Register(new(compact), "")
	cdr.Register(new(rebase), "")
	cdr.Register(new(layers), "")
	return cdr
}

// exitStatus reports err and returns the exit status for it.
func exitStatus(err error) subcommands.ExitStatus {
	util.Errorf("%v", err)
	switch {
	case errors.Is(err, checkpointimage.ErrLayerNotFound):
		return exitLayerNotFound
	case errors.Is(err, checkpointimage.ErrFormat), errors.Is(err, checkpointimage.ErrPageMismatch):
		return exitInvalid
	default:
		return subcommands.ExitFailure
	}
}

// stringSlice is a flag that holds a list of values, given by repeating the
// flag, by separating them with commas, or both. Set(String()) is idempotent.
type stringSlice []string

// String implements flag.Value.String.
func (ss *stringSlice) String() string {
	return strings.Join(*ss, ",")
}

// Get implements flag.Getter.Get.
func (ss *stringSlice) Get() any {
	return ss
}

// Set implements flag.Value.Set.
func (ss *stringSlice) Set(s string) error {
	for _, v := range strings.Split(s, ",") {
		if v == "" {
			return fmt.Errorf("empty value in %q", s)
		}
		*ss = append(*ss, v)
	}
	return nil
}
