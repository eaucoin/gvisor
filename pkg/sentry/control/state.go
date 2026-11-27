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
	"runtime"
	"runtime/debug"
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
	if o.LoadWait < 0 {
		return nil, fmt.Errorf("invalid load wait %v", o.LoadWait)
	}
	if o.LoadWait > 0 {
		saveOpts.LoadWaitDeadline = time.Now().Add(o.LoadWait)
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

	resume, err := SaveWithOpts(s.Kernel, s.Watchdog, saveOpts, &o.ExecOpts)
	if resume != nil {
		if perr := resume(); perr != nil {
			if err != nil {
				// The save's load wait ran out, or its sync failed, and the
				// sandbox runs on, but resuming it failed, as after a save
				// that succeeded: the save is not one to try again later.
				return fmt.Errorf("post resume work after a save that failed (%v): %w", err, perr)
			}
			return perr
		}
	}
	return err
}

// SaveWithOpts saves the running system, k, which w watches, with the given
// options. It is a function rather than a method of State, all of whose
// exported methods serve RPCs. If resume is not nil, the sandbox runs on, and
// the caller must call it afterwards: it undoes what of the save's preparation
// ran, as PostResume does, after a save with saveOpts.Resume set that
// succeeded, and after a save whose load wait expired once the save/restore
// binary had run (and, if the binary was the one waiting, before CUDA's
// checkpoint, which then has nothing to undo).
//
// If saveOpts.LoadWaitDeadline is not zero, it bounds the save's waits for
// pages that are still being loaded asynchronously (all of them, and the
// pages dirtied as they loaded for an incremental save): before the
// save/restore binary runs, the save waits, the sandbox running, for those it
// will read; the save/restore binary's wait for pages stalled past it is cut
// short, the binary killed (see waitWithin); pre-copy rounds, the sandbox
// running, and the save in the pause wait for the pages they copy or save
// until then. A save whose wait expires fails with pgalloc.ErrLoadWaitExpired
// before it writes the kernel's state, or, in the pause, any of its memory
// but what pre-copy rounds wrote, and leaves the sandbox running as it was,
// whether or not saveOpts.Resume is set: what it wrote to its destination
// before, such as the state file's header and metadata, is incomplete. What
// the deadline does not bound is the save/restore binary's own run time, which
// its timeout bounds, writing the save, and the pause: a task blocked in a
// system call on pages still loading, as a read of a tmpfs file's may be,
// stops only once they have loaded, which the wait before the binary runs
// leaves none of for a save that is not incremental.
//
// A save whose execOpts.Sync is set syncs the sandbox's filesystems once the
// save/restore socket has answered its save request; a sync that fails fails
// the save before it writes anything, the sandbox running on, resume undoing
// the request, as after a save whose load wait expired.
func SaveWithOpts(k *kernel.Kernel, w *watchdog.Watchdog, saveOpts *state.SaveOpts, execOpts *controlapi.SaveRestoreExecOpts) (resume func() error, err error) {
	ctx := k.SupervisorContext()
	if !saveOpts.LoadWaitDeadline.IsZero() && !saveOpts.TestOnlySkipRunningLoadWait {
		if err := k.AwaitSaveLoads(ctx, saveOpts.Parent != nil, saveOpts.LoadWaitDeadline); err != nil {
			return nil, err
		}
	}
	if err := preSave(k, saveOpts, execOpts); err != nil {
		if errors.Is(err, pgalloc.ErrLoadWaitExpired) || errors.Is(err, errSaveSyncFailed) {
			// A save/restore binary that waited for pages past the load
			// wait was killed, or the sync after the save request failed:
			// the sandbox runs on, to be resumed as after a save.
			return resumeAfterSaveRequest(k), err
		}
		return nil, err
	}
	// What the save allocates is garbage once it has ended; see
	// collectAfterSave.
	defer collectAfterSave()
	err = saveOpts.Save(ctx, k, w)
	if (err == nil && saveOpts.Resume) || errors.Is(err, pgalloc.ErrLoadWaitExpired) {
		return func() error { return PostResume(k, nil) }, err
	}
	return nil, err
}

