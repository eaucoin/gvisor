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

package control

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	pkgstate "gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/state/wire"
	"gvisor.dev/gvisor/pkg/urpc"
)

func TestStateSaveGraphEngineOverride(t *testing.T) {
	prevEngine := pkgstate.GraphEngineEnabled()
	defer pkgstate.EnableGraphEngine(prevEngine)

	for _, tc := range []struct {
		name           string
		processDefault bool
		override       bool
	}{
		{
			name:           "override_wire_with_graph",
			processDefault: false,
			override:       true,
		},
		{
			name:           "override_graph_with_wire",
			processDefault: true,
			override:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkgstate.EnableGraphEngine(tc.processDefault)
			defer pkgstate.EnableGraphEngine(prevEngine)

			k, err := testutil.Boot()
			if err != nil {
				t.Fatalf("testutil.Boot() failed: %v", err)
			}
			defer k.Timekeeper().Destroy()
			k.GlobalInit().Release(k.SupervisorContext())
			k.TestOnlySetGlobalInit(nil)
			w := watchdog.New(k, watchdog.Opts{TaskTimeout: time.Minute})
			defer w.Stop()

			statePath := filepath.Join(t.TempDir(), "checkpoint.img")
			outFile, err := os.Create(statePath)
			if err != nil {
				t.Fatalf("os.Create(%s) failed: %v", statePath, err)
			}
			defer outFile.Close()

			override := tc.override
			opts := &controlapi.SaveOpts{
				FilePayload: urpc.FilePayload{
					Files: []*os.File{outFile},
				},
				Metadata:    statefile.CompressionLevelFlateBestSpeed.ToMetadata(),
				Resume:      true,
				GraphEngine: &override,
			}

			s := &State{Kernel: k, Watchdog: w}
			if err := s.Save(opts, &struct{}{}); err != nil {
				t.Fatalf("State.Save() failed: %v", err)
			}

			if got := pkgstate.GraphEngineEnabled(); got != tc.processDefault {
				t.Errorf("GraphEngineEnabled() after Save = %v, want preserved process default %v", got, tc.processDefault)
			}

			inFile, err := os.Open(statePath)
			if err != nil {
				t.Fatalf("os.Open(%s) failed: %v", statePath, err)
			}
			defer inFile.Close()
			rc, _, err := statefile.NewReader(inFile, nil)
			if err != nil {
				t.Fatalf("statefile.NewReader failed: %v", err)
			}
			defer rc.Close()

			hdr, err := pkgstate.ReadHeaderMode(&wire.Reader{Reader: rc})
			if err != nil {
				t.Fatalf("ReadHeaderMode failed: %v", err)
			}
			if !hdr.Object || hdr.Graph != tc.override {
				t.Errorf("saved header = %+v, want Object=true Graph=%v", hdr, tc.override)
			}
		})
	}
}

