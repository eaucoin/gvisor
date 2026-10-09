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

package alias

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Run under the name of an alias, the test binary handles it as runsc
	// does, then prints the arguments that runsc would run with.
	if filepath.Base(os.Args[0]) == string(aliasCheckpointctl) {
		HandleAlias()
		fmt.Println(strings.Join(os.Args[1:], " "))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestCheckpointctlPlugin runs runsc as checkpointctl runs its plugins
// (checkpointctl's cmd/plugin.go).
func TestCheckpointctlPlugin(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(t.TempDir(), string(aliasCheckpointctl))
	if err := os.Symlink(exe, plugin); err != nil {
		t.Fatal(err)
	}

	// The description, which checkpointctl takes from the first line of the
	// output, with exit status 42.
	out, err := exec.Command(plugin, "--plugin-description").Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Errorf("%s --plugin-description: %v, want exit status 42", plugin, err)
	}
	if got := strings.TrimSpace(string(out)); got != checkpointctlPluginDescription {
		t.Errorf("%s --plugin-description printed %q, want %q", plugin, got, checkpointctlPluginDescription)
	}

	// "checkpointctl runsc ARGS..." runs "checkpointctl-runsc ARGS...", which
	// runs "runsc image ARGS...".
	out, err = exec.Command(plugin, "inspect", "--json", "checkpoint.tar").Output()
	if err != nil {
		t.Fatalf("%s inspect: %v", plugin, err)
	}
	if got, want := string(out), "image inspect --json checkpoint.tar\n"; got != want {
		t.Errorf("%s inspect runs runsc with %q, want %q", plugin, got, want)
	}
}
