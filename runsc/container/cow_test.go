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
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// cowConf returns a copy of conf whose sandboxes track dirty pages with
// write-protection, verified, write checkpoints at writeRate bytes per second
// if it is not 0, and take copy-on-write checkpoints, verified, if cow is
// true, with captures disabled on the path brk.
func cowConf(t *testing.T, conf *config.Config, writeRate uint64, cow bool, brk config.DirtyTrackingBreak) *config.Config {
	c := precopyConf(conf, writeRate)
	c.AppHugePages = false
	if cow {
		c.CoW = config.CoWOn
		c.TestOnlyCoWVerify = config.DirtyTrackingVerifyHash
	}
	c.TestOnlyCoWBreak = brk
	return withRootDir(t, c)
}

// gap returns the longest time that abwriter was kept from running since it
// was last asked.
func (w *abwriter) gap() time.Duration {
	w.t.Helper()
	if err := w.cont.SignalContainer(unix.SIGUSR2, false /* all */); err != nil {
		w.t.Fatalf("error signaling abwriter: %v", err)
	}
	got, err := w.out.next(w.cont)
	if err != nil {
		w.t.Fatalf("abwriter did not report its longest gap: %v", err)
	}
	us, ok := strings.CutPrefix(got, "GAP ")
	if !ok {
		w.t.Fatalf("abwriter reported %q, want its longest gap", got)
	}
	n, err := strconv.ParseUint(us, 10, 64)
	if err != nil {
		w.t.Fatalf("abwriter reported %q: %v", got, err)
	}
	return time.Duration(n) * time.Microsecond
}

// TestCheckpointCoWAB takes copy-on-write checkpoints of the A-B writer while
// it rewrites random pages of its 64 MiB at 1 to 200 MiB/s, to the local disk
// and to a store that takes 16 MiB/s, a whole image and then a delta of it,
// with dirty tracking and the captured pages verified: every page that a
// checkpoint writes must be the page at its save point, and each image,
// restored, must pass the writer's check of every page. The writer's longest
// gap during each checkpoint, which is how long the checkpoint kept the
// sandbox from running, is logged beside that of checkpoints that are not
// copy-on-write, which write their pages in the pause.
func TestCheckpointCoWAB(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, store := range []struct {
			name string
			rate uint64
		}{
			{"local", 0},
			{"16MiBps", 16 << 20},
		} {
			for _, rateMiB := range []int{1, 10, 50, 200} {
				for _, cow := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%dMiBps/cow=%v", name, store.name, rateMiB, cow), func(t *testing.T) {
						w := startABWriter(t, cowConf(t, conf, store.rate, cow, config.DirtyTrackingBreakNone), "64", fmt.Sprint(rateMiB))
						// The first checkpoint starts dirty tracking, which
						// copy-on-write checkpoints need.
						if _, err := w.checkpoint(sandbox.CheckpointOpts{}); err != nil {
							t.Fatalf("error checkpointing: %v", err)
						}
						w.gap()
						whole, err := w.checkpoint(sandbox.CheckpointOpts{})
						if err != nil {
							t.Fatalf("error checkpointing whole: %v", err)
						}
						wholeGap := w.gap()
						delta, err := w.checkpoint(sandbox.CheckpointOpts{ParentImagePath: whole})
						if err != nil {
							t.Fatalf("error checkpointing incrementally: %v", err)
						}
						deltaGap := w.gap()
						t.Logf("longest gap: whole %v, delta %v", wholeGap, deltaGap)

						w.restore(whole, nil)
						if got := w.check(); !strings.HasPrefix(got, "PASS ") {
							t.Errorf("abwriter's check after restoring the whole image: %s", got)
						}
						w.restore(delta, []string{whole})
						if got := w.check(); !strings.HasPrefix(got, "PASS ") {
							t.Errorf("abwriter's check after restoring the delta: %s", got)
						}
					})
				}
			}
		}
	}
}

// TestCheckpointCoWChurn takes copy-on-write checkpoints of the A-B writer
// while it churns memory as well (it allocates, fills and frees regions,
// discards pages with MADV_DONTNEED, and forks children that write their
// copies of its pages), copying late so that its writes reach the pages
// before the copier does: the checkpoints' captures must be the memory of
// their save points, and their restore must pass the writer's check. As
// negative controls, with the captures before first writes or before
// decommits disabled, the checkpoint must find pages written without being
// captured.
func TestCheckpointCoWChurn(t *testing.T) {
	const storeRate = 16 << 20
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, brk := range []config.DirtyTrackingBreak{config.DirtyTrackingBreakNone, config.DirtyTrackingBreakFault, config.DirtyTrackingBreakDecommit} {
			t.Run(fmt.Sprintf("%s/%v", name, brk), func(t *testing.T) {
				c := cowConf(t, conf, storeRate, true /* cow */, brk)
				c.TestOnlyCoWCopyDelay = 500 * time.Millisecond
				w := startABWriter(t, c, "-c", "64", "50")
				if _, err := w.checkpoint(sandbox.CheckpointOpts{}); err != nil {
					t.Fatalf("error checkpointing: %v", err)
				}
				image, err := w.checkpoint(sandbox.CheckpointOpts{})
				if brk != config.DirtyTrackingBreakNone {
					if err == nil || !strings.Contains(err.Error(), pgalloc.ErrCoWEscapes.Error()) {
						t.Errorf("checkpoint with %v captures disabled: got %v, want pages written without being captured", brk, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("error checkpointing: %v", err)
				}
				w.restore(image, nil)
				if got := w.check(); !strings.HasPrefix(got, "PASS ") {
					t.Errorf("abwriter's check after restoring: %s", got)
				}
			})
		}
	}
}
