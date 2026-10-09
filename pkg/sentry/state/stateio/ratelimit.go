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

package stateio

import (
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// RateLimitedReader is an AsyncReader that completes the reads of another no
// sooner than a device delivering a fixed number of bytes per second, in the
// order the reads were issued, would: as a disk throttled by cgroup io.max
// does. It lets tests restore from slow storage where they cannot throttle a
// device, e.g. without privileges.
type RateLimitedReader struct {
	AsyncReader

	// rate is the bandwidth of the device in bytes per second.
	rate uint64

	// busyUntil is when the device has delivered every read issued so far.
	busyUntil time.Time

	// due maps the ID of each read in flight to when it completes.
	due map[int]time.Time

	// done holds completions of the underlying reader that are not due yet.
	done []Completion
}

// NewRateLimitedReader returns a RateLimitedReader that completes the reads
// of ar at rate bytes per second. It takes ownership of ar.
//
// Preconditions: rate > 0.
func NewRateLimitedReader(ar AsyncReader, rate uint64) *RateLimitedReader {
	if rate == 0 {
		panic("invalid rate")
	}
	return &RateLimitedReader{
		AsyncReader: ar,
		rate:        rate,
		due:         make(map[int]time.Time),
	}
}

// issue records the issue of a read of n bytes with the given ID.
func (r *RateLimitedReader) issue(id int, n uint64) {
	start := time.Now()
	if r.busyUntil.After(start) {
		start = r.busyUntil
	}
	r.busyUntil = start.Add(time.Duration(n * uint64(time.Second) / r.rate))
	r.due[id] = r.busyUntil
}

// AddRead implements AsyncReader.AddRead.
func (r *RateLimitedReader) AddRead(id int, off int64, dstFile DestinationFile, dstFR memmap.FileRange, dstMap []byte) {
	r.issue(id, uint64(len(dstMap)))
	r.AsyncReader.AddRead(id, off, dstFile, dstFR, dstMap)
}

// AddReadv implements AsyncReader.AddReadv.
func (r *RateLimitedReader) AddReadv(id int, off int64, total uint64, dstFile DestinationFile, dstFRs []memmap.FileRange, dstMaps []unix.Iovec) {
	r.issue(id, total)
	r.AsyncReader.AddReadv(id, off, total, dstFile, dstFRs, dstMaps)
}

// Wait implements AsyncReader.Wait.
func (r *RateLimitedReader) Wait(cs []Completion, minCompletions int) ([]Completion, error) {
	return r.wait(cs, minCompletions, nil)
}

// WaitOr implements WaitOrAsyncReader.WaitOr.
func (r *RateLimitedReader) WaitOr(cs []Completion, wake <-chan struct{}) ([]Completion, error) {
	return r.wait(cs, 1, wake)
}

// wait appends completions that are due to cs until it has appended
// minCompletions, or until wake, if not nil, is readable while it waits for a
// completion to become due. It waits for the underlying reader's completions
// without watching wake, which is meant for readers whose reads take much less
// time than the rate makes them last, such as a local file's.
func (r *RateLimitedReader) wait(cs []Completion, minCompletions int, wake <-chan struct{}) ([]Completion, error) {
	n := 0
	for {
		var err error
		if r.done, err = r.AsyncReader.Wait(r.done, 0); err != nil {
			return cs, err
		}
		now := time.Now()
		var next time.Time
		pending := r.done[:0]
		for _, c := range r.done {
			if due := r.due[c.ID]; !due.After(now) {
				delete(r.due, c.ID)
				cs = append(cs, c)
				n++
			} else {
				pending = append(pending, c)
				if next.IsZero() || due.Before(next) {
					next = due
				}
			}
		}
		r.done = pending
		if n >= minCompletions {
			return cs, nil
		}
		if next.IsZero() || r.earliestDue().Before(next) {
			// A read that has not completed is due first.
			if r.done, err = r.AsyncReader.Wait(r.done, 1); err != nil {
				return cs, err
			}
			continue
		}
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-timer.C:
		case <-wake:
			timer.Stop()
			return cs, nil
		}
	}
}

// earliestDue returns when the first read in flight completes.
func (r *RateLimitedReader) earliestDue() time.Time {
	var earliest time.Time
	for _, due := range r.due {
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	return earliest
}
