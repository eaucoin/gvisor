// Copyright 2018 The gVisor Authors.
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

package control

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/fdcollector"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/pipefs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/timing"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
)

// SaveRestoreExecMode is the mode for the save/restore binary.
type SaveRestoreExecMode string

const (
	// SaveRestoreExecSave is the save mode for the save/restore exec.
	SaveRestoreExecSave SaveRestoreExecMode = "save"
	// SaveRestoreExecRestore is the restore mode for the save/restore exec.
	SaveRestoreExecRestore SaveRestoreExecMode = "restore"
	// SaveRestoreExecResume is the resume mode for the save/restore binary.
	SaveRestoreExecResume SaveRestoreExecMode = "resume"

	saveRestoreExecEnvVar = "GVISOR_SAVE_RESTORE_AUTO_EXEC_MODE"
)

// ErrInvalidFiles is returned when the urpc call to Save does not include an
// appropriate file payload (e.g. there is no output file!).
var ErrInvalidFiles = errors.New("exactly one file must be provided")

// State includes state-related functions.
type State struct {
	Kernel   *kernel.Kernel
	Watchdog *watchdog.Watchdog
}

// ConvertToStateSaveOpts converts a control.SaveOpts to a state.SaveOpts.
// state.SaveOpts.Close() must be called when the state.SaveOpts is no longer
// needed.
func ConvertToStateSaveOpts(o *controlapi.SaveOpts) (*state.SaveOpts, error) {
	saveOpts := &state.SaveOpts{
		Key:                            o.Key,
		Metadata:                       o.Metadata,
		AppMFExcludeCommittedZeroPages: o.AppMFExcludeCommittedZeroPages,
		Resume:                         o.Resume,
		CudaCheckpointPath:             o.CudaCheckpointPath,
		CudaCheckpointSequential:       o.CudaCheckpointSequential,
		ValidateMetadata:               o.ValidateMetadata,
		GraphEngine:                    o.GraphEngine,
	}
	if o.ParentImageDigest != "" {
		d, err := checkpointimage.ParseDigest(o.ParentImageDigest)
		if err != nil {
			return nil, fmt.Errorf("parent image digest: %w", err)
		}
		saveOpts.Parent = &d
	}
	switch o.Precopy {
	case "":
	case "on", "auto":
		if o.PrecopyBudget <= 0 || o.PrecopyMaxRounds <= 0 {
			return nil, fmt.Errorf("pre-copy requires a positive budget and round cap, got %v and %d", o.PrecopyBudget, o.PrecopyMaxRounds)
		}
		saveOpts.Precopy = &kernel.PrecopyOpts{
			Budget:    o.PrecopyBudget,
			MaxRounds: o.PrecopyMaxRounds,
			Auto:      o.Precopy == "auto",
			Throttle:  o.PrecopyThrottle,
		}
	default:
		return nil, fmt.Errorf("invalid pre-copy mode %q", o.Precopy)
	}
	if err := setSaveOpts(o, saveOpts); err != nil {
		saveOpts.Close()
		return nil, err
	}
	return saveOpts, nil
}

func setSaveOpts(o *controlapi.SaveOpts, saveOpts *state.SaveOpts) error {
	// TODO(b/541219576): Support checkpoint gofer with split checkpoint.
	if len(o.SplitFSCheckpointPaths) > 0 && o.UseCheckpointGofer {
		return fmt.Errorf("split filesystem checkpoint is not supported with checkpoint gofer")
	}
	if o.UseCheckpointGofer {
		return setSaveOptsForCheckpointGofer(o, saveOpts)
	}
	return setSaveOptsForLocalCheckpointFiles(o, saveOpts)
}

