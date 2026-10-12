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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/fifo"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/shim/v1/extension"
	"gvisor.dev/gvisor/pkg/shim/v1/proc"
	"gvisor.dev/gvisor/pkg/shim/v1/utils"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/runsc/specutils"
)

// containerSpecsKey is the key of the state file metadata under which runsc
// saves the specs of the containers that a checkpoint holds, by name
// (runsc/boot.ContainerSpecsKey; the shim does not link runsc/boot).
const containerSpecsKey = "container_specs"

// checkpointHolds reports whether the checkpoint in imagePath holds the
// container whose spec is in bundle.
//
// Only a named subcontainer can be absent from it: the root container's
// restore brings back the whole sandbox, and runsc pairs a container whose
// spec carries no name with one of the checkpoint's by creation order.
func checkpointHolds(imagePath, bundle string) (bool, error) {
	spec, err := utils.ReadSpec(bundle)
	if err != nil {
		return false, fmt.Errorf("read oci spec: %w", err)
	}
	name := specutils.ContainerName(spec)
	if specutils.IsRootContainer(spec) || name == "" {
		return true, nil
	}
	// The checkpoint is as trusted as the bundle that names it: runsc
	// restores from it.
	f, err := os.Open(filepath.Join(imagePath, checkpointfiles.StateFileName))
	if err != nil {
		return false, fmt.Errorf("checkpoint %q: %w", imagePath, err)
	}
	defer f.Close()
	metadata, err := statefile.MetadataUnsafe(f)
	if err != nil {
		return false, fmt.Errorf("checkpoint %q: read state file metadata: %w", imagePath, err)
	}
	checkpointed, err := specutils.GetSpecsFromString(metadata[containerSpecsKey])
	if err != nil {
		return false, fmt.Errorf("checkpoint %q: %w", imagePath, err)
	}
	_, ok := checkpointed[name]
	return ok, nil
}

// absentProcess is the init process of a container that the checkpoint it is
// created from does not hold: one that its sandbox no longer had when it was
// checkpointed, such as a Kubernetes init container that had run to
// completion, or one new to it. runsc has nothing to restore it to, and
// running it afresh in the restored sandbox would redo work whose result the
// sandbox already holds. So nothing runs: the process is created, and exits
// with status 0 as soon as it is started, as a container that did its work
// does, without runsc.
//
// It reports its exit on proc.ExitCh, as a runsc process does.
type absentProcess struct {
	id    string
	stdio stdio.Stdio

	// exited is closed once the process has exited.
	exited chan struct{}

	mu sync.Mutex

	// state is "created", "running" from its start or kill until its exit is
	// reported, then "stopped", and "deleted".
	//
	// +checklocks:mu
	state string

	// +checklocks:mu
	exitStatus int

	// +checklocks:mu
	exitedAt time.Time
}

var _ extension.Process = (*absentProcess)(nil)

func newAbsentProcess(id string, stdio stdio.Stdio) *absentProcess {
	return &absentProcess{
		id:     id,
		stdio:  stdio,
		exited: make(chan struct{}),
		state:  "created",
	}
}

// ID implements extension.Process.ID.
func (p *absentProcess) ID() string {
	return p.id
}

// Pid implements extension.Process.Pid. No process ever runs.
func (p *absentProcess) Pid() int {
	return 0
}

// ExitStatus implements extension.Process.ExitStatus.
func (p *absentProcess) ExitStatus() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitStatus
}

// ExitedAt implements extension.Process.ExitedAt.
func (p *absentProcess) ExitedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitedAt
}

// Stdin implements extension.Process.Stdin. Nothing reads it.
func (p *absentProcess) Stdin() io.Closer {
	return nil
}

// Stdio implements extension.Process.Stdio.
func (p *absentProcess) Stdio() stdio.Stdio {
	return p.stdio
}

// Status implements extension.Process.Status.
func (p *absentProcess) Status(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "deleted" {
		// As for a runsc process, there is no "deleted" status.
		return "stopped", nil
	}
	return p.state, nil
}

// Wait implements extension.Process.Wait.
func (p *absentProcess) Wait() {
	<-p.exited
}

// Resize implements extension.Process.Resize.
func (p *absentProcess) Resize(console.WinSize) error {
	return fmt.Errorf("container %s is not in its checkpoint, and has no console: %w", p.id, errdefs.ErrFailedPrecondition)
}

// Start implements extension.Process.Start. It ends the process's output, as
// a process that wrote none and exited does, and reports its exit.
func (p *absentProcess) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != "created" {
		return fmt.Errorf("cannot start a %s container: %w", p.state, errdefs.ErrFailedPrecondition)
	}
	for _, path := range []string{p.stdio.Stdout, p.stdio.Stderr} {
		if err := closeFifo(ctx, path); err != nil {
			return err
		}
	}
	p.exitLocked(0)
	return nil
}

// closeFifo opens the FIFO at path for writing, if path is one, and closes it,
// which its reader sees as the end of the stream.
func closeFifo(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		// A file or a logging binary that containerd names by URI, which
		// nothing reads from the shim.
		return nil
	}
	f, err := fifo.OpenFifo(ctx, path, unix.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	return f.Close()
}

// exitLocked makes the process exit with status, which it reports.
//
// +checklocks:p.mu
func (p *absentProcess) exitLocked(status int) {
	p.state = "running"
	go func() {
		proc.ExitCh <- proc.Exit{
			Timestamp: time.Now(),
			ID:        p.id,
			Status:    status,
		}
	}()
}

// Delete implements extension.Process.Delete.
func (p *absentProcess) Delete(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case "running":
		return fmt.Errorf("cannot delete a running container: %w", errdefs.ErrFailedPrecondition)
	case "created":
		// Never started, it never exits: its waiters are done waiting.
		close(p.exited)
	}
	p.state = "deleted"
	return nil
}

// Kill implements extension.Process.Kill. A created process exits killed by
// the signal; one that exited is handled as a stopped runsc process is.
func (p *absentProcess) Kill(_ context.Context, signal uint32, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case "created":
		if signal != 0 {
			p.exitLocked(128 + int(signal))
		}
		return nil
	case "running":
		// Its exit is on its way.
		return nil
	default:
		switch unix.Signal(signal) {
		case unix.SIGTERM, unix.SIGKILL:
			return nil
		default:
			return fmt.Errorf("process not found: %w", errdefs.ErrNotFound)
		}
	}
}

// SetExited implements extension.Process.SetExited.
func (p *absentProcess) SetExited(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != "running" {
		return
	}
	p.state = "stopped"
	p.exitStatus = status
	p.exitedAt = time.Now()
	close(p.exited)
}

// Restore implements extension.Process.Restore. There is nothing to restore.
func (p *absentProcess) Restore(context.Context, *extension.RestoreConfig) error {
	return fmt.Errorf("container %s is not in its checkpoint: %w", p.id, errdefs.ErrFailedPrecondition)
}
