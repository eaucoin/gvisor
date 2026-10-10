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

package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
	"gvisor.dev/gvisor/test/metricclient"
)

// abwriter runs test/cmd/precopy/abwriter, experiment 06's A-B writer, in a
// container that a test checkpoints, restores, and asks to check its memory.
type abwriter struct {
	t    *testing.T
	conf *config.Config
	spec *specs.Spec

	// bundleDir is the container's bundle.
	bundleDir string

	// dir holds the images and what abwriter reports.
	dir string

	// cont is the container that runs abwriter.
	cont *Container

	// out holds what abwriter reports.
	out reports

	// images is the number of images taken.
	images int
}

// startABWriter runs abwriter with args (its flags, the size of its buffer in
// MiB, and the rate at which it rewrites it in MiB/s) in a container with
// conf, whose root directory is set, and waits for it to be ready.
func startABWriter(t *testing.T, conf *config.Config, args ...string) *abwriter {
	t.Helper()
	dir, err := os.MkdirTemp(testutil.TmpDir(), "abwriter")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	app, err := testutil.FindFile("test/cmd/precopy/abwriter")
	if err != nil {
		t.Fatalf("finding abwriter: %v", err)
	}
	spec := testutil.NewSpecWithArgs(append(append([]string{app}, args...), dir)...)
	if overlay := conf.GetOverlay2(); overlay.Enabled() {
		// The overlay keeps the root filesystem's writes.
		spec.Root.Readonly = false
	}
	bundleDir, cleanup, err := testutil.SetupBundleDir(spec)
	if err != nil {
		t.Fatalf("error setting up bundle: %v", err)
	}
	t.Cleanup(cleanup)
	w := &abwriter{
		t:         t,
		conf:      conf,
		spec:      spec,
		bundleDir: bundleDir,
		dir:       dir,
		out:       reports{path: filepath.Join(dir, "out")},
	}
	w.cont = w.newContainer()
	if err := w.cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if got, err := w.out.next(w.cont); err != nil || got != "READY" {
		t.Fatalf("abwriter reported %q (%v), want READY", got, err)
	}
	return w
}

// newContainer creates a container for abwriter.
func (w *abwriter) newContainer() *Container {
	w.t.Helper()
	cont, err := New(w.conf, Args{ID: testutil.RandomContainerID(), Spec: w.spec, BundleDir: w.bundleDir})
	if err != nil {
		w.t.Fatalf("error creating container: %v", err)
	}
	w.t.Cleanup(func() { cont.Destroy() })
	return cont
}

// checkpoint checkpoints the container, which keeps running, to a new image
// directory with opts, and returns the directory.
func (w *abwriter) checkpoint(opts sandbox.CheckpointOpts) (string, error) {
	w.t.Helper()
	w.images++
	image := filepath.Join(w.dir, fmt.Sprint("image-", w.images))
	if err := os.Mkdir(image, 0755); err != nil {
		w.t.Fatal(err)
	}
	opts.Compression = statefile.CompressionLevelNone
	opts.Resume = true
	return image, w.cont.Checkpoint(w.conf, image, opts)
}

// restore replaces the container with one restored from image, whose layers
// other than itself are in the directories layers.
func (w *abwriter) restore(image string, layers []string) {
	w.t.Helper()
	w.cont.Destroy()
	w.cont = w.newContainer()
	if err := w.cont.Restore(w.conf, image, layers, false /* direct */, false /* background */, nil /* networkArgs */); err != nil {
		w.t.Fatalf("error restoring %s: %v", image, err)
	}
}

// check asks abwriter to check its memory, and returns its result.
func (w *abwriter) check() string {
	w.t.Helper()
	if err := w.cont.SignalContainer(unix.SIGUSR1, false /* all */); err != nil {
		w.t.Fatalf("error signaling abwriter: %v", err)
	}
	got, err := w.out.next(w.cont)
	if err != nil {
		w.t.Fatalf("abwriter did not check its memory: %v", err)
	}
	return got
}