func setSaveOptsForLocalCheckpointFiles(o *controlapi.SaveOpts, saveOpts *state.SaveOpts) error {
	wantFiles := 1
	if o.HavePagesFile {
		wantFiles += 2
	}
	fsFilesStart := wantFiles
	// Each filesystem checkpoint bundle comprises 4 files:
	// manifest, multi-tar archive, pages metadata, and pages file.
	wantFiles += 4 * len(o.SplitFSCheckpointPaths)
	if gotFiles := len(o.Files); gotFiles != wantFiles {
		return fmt.Errorf("got %d files, wanted %d", gotFiles, wantFiles)
	}

	// Save to the first provided stream.
	stateFile, err := o.ReleaseFD(0)
	if err != nil {
		return err
	}
	// Setting saveOpts.Destination/PagesMetadata/PagesFile transfers ownership
	// of the created object to saveOpts, even if we return a non-nil error.
	saveOpts.Destination = stateFile
	if o.HavePagesFile {
		pagesMetadataFile, err := o.ReleaseFD(1)
		if err != nil {
			return err
		}
		// //pkg/state/wire writes one byte at a time; buffer writes to
		// pagesMetadataFile to avoid making one syscall per write. For the
		// state file, this buffering is handled by statefile.NewWriter() =>
		// compressio.Writer or compressio.NewSimpleWriter().
		saveOpts.PagesMetadata = stateio.NewBufioWriteCloser(pagesMetadataFile)

		pagesFileFD, err := unix.Dup(int(o.Files[2].Fd()))
		if err != nil {
			return err
		}
		saveOpts.PagesFile = stateio.NewPagesFileFDWriterDefault(int32(pagesFileFD))
	}

	if len(o.SplitFSCheckpointPaths) > 0 {
		saveOpts.FSSaveOpts = &kernel.FSSaveOpts{
			RunscVersion: o.RunscVersion,
			Bundles:      make([]kernel.FSSaveBundle, len(o.SplitFSCheckpointPaths)),
		}
		for i := range o.SplitFSCheckpointPaths {
			offset := fsFilesStart + 4*i
			bundle := &saveOpts.FSSaveOpts.Bundles[i]
			bundle.Paths = o.SplitFSCheckpointPaths[i].Paths
			manifestFile, err := o.ReleaseFD(offset)
			if err != nil {
				return err
			}
			bundle.ManifestFile = stateio.NewBufioWriteCloser(manifestFile)
			multiTarFile, err := o.ReleaseFD(offset + 1)
			if err != nil {
				return err
			}
			bundle.MultiTarFile = stateio.NewBufioWriteCloser(multiTarFile)
			pagesMetadataFile, err := o.ReleaseFD(offset + 2)
			if err != nil {
				return err
			}
			bundle.PagesMetadataFile = stateio.NewBufioWriteCloser(pagesMetadataFile)
			pagesFileFD, err := unix.Dup(int(o.Files[offset+3].Fd()))
			if err != nil {
				return err
			}
			bundle.PagesFile = stateio.NewPagesFileFDWriterDefault(int32(pagesFileFD))
		}
	}
	return nil
}

func setSaveOptsForCheckpointGofer(o *controlapi.SaveOpts, saveOpts *state.SaveOpts) error {
	if gotFiles := len(o.Files); gotFiles != 1 {
		return fmt.Errorf("got %d files, wanted 1", gotFiles)
	}
	clientFD, err := unix.Dup(int(o.Files[0].Fd()))
	if err != nil {
		return fmt.Errorf("failed to dup checkpoint gofer client FD: %w", err)
	}
	clientSock, err := unet.NewSocket(clientFD)
	if err != nil {
		unix.Close(clientFD)
		return fmt.Errorf("failed to create unet.Socket for checkpoint gofer client FD: %w", err)
	}
	afc, err := stateipc.NewAsyncFileClient(urpc.NewClient(clientSock))
	if err != nil {
		return fmt.Errorf("failed to create stateipc client: %w", err)
	}
	defer afc.DecRef()

	stateFileAsync, err := afc.OpenWrite(checkpointfiles.StateFileName)
	if err != nil {
		return fmt.Errorf("failed to open state file: %w", err)
	}
	// Setting saveOpts.Destination/PagesMetadata/PagesFile transfers ownership
	// of the created object to saveOpts, even if we return a non-nil error.
	saveOpts.Destination, err = stateio.NewBufWriter(stateFileAsync /* transfers ownership */, 8<<20 /* size = 8 MiB */)
	if err != nil {
		return fmt.Errorf("failed to buffer state file: %w", err)
	}
	if o.HavePagesFile {
		pagesMetadataAsync, err := afc.OpenWrite(checkpointfiles.PagesMetadataFileName)
		if err != nil {
			return fmt.Errorf("failed to open pages metadata file: %w", err)
		}
		saveOpts.PagesMetadata, err = stateio.NewBufWriter(pagesMetadataAsync /* transfers ownership */, 8<<20 /* size = 8 MiB */)
		if err != nil {
			return fmt.Errorf("failed to buffer pages metadata file: %w", err)
		}
		saveOpts.PagesFile, err = afc.OpenWrite(checkpointfiles.PagesFileName)
		if err != nil {
			return fmt.Errorf("failed to open pages file: %w", err)
		}
	}
	return nil
}

