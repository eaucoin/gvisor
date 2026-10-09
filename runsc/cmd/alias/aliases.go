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

// Package alias provides aliases for runsc commands.
package alias

import (
	"fmt"
	"os"
	"path/filepath"

	"gvisor.dev/gvisor/runsc/cmd/alias/bwrap"
	"gvisor.dev/gvisor/runsc/cmd/util"
)

// aliasType represents the type of alias.
type aliasType string

const (
	aliasBwrap  aliasType = "bwrap"
	aliasNsjail aliasType = "nsjail"

	// aliasCheckpointctl makes runsc a plugin of checkpointctl
	// (github.com/checkpoint-restore/checkpointctl), which runs
	// "checkpointctl-<name> ARGS..." from PATH for "checkpointctl <name>
	// ARGS...": the plugin runs "runsc image ARGS...".
	aliasCheckpointctl aliasType = "checkpointctl-runsc"
)

// checkpointctlPluginDescription describes runsc as a checkpointctl plugin.
const checkpointctlPluginDescription = "Inspect, verify and rewrite checkpoints of gVisor (runsc) containers"

// HandleAlias routes the command to the appropriate alias handler.
func HandleAlias() {
	base := filepath.Base(os.Args[0])
	switch aliasType(base) {
	case aliasBwrap:
		os.Args = append([]string{os.Args[0], string(aliasBwrap)}, os.Args[1:]...)
		return
	case aliasNsjail:
		panic("Nsjail alias not implemented")
	case aliasCheckpointctl:
		// checkpointctl asks each plugin for a one-line description by running
		// it with --plugin-description alone, and uses the answer only if the
		// plugin exits with status 42.
		if len(os.Args) == 2 && os.Args[1] == "--plugin-description" {
			fmt.Println(checkpointctlPluginDescription)
			os.Exit(42)
		}
		os.Args = append([]string{os.Args[0], "image"}, os.Args[1:]...)
	}
}

// Commands returns the map of `alias` subcommands.
func Commands() map[util.SubCommand]string {
	return map[util.SubCommand]string{
		new(bwrap.Cli): "",
	}
}
