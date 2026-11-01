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

package state

import (
	"time"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// Copy-on-write releases.
//
// A release is a save that kills the sandbox (SaveOpts.Resume false). Under
// copy-on-write, its tasks stay paused after the save point while its pages
// are written, so that nothing they do outside memory, such as writes to
// files or packets, postdates the image; then it kills the sandbox. It saves
// as a live save does, so that it can be aborted: if AbortRelease is called
// before its pages are written, its tasks resume as soon as its save point is
// taken, its pages are written while they run, as a live save's are, and the
// sandbox runs on.

var (
	releaseMu sync.Mutex

	// current is the copy-on-write release under way, if any. current is
	// protected by releaseMu.
	current *release
)

// release is a copy-on-write release.
type release struct {
	// abort is closed when the release is asked to abort.
	abort chan struct{}

	// asked is when the release was asked to abort, or zero if it was not.
	// asked is protected by releaseMu.
	asked time.Time

	// ended is true once the release can no longer be aborted. ended is
	// protected by releaseMu.
	ended bool
}

// beginRelease makes a new release the current one and returns it.
func beginRelease() *release {
	releaseMu.Lock()
	defer releaseMu.Unlock()
	current = &release{abort: make(chan struct{})}
	return current
}

// endRelease ends the current release, which can no longer be aborted.
func endRelease() {
	releaseMu.Lock()
	defer releaseMu.Unlock()
	if current != nil {
		current.ended = true
		current = nil
	}
}

// AbortRelease asks the copy-on-write release under way to resume its tasks
// and the sandbox to run on. It returns false if there is no release to
// abort: none is under way, or its pages are already written.
func AbortRelease() bool {
	releaseMu.Lock()
	defer releaseMu.Unlock()
	r := current
	if r == nil || r.ended {
		return false
	}
	if r.asked.IsZero() {
		r.asked = time.Now()
		close(r.abort)
		log.Infof("Release asked to abort.")
	}
	return true
}

// end ends r's abortable time, and returns whether r was asked to abort
// before.
func (r *release) end() bool {
	releaseMu.Lock()
	defer releaseMu.Unlock()
	r.ended = true
	return !r.asked.IsZero()
}

// copy writes r's pages with copying, while its tasks stay paused, unless r
// is asked to abort first: then it calls resume, and the pages are written
// while the tasks run. It returns whether r was aborted, and copying's error.
func (r *release) copy(copying func() error, resume func()) (bool, error) {
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- copying()
	}()
	var (
		err    error
		copied bool
	)
	select {
	case err = <-done:
		copied = true
	case <-r.abort:
	}
	if !r.end() {
		log.Infof("Copy-on-write release: pages written in %v with tasks paused.", time.Since(start))
		return false, err
	}
	resume()
	releaseMu.Lock()
	asked := r.asked
	releaseMu.Unlock()
	log.Infof("Copy-on-write release aborted: tasks resumed %v after the abort was asked, %v after the save point.", time.Since(asked), time.Since(start))
	if !copied {
		err = <-done
		log.Infof("Copy-on-write release aborted: pages written %v after the save point.", time.Since(start))
	}
	return true, err
}