// Save saves the running system.
func (s *State) Save(o *controlapi.SaveOpts, _ *struct{}) (err error) {
	if len(o.SplitFSCheckpointPaths) > 0 {
		defer func() {
			s.Kernel.SignalAllFSSaveWaiters(err)
		}()
	}

	saveOpts, err := ConvertToStateSaveOpts(o)
	if err != nil {
		return err
	}
	defer saveOpts.Close()

	if err := s.SaveWithOpts(saveOpts, &o.ExecOpts); err != nil {
		return err
	}
	if saveOpts.Resume {
		return PostResume(s.Kernel, nil)
	}
	return nil
}

// SaveWithOpts saves the running system with the given options. If
// saveOpts.Resume is true, the caller must call PostResume afterwards.
func (s *State) SaveWithOpts(saveOpts *state.SaveOpts, execOpts *controlapi.SaveRestoreExecOpts) error {
	if err := preSave(s.Kernel, saveOpts, execOpts); err != nil {
		return err
	}
	return saveOpts.Save(s.Kernel.SupervisorContext(), s.Kernel, s.Watchdog)
}

// preSave is called before saving the kernel.
func preSave(k *kernel.Kernel, o *state.SaveOpts, execOpts *controlapi.SaveRestoreExecOpts) error {
	if err := preSaveTPU(k); err != nil {
		return err
	}

	if execOpts.Argv != "" {
		argv := strings.Split(execOpts.Argv, " ")
		if err := ConfigureSaveRestoreExec(k, argv, execOpts.Timeout, execOpts.ContainerID); err != nil {
			return fmt.Errorf("failed to configure save/restore binary: %w", err)
		}
		if err := SaveRestoreExec(k, SaveRestoreExecSave); err != nil {
			return fmt.Errorf("failed to exec save/restore binary: %w", err)
		}
	}
	return preSaveCuda(k, o)
}

// PostResume is called after resuming the kernel.
//
// Precondition: The kernel should be running.
func PostResume(k *kernel.Kernel, timeline *timing.Timeline) error {
	if k.IsPaused() {
		// The kernel is still paused (double-pause can happen with Docker which
		// calls pause first and then checkpoint command). The final resume command
		// will invoke save/restore binary if necessary.
		return nil
	}
	if k.TaskSet().IsExiting() {
		// This can occur when kernel is saved with control.SaveOpts.Resume=false.
		// We can not invoke the save/restore binary on such a kernel.
		return nil
	}
	// Undo preSave in reverse order.
	if err := postResumeCuda(k, timeline); err != nil {
		return err
	}

	if err := SaveRestoreExec(k, SaveRestoreExecResume); err != nil {
		return fmt.Errorf("failed to wait for save/restore binary: %w", err)
	}

	return postResumeTPU(k)
}

