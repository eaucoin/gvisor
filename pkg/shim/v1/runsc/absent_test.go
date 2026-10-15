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

package runsc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/shim/v1/proc"
	"gvisor.dev/gvisor/pkg/shim/v1/utils"
	"gvisor.dev/gvisor/runsc/specutils"
)

// fakeRunsc writes a runsc that "image inspect" finds the containers named in
// any image, and returns options that run it. It logs the arguments of each
// run to the returned file.
func fakeRunsc(t *testing.T, names ...string) (*Options, string) {
	t.Helper()
	dir := t.TempDir()
	containers, err := json.Marshal(names)
	if err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-runsc")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >>%q\necho '{\"containers\": %s}'\n", argsLog, containers)
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return &Options{BinaryName: script, Root: filepath.Join(dir, "root")}, argsLog
}

// writeBundle writes a bundle whose spec carries annotations, and returns it.
func writeBundle(t *testing.T, annotations map[string]string) string {
	t.Helper()
	bundle := t.TempDir()
	if err := utils.WriteSpec(bundle, &specs.Spec{Annotations: annotations}); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	return bundle
}

func TestCheckpointHolds(t *testing.T) {
	subcontainer := func(name string) map[string]string {
		annotations := map[string]string{
			specutils.ContainerdContainerTypeAnnotation: specutils.ContainerdContainerTypeContainer,
		}
		if name != "" {
			annotations[specutils.ContainerdContainerNameAnnotation] = name
		}
		return annotations
	}
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        bool
		// inspected is whether the shim asks runsc which containers the
		// checkpoint holds.
		inspected bool
	}{
		{
			name: "root container",
			annotations: map[string]string{
				specutils.ContainerdContainerTypeAnnotation: specutils.ContainerdContainerTypeSandbox,
				specutils.ContainerdContainerNameAnnotation: "renamed",
			},
			want: true,
		},
		{
			name:        "named subcontainer held",
			annotations: subcontainer("app"),
			want:        true,
			inspected:   true,
		},
		{
			name:        "named subcontainer absent",
			annotations: subcontainer("init"),
			want:        false,
			inspected:   true,
		},
		{
			name:        "unnamed subcontainer",
			annotations: subcontainer(""),
			want:        true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, argsLog := fakeRunsc(t, "sandbox", "app")
			got, err := checkpointHolds(t.Context(), "/ckpt", writeBundle(t, tc.annotations), "k8s.io", opts)
			if err != nil {
				t.Fatalf("checkpointHolds: %v", err)
			}
			if got != tc.want {
				t.Errorf("checkpointHolds = %t, want %t", got, tc.want)
			}
			args, err := os.ReadFile(argsLog)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if inspected := strings.HasSuffix(string(args), " image inspect --json /ckpt\n"); inspected != tc.inspected || strings.Count(string(args), "\n") > 1 {
				t.Errorf("runsc ran with %q, want one run of image inspect: %t", args, tc.inspected)
			}
		})
	}

	t.Run("runsc fails", func(t *testing.T) {
		opts := &Options{BinaryName: "/bin/false", Root: t.TempDir()}
		if _, err := checkpointHolds(t.Context(), "/ckpt", writeBundle(t, subcontainer("app")), "k8s.io", opts); err == nil {
			t.Errorf("checkpointHolds succeeded with a runsc that fails")
		}
	})
}

// nextExit returns the next exit reported on proc.ExitCh.
func nextExit(t *testing.T) proc.Exit {
	t.Helper()
	select {
	case e := <-proc.ExitCh:
		return e
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit reported")
		return proc.Exit{}
	}
}

// checkStatus checks the status of the process.
func checkStatus(t *testing.T, p *absentProcess, want string) {
	t.Helper()
	if got, err := p.Status(context.Background()); err != nil || got != want {
		t.Errorf("Status() = %q, %v, want %q", got, err, want)
	}
}

func TestAbsentProcessStart(t *testing.T) {
	ctx := context.Background()
	stdout := filepath.Join(t.TempDir(), "stdout")
	if err := unix.Mkfifo(stdout, 0600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	// A blocking open of the reading end returns once a writer opens it.
	readers := make(chan *os.File, 1)
	go func() {
		reader, err := os.OpenFile(stdout, os.O_RDONLY, 0)
		if err != nil {
			t.Errorf("open stdout: %v", err)
			return
		}
		readers <- reader
	}()

	p := newAbsentProcess("init", stdio.Stdio{Stdout: stdout})
	checkStatus(t, p, "created")
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if e := nextExit(t); e.ID != "init" || e.Status != 0 {
		t.Errorf("exit reported: got %+v, want init's with status 0", e)
	}
	select {
	case reader := <-readers:
		defer reader.Close()
		if out, err := io.ReadAll(reader); err != nil || len(out) != 0 {
			t.Errorf("stdout: got %q, %v, want it to end empty", out, err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("stdout was never opened for writing")
	}
	checkStatus(t, p, "running")
	if err := p.Start(ctx); !errors.Is(err, errdefs.ErrFailedPrecondition) {
		t.Errorf("second Start: got error %v, want %v", err, errdefs.ErrFailedPrecondition)
	}
	if err := p.Delete(ctx); !errors.Is(err, errdefs.ErrFailedPrecondition) {
		t.Errorf("Delete before the exit: got error %v, want %v", err, errdefs.ErrFailedPrecondition)
	}

	// The service sets the exit it hears of.
	p.SetExited(0)
	p.Wait()
	checkStatus(t, p, "stopped")
	if got := p.ExitStatus(); got != 0 {
		t.Errorf("ExitStatus() = %d, want 0", got)
	}
	if p.ExitedAt().IsZero() {
		t.Errorf("ExitedAt() is zero, want the time of the exit")
	}
	if err := p.Kill(ctx, uint32(unix.SIGKILL), false); err != nil {
		t.Errorf("Kill(SIGKILL) after the exit: %v", err)
	}
	if err := p.Kill(ctx, uint32(unix.SIGUSR1), false); !errors.Is(err, errdefs.ErrNotFound) {
		t.Errorf("Kill(SIGUSR1) after the exit: got error %v, want %v", err, errdefs.ErrNotFound)
	}
	if err := p.Delete(ctx); err != nil {
		t.Errorf("Delete: %v", err)
	}
	checkStatus(t, p, "stopped")
}

func TestAbsentProcessKill(t *testing.T) {
	ctx := context.Background()
	p := newAbsentProcess("init", stdio.Stdio{})
	if err := p.Kill(ctx, 0, false); err != nil {
		t.Fatalf("Kill(0): %v", err)
	}
	checkStatus(t, p, "created")
	if err := p.Kill(ctx, uint32(unix.SIGTERM), false); err != nil {
		t.Fatalf("Kill(SIGTERM): %v", err)
	}
	if e := nextExit(t); e.ID != "init" || e.Status != 128+int(unix.SIGTERM) {
		t.Errorf("exit reported: got %+v, want init's with status %d", e, 128+int(unix.SIGTERM))
	}
	p.SetExited(128 + int(unix.SIGTERM))
	p.Wait()
	if err := p.Start(ctx); !errors.Is(err, errdefs.ErrFailedPrecondition) {
		t.Errorf("Start after the exit: got error %v, want %v", err, errdefs.ErrFailedPrecondition)
	}
}

func TestAbsentProcessDeleteCreated(t *testing.T) {
	p := newAbsentProcess("init", stdio.Stdio{})
	if err := p.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Never started, it never exits: Wait returns all the same.
	p.Wait()
	checkStatus(t, p, "stopped")
}