// precopyOpts returns the options of a checkpoint that pre-copies memory in
// mode ("on" or "auto"), with the defaults of runsc checkpoint.
func precopyOpts(mode string) sandbox.CheckpointOpts {
	return sandbox.CheckpointOpts{
		Precopy:          mode,
		PrecopyBudget:    100 * time.Millisecond,
		PrecopyMaxRounds: 8,
	}
}

// precopyConf returns a copy of conf whose sandboxes track dirty pages with
// write-protection, with verification, and write checkpoints at writeRate
// bytes per second if it is not 0.
func precopyConf(conf *config.Config, writeRate uint64) *config.Config {
	c := *conf
	c.DirtyTracking = config.DirtyTrackingWriteProtect
	c.DirtyTrackingVerify = config.DirtyTrackingVerifyHash
	c.TestOnlyCheckpointWriteRate = writeRate
	return &c
}

// withRootDir sets the root directory of conf to a new one.
func withRootDir(t *testing.T, conf *config.Config) *config.Config {
	t.Helper()
	rootDir, cleanup, err := testutil.SetupRootDir()
	if err != nil {
		t.Fatalf("error setting up root directory: %v", err)
	}
	t.Cleanup(cleanup)
	conf.RootDir = rootDir
	return conf
}

// metric returns the value of the metric name, as the metric server exports
// it without te's prefix, of the sandbox of cont, with labels: the number of
// samples of a distribution. The metric server does not export counters that
// are 0, so a metric that it does not export is 0.
func (te *metricsTest) metric(t *testing.T, cont *Container, name string, labels map[string]string) int64 {
	t.Helper()
	data, err := te.client.GetMetrics(te.testCtx, nil)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	v, _, err := data.GetPrometheusContainerInteger(metricclient.WantMetric{
		Metric:      "testmetric_" + name,
		Sandbox:     cont.Sandbox.ID,
		ExtraLabels: labels,
	})
	if errors.Is(err, metricclient.ErrNoData) {
		return 0
	}
	if err != nil {
		t.Fatalf("metric %s%v: %v", name, labels, err)
	}
	return v
}

// precopyStops returns the counts of pre-copies of the sandbox of cont by why
// their rounds stopped.
func (te *metricsTest) precopyStops(t *testing.T, cont *Container) map[string]int64 {
	t.Helper()
	stops := make(map[string]int64)
	for _, reason := range []string{"converged", "round_cap", "not_halved", "skipped"} {
		if n := te.metric(t, cont, "checkpoint_precopy_stops", map[string]string{"reason": reason}); n != 0 {
			stops[reason] = n
		}
	}
	return stops
}

// TestCheckpointPrecopyAB pre-copies experiment 06's A-B writer while it
// rewrites random pages of its 32 MiB at several rates, to a store that takes
// 16 MiB/s, with dirty tracking verified; then restores each image, which must
// pass the writer's check of every page. Below half the store's speed, the
// rounds converge; at four times it (64 MiB/s), the halving rule must stop
// them after the first, rather than the round cap after 8. The pre-copy's
// metrics must describe its rounds and its pause, and the cost of writing
// pages that they measure must be at least the store's. The writer's memory is
// in small pages: write-protection tracks a huge page whole, so random writes
// to huge pages keep the rounds from converging at these rates (as on CI's
// runners with transparent huge pages for shared memory).
func TestCheckpointPrecopyAB(t *testing.T) {
	const storeRate = 16 << 20
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, test := range []struct {
			rateMiB int
			stop    string
		}{
			{rateMiB: 1, stop: "converged"},
			{rateMiB: 4, stop: "converged"},
			{rateMiB: 64, stop: "not_halved"},
		} {
			t.Run(fmt.Sprintf("%s/%dMiBps", name, test.rateMiB), func(t *testing.T) {
				// After the containers' cleanups, which run first.
				te, cleanup := setupMetrics(t, false /* forceTempUDS */)
				t.Cleanup(cleanup)
				c := precopyConf(conf, storeRate)
				c.AppHugePages = false
				w := startABWriter(t, te.applyConf(c), "32", fmt.Sprint(test.rateMiB))
				image, err := w.checkpoint(precopyOpts("on"))
				if err != nil {
					t.Fatalf("error checkpointing with pre-copy: %v", err)
				}

				rounds := te.metric(t, w.cont, "checkpoint_precopy_rounds", nil)
				stops := te.precopyStops(t, w.cont)
				if len(stops) != 1 || stops[test.stop] != 1 {
					t.Errorf("pre-copies by why their rounds stopped: %v, want 1 stopped by %s", stops, test.stop)
				}
				if test.stop == "not_halved" && rounds != 1 {
					t.Errorf("%d rounds, want 1: the first does not halve the bytes left", rounds)
				}
				for _, m := range []struct {
					name string
					want int64
				}{
					{"checkpoint_precopy_round_bytes", rounds},
					{"checkpoint_precopy_pending_bytes", rounds},
					{"checkpoint_precopy_pause", 1},
					{"checkpoint_precopy_longest_stall", 1},
				} {
					if got := te.metric(t, w.cont, m.name, nil); got != m.want {
						t.Errorf("%s has %d samples, want %d", m.name, got, m.want)
					}
				}
				if cost, minCost := time.Duration(te.metric(t, w.cont, "checkpoint_pages_write_cost", nil)), time.Second/(storeRate>>20); cost < minCost {
					t.Errorf("measured %v per MiB written, want at least the store's %v", cost, minCost)
				}

				w.restore(image, nil)
				if got := w.check(); !strings.HasPrefix(got, "PASS ") {
					t.Errorf("abwriter's check after restoring %d rounds of pre-copy: %s", rounds, got)
				}
			})
		}
	}
}