// TestResumeAfterBinaryExpiredLeavesCuda checks that the resume of a save
// whose save/restore binary was killed for waiting past its load wait does not
// undo CUDA's checkpoint, which preSave runs after the binary and so did not
// run for it: CUDA's checkpoint state, as one before left it, is left as it
// is.
func TestResumeAfterBinaryExpiredLeavesCuda(t *testing.T) {
	k, err := testutil.Boot()
	if err != nil {
		t.Fatalf("testutil.Boot() failed: %v", err)
	}
	defer k.Timekeeper().Destroy()
	k.AddStateToCheckpoint(cudaCheckpointPathKey, "/cuda-checkpoint")
	k.AddStateToCheckpoint(cudaCheckpointSequentialKey, false)
	k.AddStateToCheckpoint(cudaProcsKey, []*kernel.ThreadGroup(nil))
	if err := resumeAfterBinaryExpired(k)(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := k.PopCheckpointState(cudaCheckpointPathKey); got != "/cuda-checkpoint" {
		t.Errorf("CUDA's checkpoint state after the resume: got %v, want it left as it was", got)
	}
}

// TestStateServesRPCs checks that State, every exported method of which
// serves an RPC, registers with a urpc server, as the sandbox's controller
// registers it as it boots.
func TestStateServesRPCs(t *testing.T) {
	urpc.NewServer().Register(&State{})
}

// fakeLoads is what async page loads are doing, as a test sets it.
type fakeLoads struct {
	mu sync.Mutex
	p  pgalloc.AsyncPageLoadProgress
	// block, if true, has progress never answer.
	block bool
}

func (f *fakeLoads) set(p pgalloc.AsyncPageLoadProgress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.p = p
}

func (f *fakeLoads) progress() pgalloc.AsyncPageLoadProgress {
	f.mu.Lock()
	if f.block {
		f.mu.Unlock()
		select {}
	}
	defer f.mu.Unlock()
	return f.p
}

// waitWithinAsync runs waitWithin, without a load wait, in a goroutine of its
// own: the channel receives when it timed out, or the zero time if it was
// done.
func waitWithinAsync(done <-chan struct{}, timeout time.Duration, f *fakeLoads) <-chan time.Time {
	returned := make(chan time.Time, 1)
	go func() {
		if waitWithin(done, timeout, time.Time{}, f.progress) == waitDone {
			returned <- time.Time{}
			return
		}
		returned <- time.Now()
	}()
	return returned
}

func TestWaitWithin(t *testing.T) {
	tick := saveRestoreExecTick
	saveRestoreExecTick = 5 * time.Millisecond
	t.Cleanup(func() { saveRestoreExecTick = tick })
	const timeout = 200 * time.Millisecond
	loading := pgalloc.AsyncPageLoadProgress{Loading: true}
	stalled := pgalloc.AsyncPageLoadProgress{Loading: true, Stalled: true}

	// What ends in time ends.
	t.Run("done", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		if got := waitWithin(done, timeout, time.Time{}, (&fakeLoads{}).progress); got != waitDone {
			t.Errorf("waitWithin of what was done: got %v, want %v", got, waitDone)
		}
	})

	// Without async page loads, or with loads that do not stall, the timeout
	// counts all of the time.
	for name, p := range map[string]pgalloc.AsyncPageLoadProgress{
		"no loads":       {},
		"loads that run": loading,
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			returned := waitWithinAsync(make(chan struct{}), timeout, &fakeLoads{p: p})
			select {
			case at := <-returned:
				if at.IsZero() {
					t.Fatalf("waitWithin of what never ended: got true, want false")
				}
				if took := at.Sub(start); took < timeout || took > timeout+time.Second {
					t.Errorf("waitWithin took %v, want its timeout, %v", took, timeout)
				}
			case <-time.After(timeout + 5*time.Second):
				t.Fatalf("waitWithin had not returned %v after its timeout", 5*time.Second)
			}
		})
	}

	// Loads that stall stop its clock: it ends once its timeout ran out of the
	// time they did not.
	t.Run("loads that stall", func(t *testing.T) {
		f := &fakeLoads{p: stalled}
		start := time.Now()
		returned := waitWithinAsync(make(chan struct{}), timeout, f)
		const stall = 3 * timeout
		select {
		case <-returned:
			t.Fatalf("waitWithin returned while the loads stalled")
		case <-time.After(stall):
		}
		resumed := time.Now()
		f.set(loading)
		select {
		case at := <-returned:
			if at.IsZero() {
				t.Fatalf("waitWithin of what never ended: got true, want false")
			}
			if took := at.Sub(resumed); took > timeout+time.Second {
				t.Errorf("waitWithin took %v after the stall, want about its timeout, %v", took, timeout)
			}
			if took := at.Sub(start); took < stall {
				t.Errorf("waitWithin took %v, less than the stall's %v", took, stall)
			}
		case <-time.After(timeout + 5*time.Second):
			t.Fatalf("waitWithin had not returned %v after its timeout ran out", 5*time.Second)
		}
	})

	// Loads that stall once it began, the clock stops then; and once they have
	// no page left to load, it no more stops.
	t.Run("loads that stall later", func(t *testing.T) {
		f := &fakeLoads{p: loading}
		returned := waitWithinAsync(make(chan struct{}), timeout, f)
		time.Sleep(timeout / 2)
		f.set(stalled)
		select {
		case <-returned:
			t.Fatalf("waitWithin returned while the loads stalled")
		case <-time.After(2 * timeout):
		}
		f.set(pgalloc.AsyncPageLoadProgress{})
		select {
		case at := <-returned:
			if at.IsZero() {
				t.Fatalf("waitWithin of what never ended: got true, want false")
			}
		case <-time.After(timeout + 5*time.Second):
			t.Fatalf("waitWithin had not returned %v after its timeout ran out", 5*time.Second)
		}
	})

	// Loads that keep it waiting for an answer, as a reader in a process that
	// is wedged does, stall nothing: the timeout counts the time it waits.
	for name, block := range map[string]func(f *fakeLoads){
		"progress never answers": func(f *fakeLoads) { f.block = true },
		"progress stops answering": func(f *fakeLoads) {
			go func() {
				time.Sleep(timeout / 2)
				f.mu.Lock()
				defer f.mu.Unlock()
				f.block = true
			}()
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLoads{p: stalled}
			block(f)
			start := time.Now()
			returned := waitWithinAsync(make(chan struct{}), timeout, f)
			select {
			case at := <-returned:
				if at.IsZero() {
					t.Fatalf("waitWithin of what never ended: got true, want false")
				}
				if took := at.Sub(start); took > timeout+time.Second {
					t.Errorf("waitWithin took %v, want about its timeout, %v", took, timeout)
				}
			case <-time.After(timeout + 5*time.Second):
				t.Fatalf("waitWithin had not returned %v after its timeout, its progress not answering", 5*time.Second)
			}
		})
	}

	// And what ends while the loads stall ends.
	t.Run("done while loads stall", func(t *testing.T) {
		done := make(chan struct{})
		returned := waitWithinAsync(done, timeout, &fakeLoads{p: stalled})
		time.Sleep(2 * timeout)
		close(done)
		select {
		case at := <-returned:
			if !at.IsZero() {
				t.Errorf("waitWithin of what ended while the loads stalled: got false, want true")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("waitWithin had not returned 5 s after what it waited for ended")
		}
	})

	// A load wait ends the wait at the first tick past its deadline at which
	// the loads stall, rather than stop the clock; loads that run past it
	// leave the timeout as it was.
	for _, tc := range []struct {
		name string
		p    pgalloc.AsyncPageLoadProgress
		want waitOutcome
		// minTook and maxTook bound the time the wait takes.
		minTook, maxTook time.Duration
	}{
		{name: "loads that stall past the load wait", p: stalled, want: waitLoadWaitExpired, minTook: timeout / 2, maxTook: timeout},
		{name: "loads that run past the load wait", p: loading, want: waitTimedOut, minTook: timeout, maxTook: timeout + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			returned := make(chan waitOutcome, 1)
			go func() {
				returned <- waitWithin(make(chan struct{}), timeout, start.Add(timeout/2), (&fakeLoads{p: tc.p}).progress)
			}()
			var got waitOutcome
			select {
			case got = <-returned:
			case <-time.After(timeout + 5*time.Second):
				t.Fatalf("waitWithin had not returned %v after its timeout", 5*time.Second)
			}
			took := time.Since(start)
			if got != tc.want {
				t.Errorf("waitWithin: got %v, want %v", got, tc.want)
			}
			if took < tc.minTook || took > tc.maxTook {
				t.Errorf("waitWithin took %v, want between %v and %v", took, tc.minTook, tc.maxTook)
			}
		})
	}
}
