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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/summary"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/runsc/cmd/image"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/container"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// Checkpoint implements subcommands.Command for the "checkpoint" command.
type Checkpoint struct {
	containerLoader
	imagePath                 string
	leaveRunning              bool
	compression               CheckpointCompression
	excludeCommittedZeroPages bool
	cudaCheckpointPath        string
	cudaCheckpointSequential  bool
	saveRestoreExecArgv       string
	saveRestoreExecSocket     string
	saveRestoreExecTimeout    time.Duration
	saveRestoreSaveTimeout    time.Duration
	splitFSCheckpointPaths    pathVar
	stateEngine               optionalStateEnginePolicy
	parentImagePath           string
	precopy                   string
	precopyBudget             time.Duration
	precopyMaxRounds          int
	precopyThrottle           string
	loadWait                  time.Duration
	summaryFile               string

	// direct indicates whether O_DIRECT should be used for writing the
	// checkpoint pages file. It bypasses the kernel page cache. It is beneficial
	// if the checkpoint files are not expected to be read again on this host.
	// For example, if the checkpoint files will be stored on a network block
	// device, which will be detached after the checkpoint is done.
	direct bool
}

// Name implements subcommands.Command.Name.
func (*Checkpoint) Name() string {
	return "checkpoint"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*Checkpoint) Synopsis() string {
	return "checkpoint current state of container (experimental)"
}