// resumeAfterSaveRequest returns what undoes preSave once its save/restore
// binary was killed for waiting for pages past the save's load wait, or its
// sync of the sandbox's filesystems after the socket's save request failed:
// the binary's or socket's resume, and the TPU checkpoint before it, but not
// CUDA's, which runs after them and so never ran.
func resumeAfterSaveRequest(k *kernel.Kernel) func() error {
	return func() error { return postResume(k, nil, false /* cuda */) }
}

// collectAfterSave collects, in the background, the garbage of the save that
// has just ended, and returns to the host the memory it held.
//
// A save allocates in proportion to the state it writes, hundreds of
// megabytes for a sandbox of a few gigabytes, all of it garbage once the state
// is written. The Go runtime's background scavenger returns to the host the
// heap's free memory beyond what its heap goal says it will need, and that
// goal is set by the last collection, which ran during the save, while the
// save's allocations were live. Until the next collection, which a Sentry that
// allocates little may not run for a long time, the Sentry so keeps memory
// for a heap as large as the save's: a sandbox saved again and again as its
// memory grows keeps the largest save's. Even once a collection has set the
// goal anew, the scavenger, paced to a sliver of one CPU, returns hundreds of
// megabytes over minutes. FreeOSMemory collects, then returns the heap's free
// memory at once.
//
// It runs on a goroutine of its own, so that the save's caller does not wait
// for it: a sandbox left running runs on meanwhile, stopped only for the
// collection's brief stop-the-world phases, though what in it allocates waits
// for the heap while its free memory is returned. The log says how long both
// took.
func collectAfterSave() {
	go func() {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		debug.FreeOSMemory()
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		log.Infof("Collected the save's garbage and returned it to the host in %v, the world stopped for %v of it: heap in use %d -> %d bytes, held from the host %d -> %d bytes", took, time.Duration(after.PauseTotalNs-before.PauseTotalNs), before.HeapAlloc, after.HeapAlloc, before.HeapSys-before.HeapReleased, after.HeapSys-after.HeapReleased)
	}()
}

// preSave is called before saving the kernel.
func preSave(k *kernel.Kernel, o *state.SaveOpts, execOpts *controlapi.SaveRestoreExecOpts) error {
	if err := preSaveTPU(k); err != nil {
		return err
	}

	if execOpts.Argv != "" || execOpts.Socket != "" {
		if execOpts.Argv != "" && execOpts.Socket != "" {
			return fmt.Errorf("both a save/restore binary (%q) and a save/restore socket (%q) are given: give one", execOpts.Argv, execOpts.Socket)
		}
		var argv []string
		if execOpts.Argv != "" {
			argv = strings.Split(execOpts.Argv, " ")
		}
		if err := ConfigureSaveRestoreExec(k, argv, execOpts.Socket, execOpts.Timeout, execOpts.SaveTimeout, execOpts.ContainerID); err != nil {
			return fmt.Errorf("failed to configure save/restore binary: %w", err)
		}
		if execOpts.Sync && execOpts.Socket == "" {
			return fmt.Errorf("a save that syncs the sandbox's filesystems tells a save/restore socket so: give one, not a save/restore binary (%q)", execOpts.Argv)
		}
		// A save that leaves the sandbox running, or that syncs its
		// filesystems, says so to a socket.
		var told []string
		if o.Resume {
			told = append(told, saveRestoreLeaveRunning)
		}
		if execOpts.Sync {
			told = append(told, saveRestoreSyncs)
		}
		if err := saveRestoreExec(k, SaveRestoreExecSave, strings.Join(told, " "), o.LoadWaitDeadline); err != nil {
			return fmt.Errorf("failed to exec save/restore binary: %w", err)
		}
		if execOpts.Sync {
			start := time.Now()
			if err := syncSandboxFilesystems(k); err != nil {
				return fmt.Errorf("%w: %w", errSaveSyncFailed, err)
			}
			log.Infof("Synced the sandbox's filesystems after the save request in %v", time.Since(start))
		}
	}
	return preSaveCuda(k, o)
}