// TestCheckpointPrecopyChurn pre-copies the A-B writer while it churns memory
// as well (it allocates, fills and frees regions, discards pages with
// MADV_DONTNEED, and forks children that write their copies of its pages),
// with dirty tracking verified: the checkpoint must find no write that
// escaped tracking, and its restore must pass the writer's check. As negative
// controls, with each of the dirty source's own marking paths disabled, the
// verification of the checkpoint must fail.
func TestCheckpointPrecopyChurn(t *testing.T) {
	const storeRate = 16 << 20
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, src := range incrementalDirtySources {
			breaks := append([]config.DirtyTrackingBreak{config.DirtyTrackingBreakNone}, src.breaks...)
			for _, brk := range breaks {
				t.Run(fmt.Sprintf("%s/%s/%v", name, src.name, brk), func(t *testing.T) {
					c := withRootDir(t, incrementalConf(precopyConf(conf, storeRate), src, true /* verify */, brk))
					w := startABWriter(t, c, "-c", "32", "4")
					image, err := w.checkpoint(precopyOpts("on"))
					if brk != config.DirtyTrackingBreakNone {
						if err == nil || !strings.Contains(err.Error(), kernel.ErrDirtyTrackingEscapes.Error()) {
							t.Errorf("checkpoint with %v disabled: got %v, want writes that escaped tracking", brk, err)
						}
						return
					}
					if err != nil {
						t.Fatalf("error checkpointing with pre-copy: %v", err)
					}
					w.restore(image, nil)
					if got := w.check(); !strings.HasPrefix(got, "PASS ") {
						t.Errorf("abwriter's check after restoring: %s", got)
					}
				})
			}
		}
	}
}

