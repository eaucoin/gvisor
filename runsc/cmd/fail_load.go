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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
)

// FailLoad implements subcommands.Command for the "fail-load" command.
type FailLoad struct {
	containerLoader
	reason string
}

// Name implements subcommands.Command.Name.
func (*FailLoad) Name() string {
	return "fail-load"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*FailLoad) Synopsis() string {
	return "end the background loading of a restored sandbox's pages as a failed read does"
}

// Usage implements subcommands.Command.Usage.
func (*FailLoad) Usage() string {
	return `fail-load [flags] <container id> - end the loading of the pages of the
restore of the container's sandbox that goes on in the background (restore
--background) as a failed read of its pages file ends it, whether or not the
store answers: the pages not loaded yet are never loaded, and a checkpoint of
the sandbox, which would wait for them, fails instead, at once if it waits
already. It prints {"ended": N}, the number of loads it ended: 0 where the
sandbox loads none.
`
}

// SetFlags implements subcommands.Command.SetFlags.
func (fl *FailLoad) SetFlags(f *flag.FlagSet) {
	f.StringVar(&fl.reason, "reason", "failed by runsc fail-load", "why the loading is ended, which the sandbox logs")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (fl *FailLoad) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	c, err := fl.loadContainer(conf, f, container.LoadOpts{ReadOnly: true})
	if err != nil {
		return "", nil, fmt.Errorf("loading container: %w", err)
	}
	return c.ID, c.Spec, nil
}

// Execute implements subcommands.Command.Execute.
func (fl *FailLoad) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() != 1 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	conf := args[0].(*config.Config)

	// Read only, as runsc wait reads it: a checkpoint of the container may
	// be waiting on the loading that this ends.
	c, err := fl.loadContainer(conf, f, container.LoadOpts{ReadOnly: true})
	if err != nil {
		util.Fatalf("loading container: %v", err)
	}
	ended, err := c.FailLoad(fl.reason)
	if err != nil {
		util.Fatalf("failing the loading of the pages: %v", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Ended int `json:"ended"`
	}{ended}); err != nil {
		util.Fatalf("writing the result: %v", err)
	}
	return subcommands.ExitSuccess
}