// errSaveSyncFailed is the error of a save whose sync of the sandbox's
// filesystems failed (controlapi.SaveRestoreExecOpts.Sync): the save/restore
// socket was asked the save, which the sandbox then runs on from.
var errSaveSyncFailed = errors.New("syncing the sandbox's filesystems after the save request failed")

// syncSandboxFilesystems syncs all of k's filesystems, as sync(2) does. It is
// a variable for tests.
var syncSandboxFilesystems = func(k *kernel.Kernel) error {
	return k.VFS().SyncAllFilesystems(k.SupervisorContext())
}

// PostResume is called after resuming the kernel.
//
// Precondition: The kernel should be running.
func PostResume(k *kernel.Kernel, timeline *timing.Timeline) error {
	return postResume(k, timeline, true /* cuda */)
}

// postResume is PostResume, which undoes CUDA's checkpoint only if cuda is
// true.
func postResume(k *kernel.Kernel, timeline *timing.Timeline, cuda bool) error {
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
	if cuda {
		if err := postResumeCuda(k, timeline); err != nil {
			return err
		}
	}

	if err := SaveRestoreExec(k, SaveRestoreExecResume); err != nil {
		return fmt.Errorf("failed to wait for save/restore binary: %w", err)
	}

	return postResumeTPU(k)
}

// PostRestore is called after restoring the kernel. reason, if not empty, is
// why the sandbox stopped before it was restored, which the restore mode's
// request tells a save/restore socket (see SaveRestoreRequest).
//
// Precondition: The kernel should be running.
func PostRestore(k *kernel.Kernel, timeline *timing.Timeline, reason string) error {
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

	if err := saveRestoreExec(k, SaveRestoreExecRestore, reason, time.Time{}); err != nil {
		return fmt.Errorf("failed to wait for save/restore binary: %w", err)
	}

	return postRestoreTPU(k)
}

// saveRestoreExecTick is how often waitWithin asks what async page loads are
// doing, and so how much later than its timeout it may tell that the timeout
// ran out. It is a variable for tests.
var saveRestoreExecTick = 100 * time.Millisecond

// waitOutcome is how a wait of waitWithin ended.
type waitOutcome int

const (
	// waitDone is a wait that ended as done was closed.
	waitDone waitOutcome = iota
	// waitTimedOut is a wait that ended as its timeout ran out.
	waitTimedOut
	// waitLoadWaitExpired is a wait that ended as async page loads stalled
	// past its load wait's deadline.
	waitLoadWaitExpired
)