// Usage implements subcommands.Command.Usage.
func (*Checkpoint) Usage() string {
	return "checkpoint [flags] <container id> - save current state of container.\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (c *Checkpoint) SetFlags(f *flag.FlagSet) {
	f.StringVar(&c.imagePath, "image-path", "", "directory path to saved container image")
	f.BoolVar(&c.leaveRunning, "leave-running", false, "restart the container after checkpointing")
	f.Var(newCheckpointCompressionValue(statefile.CompressionLevelDefault, &c.compression), "compression", "compress checkpoint image on disk. Values: none|flate-best-speed.")
	f.BoolVar(&c.excludeCommittedZeroPages, "exclude-committed-zero-pages", true, "exclude committed zero-filled pages from checkpoint")
	f.BoolVar(&c.direct, "direct", false, "use O_DIRECT for writing checkpoint pages file")
	f.StringVar(&c.cudaCheckpointPath, "cuda-checkpoint-path", "", "path to the cuda-checkpoint binary in the container")
	f.BoolVar(&c.cudaCheckpointSequential, "cuda-checkpoint-sequential", false, "run cuda-checkpoint sequentially in the container")
	f.StringVar(&c.saveRestoreExecArgv, "save-restore-exec-argv", "", "argv (split by spaces) for a save/restore binary that's automatically executed in the sandbox before saving and after restoring. If the execution fails, the save/restore process will fail.")
	f.StringVar(&c.saveRestoreExecSocket, "save-restore-exec-socket", "", "path, in the container, of a Unix stream socket that the sandbox sends a request to before saving and after restoring, in place of running a save/restore binary (not with --save-restore-exec-argv): one line, the mode (save, restore or resume), then, after a space, \"leave-running\" for a save that leaves the container running, or for a restore the reason the resume that runs it was given (runsc resume --save-restore-reason), and a newline. The listener answers \"ok\" and a newline, or \"ok\", a space, anything and a newline, then ends the connection; any other answer fails the save or restore.")
	f.DurationVar(&c.saveRestoreExecTimeout, "save-restore-exec-timeout", controlapi.DefaultSaveRestoreExecTimeout, "timeout for the binary pointed to by save-restore-exec-argv, or the request to save-restore-exec-socket.")
	f.DurationVar(&c.saveRestoreSaveTimeout, "save-restore-exec-save-timeout", 0, "if not 0, the timeout of the save/restore binary or request before saving and after a save that leaves the container running, --save-restore-exec-timeout then bounding the one after restoring alone, which may wait for memory that is still being loaded.")
	f.StringVar(&c.parentImagePath, "parent-image-path", "", "make the checkpoint incremental: write only the memory changed since the image at this path, which must be the image the container was last checkpointed to or restored from (with --dirty-tracking), and refer to it for the rest. Restoring the checkpoint then needs that image, found by --layer-path or in the checkpoint's layers/ directory.")
	f.StringVar(&c.precopy, "precopy", "off", "write memory while the container runs, in rounds over the memory written meanwhile, before pausing it to write the rest: off, on, or auto (on unless the write speed last measured, by a checkpoint or a round of this container, says that the pause would write memory within --precopy-budget anyway; the first round measures it). Requires --dirty-tracking and an uncompressed image; --direct is recommended.")
	f.DurationVar(&c.precopyBudget, "precopy-budget", 100*time.Millisecond, "with --precopy, stop the rounds when the memory written during the last one would take at most this long to write in the pause.")
	f.IntVar(&c.precopyMaxRounds, "precopy-max-rounds", 8, "with --precopy, the maximum number of rounds. Rounds also stop when one does not halve the memory left to write.")
	f.StringVar(&c.precopyThrottle, "precopy-throttle", "off", "with --precopy, when a round does not halve the memory left to write: off (default) stops the rounds; on limits instead, until the checkpoint completes, the rate at which the container's processes dirty memory, all together, to a quarter of the measured write speed, delaying only the processes that write, and goes on (requires --dirty-tracking=wp).")
	f.StringVar(&c.summaryFile, "summary-file", "", "once the checkpoint has succeeded, write to this path, atomically, a summary of it as one JSON object: the image (its digest, version, layers, memory files and files) and how long the parts of the save took. See pkg/sentry/state/summary for its keys.")
	f.DurationVar(&c.loadWait, "load-wait", 0, "with a container restored with --background, the longest that the checkpoint waits for the memory that is still being loaded, the memory it saves and the memory the save/restore binary waits for: past it, the checkpoint fails, its image incomplete, and the container runs on as it was, even without --leave-running, its memory loading on. It does not bound a pause held by a process blocked in a system call on such memory. 0 (default) waits for as long as loading takes.")
	// Must match the default prefix applied by sandbox.Sandbox.Checkpoint.
	c.splitFSCheckpointPaths.defaultPrefix = checkpointfiles.FSCheckpointDir
	f.Var(&c.splitFSCheckpointPaths, "fs-checkpoint-paths", `[<prefix>=][<container>:]<path> target to include in the filesystem checkpoint (can be repeated). <prefix> defaults to "fs". The files are written to --image-path as <prefix>_*, or into a <prefix>/ subdirectory if <prefix> ends with "/". Paths containing "=" require an explicit prefix. For capturing all of tmpfs, the path should be "all-tmpfs".`)
	f.Var(&c.stateEngine, "state-engine", "override the sandbox's boot-time state serialization engine for this checkpoint. Values: wire|graph.")

	// Unimplemented flags necessary for compatibility with docker.
	var wp string
	f.StringVar(&wp, "work-path", "", "ignored")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (c *Checkpoint) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	cont, err := c.loadContainer(conf, f, container.LoadOpts{ReadOnly: true})
	if err != nil {
		return "", nil, fmt.Errorf("loading container: %w", err)
	}
	return cont.ID, cont.Spec, nil
}