// TestCheckpointPrecopyPrivateMemoryFile pre-copies the A-B writer whose
// buffer is a file of the root filesystem's overlay, backed by a filestore on
// disk, and so in a private MemoryFile rather than the application's. Unchanged,
// the file is written by the rounds, not in the pause; rewritten while the
// rounds run, in an incremental checkpoint of a full one, the chain's restore
// must pass the writer's check.
func TestCheckpointPrecopyPrivateMemoryFile(t *testing.T) {
	const (
		storeRate = 16 << 20
		bufMiB    = 32
	)
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			// After the containers' cleanups, which run first.
			te, cleanup := setupMetrics(t, false /* forceTempUDS */)
			t.Cleanup(cleanup)
			c := te.applyConf(precopyConf(conf, storeRate))
			filestoreDir := t.TempDir()
			if err := os.Chmod(filestoreDir, 0777); err != nil {
				t.Fatal(err)
			}
			if err := c.Overlay2.Set("root:dir=" + filestoreDir); err != nil {
				t.Fatal(err)
			}

			// The writer does not write: the pause writes only what the
			// sandbox changed besides, a few MiB at most.
			w := startABWriter(t, c, "-f", "/abwriter.buf", fmt.Sprint(bufMiB), "0")
			image, err := w.checkpoint(precopyOpts("on"))
			if err != nil {
				t.Fatalf("error checkpointing with pre-copy: %v", err)
			}
			st, err := os.Stat(filepath.Join(image, checkpointfiles.PagesFileName))
			if err != nil {
				t.Fatal(err)
			}
			copied := te.metric(t, w.cont, "checkpoint_precopy_bytes", nil)
			if paused := st.Size() - copied; paused >= bufMiB<<20/4 {
				t.Errorf("the pause wrote %d bytes (pages file %d bytes, rounds %d), want the %d MiB file written by the rounds", paused, st.Size(), copied, bufMiB)
			}
			w.restore(image, nil)
			if got := w.check(); !strings.HasPrefix(got, "PASS ") {
				t.Errorf("abwriter's check after restoring: %s", got)
			}

			// An incremental checkpoint of a full one, pre-copied while the
			// writer rewrites its file at 4 MiB/s.
			w = startABWriter(t, c, "-f", "/abwriter.buf", fmt.Sprint(bufMiB), "4")
			full, err := w.checkpoint(sandbox.CheckpointOpts{})
			if err != nil {
				t.Fatalf("error checkpointing: %v", err)
			}
			opts := precopyOpts("on")
			opts.ParentImagePath = full
			delta, err := w.checkpoint(opts)
			if err != nil {
				t.Fatalf("error checkpointing incrementally with pre-copy: %v", err)
			}
			w.restore(delta, []string{full})
			if got := w.check(); !strings.HasPrefix(got, "PASS ") {
				t.Errorf("abwriter's check after restoring the chain: %s", got)
			}
		})
	}
}

// TestCheckpointPrecopyAuto checks --precopy=auto: the first checkpoint of a
// sandbox, which has no measure of the cost of writing pages, runs the rounds,
// the first of which measures it; the next skips them if, at that cost, the
// pause writes memory within the budget, and runs them otherwise.
func TestCheckpointPrecopyAuto(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, test := range []struct {
			name      string
			writeRate uint64
			budget    time.Duration
			skipped   bool
		}{
			// About 40 MiB of memory takes 2.5 s to write.
			{name: "slow", writeRate: 16 << 20, budget: 100 * time.Millisecond},
			// Without a limit, writing it takes well under a second.
			{name: "fast", budget: time.Second, skipped: true},
		} {
			t.Run(fmt.Sprintf("%s/%s", name, test.name), func(t *testing.T) {
				// After the containers' cleanups, which run first.
				te, cleanup := setupMetrics(t, false /* forceTempUDS */)
				t.Cleanup(cleanup)
				w := startABWriter(t, te.applyConf(precopyConf(conf, test.writeRate)), "32", "0")
				opts := precopyOpts("auto")
				opts.PrecopyBudget = test.budget
				if _, err := w.checkpoint(opts); err != nil {
					t.Fatalf("error checkpointing with --precopy=auto: %v", err)
				}
				first := te.metric(t, w.cont, "checkpoint_precopy_rounds", nil)
				if first == 0 {
					t.Errorf("the first checkpoint with --precopy=auto ran no round, want the first to measure the cost of writing pages")
				}
				image, err := w.checkpoint(opts)
				if err != nil {
					t.Fatalf("error checkpointing again with --precopy=auto: %v", err)
				}
				second := te.metric(t, w.cont, "checkpoint_precopy_rounds", nil) - first
				skipped := te.precopyStops(t, w.cont)["skipped"]
				if (second == 0) != test.skipped || (skipped == 1) != test.skipped {
					t.Errorf("the second checkpoint ran %d rounds, and %d were skipped; want them skipped: %t", second, skipped, test.skipped)
				}
				w.restore(image, nil)
				if got := w.check(); !strings.HasPrefix(got, "PASS ") {
					t.Errorf("abwriter's check after restoring: %s", got)
				}
			})
		}
	}
}