// waitWithin waits for done to be closed for up to timeout, and returns how
// the wait ended. Where async page loads have pages left to load as it begins
// (progress says so), timeout counts none of the time in which progress finds
// them stalled: tasks waiting for pages while a pages file's reader retries
// reads that failed, as the reader of a store that is away for a while does.
// The save/restore binary that runs after a lazy restore touches pages that
// load after the restore began, so that the time it waits for a store to come
// back is not its own. Otherwise, as for a save, timeout counts all of the
// time, as it always did.
//
// If loadWaitDeadline is not zero, it bounds the time not counted: the first
// tick past it at which progress finds the loads stalled ends the wait
// (waitLoadWaitExpired). A save with a load wait so bounds its wait for the
// save/restore binary, which may touch pages that a store that is away keeps
// from loading, by its load wait, as it bounds its own waits for pages.
//
// progress may take long to answer, or never answer, as when it asks a pages
// file's reader in another process that is wedged: it is asked off the wait,
// once at a time, and a tick with no answer since the one before counts, so
// that timeout still bounds the wait.
func waitWithin(done <-chan struct{}, timeout time.Duration, loadWaitDeadline time.Time, progress func() pgalloc.AsyncPageLoadProgress) waitOutcome {
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
		return waitDone
	case p := <-answers:
		asking = false
		stalled = p.Stalled
		if !p.Loading {
			select {
			case <-done:
				return waitDone
			case <-time.After(timeout - time.Since(start)):
				return waitTimedOut
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
			return waitDone
		case p := <-answers:
			asking = false
			stalled = stalled || p.Stalled
		case now := <-ticker.C:
			if stalled && !loadWaitDeadline.IsZero() && now.After(loadWaitDeadline) {
				select {
				case <-done:
					return waitDone
				default:
					return waitLoadWaitExpired
				}
			}
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
		return waitDone
	default:
		return waitTimedOut
	}
}

// SaveRestoreExec creates a new process that executes the save/restore
// binary specified by k.SaveRestoreExecConfig and waits for it to finish, or,
// where the configuration names a socket, sends the mode's request to it and
// waits for its answer (see SaveRestoreRequest).
//
// Precondition: The kernel should be running; k.SetSaveRestoreExecConfig should
// be setup with an argv or a socket, otherwise this function is a no-op.
func SaveRestoreExec(k *kernel.Kernel, mode SaveRestoreExecMode) error {
	return saveRestoreExec(k, mode, "", time.Time{})
}

// saveRestoreExec is SaveRestoreExec, whose request to a socket carries
// detail after its mode, if detail is not empty, and whose wait for the
// binary or the socket loadWaitDeadline bounds if it is not zero (see
// waitWithin): once async page loads stall past it, the binary is killed, or
// the request abandoned, and saveRestoreExec fails with
// pgalloc.ErrLoadWaitExpired. The wait is bounded by the configuration's
// SaveTimeout for the save and resume modes, where it is not zero, and by its
// Timeout otherwise.
func saveRestoreExec(k *kernel.Kernel, mode SaveRestoreExecMode, detail string, loadWaitDeadline time.Time) error {
	config := k.SaveRestoreExecConfig
	if config == nil {
		return nil
	}
	timeout := config.Timeout
	if mode != SaveRestoreExecRestore && config.SaveTimeout != 0 {
		timeout = config.SaveTimeout
	}
	if config.Socket != "" {
		return saveRestoreRequest(k, config, mode, detail, timeout, loadWaitDeadline)
	}

	leader := config.LeaderTask
	argv := config.Argv
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
	switch waitWithin(waitC, timeout, loadWaitDeadline, pgalloc.CurrentAsyncPageLoadProgress) {
	case waitTimedOut:
		tg.SendForcedSignal(&linux.SignalInfo{Signo: int32(linux.SIGKILL)})
		return fmt.Errorf("%s timed out after %v", argv[0], timeout)
	case waitLoadWaitExpired:
		tg.SendForcedSignal(&linux.SignalInfo{Signo: int32(linux.SIGKILL)})
		return fmt.Errorf("%s waited for pages still loading past the save's load wait, killed: %w", argv[0], pgalloc.ErrLoadWaitExpired)
	}
	if tg.ExitStatus() != 0 {
		return fmt.Errorf("%v exited with non-zero status %d", argv[0], tg.ExitStatus())
	}
	log.Debugf("save/restore binary %s output: %s", argv[0], execOut.String())
	return nil
}

// ConfigureSaveRestoreExec sets the configuration for the save/restore binary,
// argv, or for the save/restore socket, socket, one of which is empty; see
// kernel.SaveRestoreExecConfig for the timeouts. If containerID is empty, the
// global init process will be used for the save/restore binary's leader task.
//
// A previously stored configuration is replaced: a sandbox may be
// checkpointed repeatedly, and the configuration also survives in the kernel
// state across a restore. The leader task is always resolved anew, since a
// task stored by an earlier call may have exited.
func ConfigureSaveRestoreExec(k *kernel.Kernel, argv []string, socket string, timeout, saveTimeout time.Duration, containerID string) error {
	leader, err := findContainerInitProcess(k, containerID)
	if err != nil {
		return err
	}
	k.SaveRestoreExecConfig = &kernel.SaveRestoreExecConfig{
		Argv:        argv,
		Socket:      socket,
		Timeout:     timeout,
		SaveTimeout: saveTimeout,
		LeaderTask:  leader,
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