// Execute implements subcommands.Command.Execute.
func (c *Checkpoint) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() != 1 {
		f.Usage()
		return subcommands.ExitUsageError
	}

	conf := args[0].(*config.Config)

	cont, err := c.loadContainer(conf, f, container.LoadOpts{ReadOnly: true})
	if err != nil {
		util.Fatalf("loading container: %v", err)
	}

	if c.imagePath == "" {
		util.Fatalf("image-path flag must be provided")
	}
	if c.saveRestoreExecArgv != "" && c.saveRestoreExecSocket != "" {
		util.Fatalf("save-restore-exec-argv and save-restore-exec-socket cannot both be given")
	}

	if err := os.MkdirAll(c.imagePath, 0755); err != nil {
		util.Fatalf("making directories at path provided: %v", err)
	}

	opts := sandbox.CheckpointOpts{
		Compression:                c.compression.Level(),
		Resume:                     c.leaveRunning,
		Direct:                     c.direct,
		ExcludeCommittedZeroPages:  c.excludeCommittedZeroPages,
		CudaCheckpointPath:         c.cudaCheckpointPath,
		CudaCheckpointSequential:   c.cudaCheckpointSequential,
		SaveRestoreExecArgv:        c.saveRestoreExecArgv,
		SaveRestoreExecSocket:      c.saveRestoreExecSocket,
		SaveRestoreExecTimeout:     c.saveRestoreExecTimeout,
		SaveRestoreExecSaveTimeout: c.saveRestoreSaveTimeout,
		SaveRestoreExecContainerID: cont.ID,
		SplitFSCheckpointPaths:     c.splitFSCheckpointPaths.targets,
		StateEngine:                c.stateEngine.ptr(),
		ParentImagePath:            c.parentImagePath,
		PrecopyBudget:              c.precopyBudget,
		PrecopyMaxRounds:           c.precopyMaxRounds,
		LoadWait:                   c.loadWait,
	}
	switch c.precopy {
	case "off":
	case "on", "auto":
		opts.Precopy = c.precopy
	default:
		util.Fatalf("invalid --precopy %q: want off, on or auto", c.precopy)
	}
	switch c.precopyThrottle {
	case "off":
	case "on":
		opts.PrecopyThrottle = true
	default:
		util.Fatalf("invalid --precopy-throttle %q: want off or on", c.precopyThrottle)
	}

	if c.summaryFile != "" {
		opts.Summary = &summary.Checkpoint{}
	}

	if err := cont.Checkpoint(conf, c.imagePath, opts); err != nil {
		util.Fatalf("checkpoint failed: %v", err)
	}

	if c.summaryFile != "" {
		if err := writeCheckpointSummary(conf, c.summaryFile, c.imagePath, opts.Summary); err != nil {
			util.Fatalf("checkpoint succeeded, but writing its summary failed: %v", err)
		}
	}

	return subcommands.ExitSuccess
}

// writeCheckpointSummary completes sum, the summary of a checkpoint to
// imagePath, with the description of the image, and writes it to path
// atomically.
func writeCheckpointSummary(conf *config.Config, path, imagePath string, sum *summary.Checkpoint) error {
	img, err := image.SummaryImage(conf, imagePath)
	if err != nil {
		return fmt.Errorf("describing the image: %w", err)
	}
	sum.Image = img
	b, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	return writeFileAtomically(path, append(b, '\n'))
}

// writeFileAtomically writes data to a temporary file beside path, then
// renames it to path, so that path never holds part of data.
func writeFileAtomically(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cu := cleanup.Make(func() {
		f.Close()
		os.Remove(tmp)
	})
	defer cu.Clean()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(0644); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cu.Release()
	return nil
}

// CheckpointCompression represents checkpoint image writer behavior. The
// default behavior is to compress because the default behavior used to be to
// always compress.
type CheckpointCompression statefile.CompressionLevel

func newCheckpointCompressionValue(val statefile.CompressionLevel, p *CheckpointCompression) *CheckpointCompression {
	*p = CheckpointCompression(val)
	return (*CheckpointCompression)(p)
}

// Set implements flag.Value.
func (g *CheckpointCompression) Set(v string) error {
	t, err := statefile.CompressionLevelFromString(v)
	if err != nil {
		return fmt.Errorf("invalid checkpoint compression type %q", v)
	}

	*g = CheckpointCompression(t)

	return nil
}

// Get implements flag.Getter.
func (g *CheckpointCompression) Get() any {
	return *g
}

// String implements flag.Value.
func (g CheckpointCompression) String() string {
	return string(g)
}

// Level returns corresponding statefile.CompressionLevel value.
func (g CheckpointCompression) Level() statefile.CompressionLevel {
	return statefile.CompressionLevel(g)
}

type optionalStateEnginePolicy struct {
	set   bool
	value config.StateEnginePolicy
}

// String implements flag.Value.
func (o *optionalStateEnginePolicy) String() string {
	if !o.set {
		return ""
	}
	return o.value.String()
}

// Set implements flag.Value.
func (o *optionalStateEnginePolicy) Set(v string) error {
	if err := o.value.Set(v); err != nil {
		return err
	}
	o.set = true
	return nil
}

// Get implements flag.Getter.
func (o *optionalStateEnginePolicy) Get() any {
	if !o.set {
		return nil
	}
	return o.value
}

func (o *optionalStateEnginePolicy) ptr() *config.StateEnginePolicy {
	if !o.set {
		return nil
	}
	v := o.value
	return &v
}
