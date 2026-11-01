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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// TestCheckpointPrecopyWriters pre-copies 1, 2, 3, 4 and 8 A-B writers that
// share a heap and rewrite it as fast as they can, with the flags of alasio's
// live saves (--leave-running --precopy=auto --precopy-throttle=on --direct,
// write-protection in 64 KiB units, small pages), five times each, and logs
// what the sandbox says of each pre-copy's rounds and pause: 1 GiB to the
// local disk, and 256 MiB to a store that takes 32 MiB/s, below the writers'
// dirtying rate. Throttling limited each MemoryManager's dirtying to a quarter
// of the measured write speed, so that writers in several processes dirtied
// memory together at several times that; it limits all of them together now:
// this measures whether the pause grows with their number. Each image, restored, must pass every writer's
// check.
func TestCheckpointPrecopyWriters(t *testing.T) {
	const runs = 5
	app, err := testutil.FindFile("test/cmd/precopy/abwriter")
	if err != nil {
		t.Fatalf("finding abwriter: %v", err)
	}
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, store := range []struct {
			name    string
			rate    uint64
			heapMiB int
		}{
			{"local", 0, 1024},
			{"32MiBps", 32 << 20, 256},
		} {
			heapMiB := store.heapMiB
			for _, writers := range []int{1, 2, 3, 4, 8} {
				for run := 0; run < runs; run++ {
					t.Run(fmt.Sprintf("%s/%s/%dwriters/%d", name, store.name, writers, run), func(t *testing.T) {
						c := *conf
						c.DirtyTracking = config.DirtyTrackingWriteProtect
						c.AppHugePages = false
						c.TestOnlyCheckpointWriteRate = store.rate
						c2 := withRootDir(t, &c)
						dir, err := os.MkdirTemp(testutil.TmpDir(), "writers")
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { os.RemoveAll(dir) })
						// Each writes as fast as it can, far below 100 GiB/s. The
						// last becomes the container's init, so that signals to
						// every process reach writers alone.
						var script strings.Builder
						var outs []reports
						for i := 0; i < writers; i++ {
							comm := filepath.Join(dir, fmt.Sprint(i))
							if err := os.Mkdir(comm, 0777); err != nil {
								t.Fatal(err)
							}
							cmd := fmt.Sprintf("%s %d 100000 %s", app, heapMiB/writers, comm)
							if i < writers-1 {
								fmt.Fprintf(&script, "%s & ", cmd)
							} else {
								fmt.Fprintf(&script, "exec %s", cmd)
							}
							outs = append(outs, reports{path: filepath.Join(comm, "out")})
						}
						spec := testutil.NewSpecWithArgs("/bin/sh", "-c", script.String())
						bundleDir, cleanup, err := testutil.SetupBundleDir(spec)
						if err != nil {
							t.Fatalf("error setting up bundle: %v", err)
						}
						t.Cleanup(cleanup)
						newCont := func() *Container {
							cont, err := New(c2, Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir})
							if err != nil {
								t.Fatalf("error creating container: %v", err)
							}
							t.Cleanup(func() { cont.Destroy() })
							return cont
						}
						cont := newCont()
						if err := cont.Start(c2); err != nil {
							t.Fatalf("error starting container: %v", err)
						}
						for i := range outs {
							if got, err := outs[i].next(cont); err != nil || got != "READY" {
								t.Fatalf("writer %d reported %q (%v), want READY", i, got, err)
							}
						}
						// Let them write for a second, as a session does before
						// its save.
						time.Sleep(time.Second)
						start := time.Now()
						image := filepath.Join(dir, "image")
						if err := os.Mkdir(image, 0755); err != nil {
							t.Fatal(err)
						}
						if err := cont.Checkpoint(c2, image, sandbox.CheckpointOpts{
							Compression:      statefile.CompressionLevelNone,
							Resume:           true,
							Direct:           true,
							Precopy:          "auto",
							PrecopyBudget:    100 * time.Millisecond,
							PrecopyMaxRounds: 8,
							PrecopyThrottle:  true,
						}); err != nil {
							t.Fatalf("error checkpointing: %v", err)
						}
						t.Logf("%s: %d writers: checkpoint took %v; the sandbox said: %q", store.name, writers, time.Since(start), precopySaid(t, start))

						cont.Destroy()
						cont = newCont()
						if err := cont.Restore(c2, image, sandbox.RestoreOpts{}); err != nil {
							t.Fatalf("error restoring: %v", err)
						}
						if err := cont.SignalContainer(unix.SIGUSR1, true /* all */); err != nil {
							t.Fatalf("error signaling the writers: %v", err)
						}
						for i := range outs {
							if got, err := outs[i].next(cont); err != nil || !strings.HasPrefix(got, "PASS ") {
								t.Errorf("writer %d's check after restoring: %q (%v)", i, got, err)
							}
						}
					})
				}
			}
		}
	}
}

// precopySaid returns the lines that the sandboxes' boot logs wrote since
// start that tell a pre-copy's rounds and its save's pause.
func precopySaid(t *testing.T, start time.Time) []string {
	return bootLogSince(t, start, "Pre-copy round", "Pre-copy done", "Pre-copy: limiting", "Pre-copy: tasks were kept", "paused for", "Kernel save took", "Memory files save took")
}
