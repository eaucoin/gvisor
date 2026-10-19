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
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

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

// waitWithinAsync runs waitWithin in a goroutine of its own: the channel
// receives when it returned false, or the zero time if it returned true.
func waitWithinAsync(done <-chan struct{}, timeout time.Duration, f *fakeLoads) <-chan time.Time {
	returned := make(chan time.Time, 1)
	go func() {
		if waitWithin(done, timeout, f.progress) {
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
		if !waitWithin(done, timeout, (&fakeLoads{}).progress) {
			t.Errorf("waitWithin of what was done: got false, want true")
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
}