// PostRestore is called after restoring the kernel.
//
// Precondition: The kernel should be running.
func PostRestore(k *kernel.Kernel, timeline *timing.Timeline) error {
	if k.IsPaused() {
		// The kernel is still paused (double-pause can happen with Docker which
		// calls pause first and then checkpoint command). The final resume command
		// will invoke cuda-checkpoint if necessary.
		return nil
	}
	if k.TaskSet().IsExiting() {
		// This can occur when kernel is saved with control.SaveOpts.Resume=false.
		// We can not invoke cuda-checkpoint on such a kernel.
		return nil
	}
	// Undo preSave in reverse order.
	if err := postRestoreCuda(k, timeline); err != nil {
		return err
	}

	if err := SaveRestoreExec(k, SaveRestoreExecRestore); err != nil {
		return fmt.Errorf("failed to wait for save/restore binary: %w", err)
	}

	return postRestoreTPU(k)
}

// saveRestoreExecTick is how often waitWithin asks what async page loads are
// doing, and so how much later than its timeout it may tell that the timeout
// ran out. It is a variable for tests.
var saveRestoreExecTick = 100 * time.Millisecond

// waitWithin waits for done to be closed for up to timeout, and returns true
// if it was. Where async page loads have pages left to load as it begins
// (progress says so), timeout counts none of the time in which progress finds
// them stalled: tasks waiting for pages while a pages file's reader retries
// reads that failed, as the reader of a store that is away for a while does.
// The save/restore binary that runs after a lazy restore touches pages that
// load after the restore began, so that the time it waits for a store to come
// back is not its own. Otherwise, as for a save, timeout counts all of the
// time, as it always did.
//
// progress may take long to answer, or never answer, as when it asks a pages
// file's reader in another process that is wedged: it is asked off the wait,
// once at a time, and a tick with no answer since the one before counts, so
// that timeout still bounds the wait.
func waitWithin(done <-chan struct{}, timeout time.Duration, progress func() pgalloc.AsyncPageLoadProgress) bool {
	start := time.Now()
	answers := make(chan pgalloc.AsyncPageLoadProgress, 1)
	asking := false
	ask := func() {
		asking = true
		go func() { answers <- progress() }()
	}
	ask()
	// stalled is true if an answer since the last tick, or since start, said
	// the loads stall.
	stalled := false
	select {
	case <-done:
		return true
	case p := <-answers:
		asking = false
		stalled = p.Stalled
		if !p.Loading {
			select {
			case <-done:
				return true
			case <-time.After(timeout - time.Since(start)):
				return false
			}
		}
	case <-time.After(saveRestoreExecTick):
		// No answer yet: loads may be stalled behind a reader that does not
		// answer, which the ticks below count against timeout.
	}
	ticker := time.NewTicker(saveRestoreExecTick)
	defer ticker.Stop()
	last := start
	for counted := time.Duration(0); counted < timeout; {
		select {
		case <-done:
			return true
		case p := <-answers:
			asking = false
			stalled = stalled || p.Stalled
		case now := <-ticker.C:
			if !stalled {
				counted += now.Sub(last)
			}
			stalled = false
			last = now
			if !asking {
				ask()
			}
		}
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// SaveRestoreExec creates a new process that executes the save/restore
// binary specified by k.SaveRestoreExecConfig and waits for it to finish.
//
// Precondition: The kernel should be running; k.SetSaveRestoreExecConfig should
// be setup with an argv, otherwise this function is a no-op.
func SaveRestoreExec(k *kernel.Kernel, mode SaveRestoreExecMode) error {
	if k.SaveRestoreExecConfig == nil {
		return nil
	}

	leader := k.SaveRestoreExecConfig.LeaderTask
	argv := k.SaveRestoreExecConfig.Argv
	timeout := k.SaveRestoreExecConfig.Timeout
	sctx := k.SupervisorContext()
	contID := leader.ContainerID()
	mntns := leader.MountNamespace()
	if mntns == nil || !mntns.TryIncRef() {
		log.Warningf("PID %d in container %q has exited, skipping save/restore exec for it", leader.ThreadGroup().ID(), contID)
		return nil
	}
	root := mntns.Root(sctx)
	cu := cleanup.Make(func() {
		root.DecRef(sctx)
	})
	defer cu.Clean()
	ctx := vfs.WithRoot(sctx, root)
	cu.Add(func() {
		mntns.DecRef(ctx)
	})

	fdTable := k.NewFDTable()
	cu.Add(func() {
		fdTable.DecRef(sctx)
	})
	var execOut *fdcollector.Agent
	rfd, wfd, err := pipefs.NewConnectedPipeFDs(ctx, k.PipeMount(), 0 /* flags */)
	if err != nil {
		log.Warningf("Failed to create stdout/stderr pipe for %s: %v", argv[0], err)
	} else {
		if _, err := fdTable.NewFDAt(ctx, 1, wfd, kernel.FDFlags{}); err != nil {
			log.Warningf("Failed to make pipe stdout for %s: %v", argv[0], err)
		}
		if _, err := fdTable.NewFDAt(ctx, 2, wfd, kernel.FDFlags{}); err != nil {
			log.Warningf("Failed to make pipe stderr for %s: %v", argv[0], err)
		}
		wfd.DecRef(ctx)
		execOut = fdcollector.NewAgent(ctx, rfd, argv[0]) // transfers ownership of rfd
		cu.Add(execOut.Stop)
	}
	envv := k.SpecEnviron(contID)

	proc := Proc{
		Kernel: k,
	}
	execArgs := ExecArgs{
		ExecArgs: controlapi.ExecArgs{
			Filename:    argv[0],
			Argv:        argv,
			Envv:        append(envv, fmt.Sprintf("%s=%s", saveRestoreExecEnvVar, mode)),
			ContainerID: contID,
		},
		MountNamespace: mntns,
		PIDNamespace:   leader.PIDNamespace(),
		Limits:         limits.NewLimitSet(),
		FDTable:        fdTable,
	}
	tg, _, _, err := ExecAsync(&proc, &execArgs)
	if err != nil {
		return fmt.Errorf("failed to exec save/restore binary: %w", err)
	}

	waitC := make(chan struct{})
	go func() {
		tg.WaitExited()
		close(waitC)
	}()
	if !waitWithin(waitC, timeout, pgalloc.CurrentAsyncPageLoadProgress) {
		tg.SendForcedSignal(&linux.SignalInfo{Signo: int32(linux.SIGKILL)})
		return fmt.Errorf("%s timed out after %v", argv[0], timeout)
	}
	if tg.ExitStatus() != 0 {
		return fmt.Errorf("%v exited with non-zero status %d", argv[0], tg.ExitStatus())
	}
	log.Debugf("save/restore binary %s output: %s", argv[0], execOut.String())
	return nil
}

// ConfigureSaveRestoreExec sets the configuration for the save/restore binary.
// If containerID is empty, the global init process will be used for the
// save/restore binary's leader task.
//
// A previously stored configuration is replaced: a sandbox may be
// checkpointed repeatedly, and the configuration also survives in the kernel
// state across a restore. The leader task is always resolved anew, since a
// task stored by an earlier call may have exited.
func ConfigureSaveRestoreExec(k *kernel.Kernel, argv []string, timeout time.Duration, containerID string) error {
	leader, err := findContainerInitProcess(k, containerID)
	if err != nil {
		return err
	}
	k.SaveRestoreExecConfig = &kernel.SaveRestoreExecConfig{
		Argv:       argv,
		Timeout:    timeout,
		LeaderTask: leader,
	}
	return nil
}

// findContainerInitProcess finds the init process (root of execution) associated with
// the given containerID. If containerID is empty, the global init process is returned.
func findContainerInitProcess(k *kernel.Kernel, containerID string) (*kernel.Task, error) {
	if containerID == "" {
		init := k.GlobalInit()
		if init == nil {
			return nil, fmt.Errorf("sandbox has no global init process")
		}
		return init.Leader(), nil
	}
	for _, tg := range k.RootPIDNamespace().ThreadGroups() {
		if leader := tg.Leader(); tg.IsContainerInit() && leader.ContainerID() == containerID {
			return leader, nil
		}
	}
	return nil, fmt.Errorf("failed to find process associated with container %s", containerID)
}
