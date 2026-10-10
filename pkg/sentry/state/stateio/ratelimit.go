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

// rateLimiter completes the I/O of an AsyncReader or AsyncWriter no sooner
// than a device transferring a fixed number of bytes per second, in the order
// the I/O was issued, would: as a disk throttled by cgroup io.max does.
type rateLimiter struct {
	// rate is the bandwidth of the device in bytes per second.
	rate uint64

	// busyUntil is when the device has transferred every byte issued so far.
	busyUntil time.Time

	// due maps the ID of each I/O in flight to when it completes.
	due map[int]time.Time

	// done holds completions of the underlying file that are not due yet.
	done []Completion
}

func newRateLimiter(rate uint64) rateLimiter {
	if rate == 0 {
		panic("invalid rate")
	}
	return rateLimiter{
		rate: rate,
		due:  make(map[int]time.Time),
	}
}

// issue records the issue of an I/O of n bytes with the given ID.
func (l *rateLimiter) issue(id int, n uint64) {
	start := time.Now()
	if l.busyUntil.After(start) {
		start = l.busyUntil
	}
	l.busyUntil = start.Add(time.Duration(n * uint64(time.Second) / l.rate))
	l.due[id] = l.busyUntil
}

// wait appends completions that are due to cs until it has appended
// minCompletions, or until wake, if not nil, is readable while it waits for a
// completion to become due. waitIO is the underlying file's Wait. wait waits
// for the underlying file's completions without watching wake, which is meant
// for files whose I/O takes much less time than the rate makes it last, such
// as a local file's.
func (l *rateLimiter) wait(waitIO func([]Completion, int) ([]Completion, error), cs []Completion, minCompletions int, wake <-chan struct{}) ([]Completion, error) {
	n := 0
	for {
		var err error
		if l.done, err = waitIO(l.done, 0); err != nil {
			return cs, err
		}
		now := time.Now()
		var next time.Time
		pending := l.done[:0]
		for _, c := range l.done {
			if due := l.due[c.ID]; !due.After(now) {
				delete(l.due, c.ID)
				cs = append(cs, c)
				n++
			} else {
				pending = append(pending, c)
				if next.IsZero() || due.Before(next) {
					next = due
				}
			}
		}
		l.done = pending
		if n >= minCompletions {
			return cs, nil
		}
		if next.IsZero() || l.earliestDue().Before(next) {
			// An I/O that has not completed is due first.
			if l.done, err = waitIO(l.done, 1); err != nil {
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

// earliestDue returns when the first I/O in flight completes.
func (l *rateLimiter) earliestDue() time.Time {
	var earliest time.Time
	for _, due := range l.due {
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	return earliest
}

// RateLimitedReader is an AsyncReader that completes the reads of another no
// sooner than a device delivering a fixed number of bytes per second, in the
// order the reads were issued, would: as a disk throttled by cgroup io.max
// does. It lets tests restore from slow storage where they cannot throttle a
// device, e.g. without privileges.
type RateLimitedReader struct {
	AsyncReader
	limiter rateLimiter
}

// NewRateLimitedReader returns a RateLimitedReader that completes the reads
// of ar at rate bytes per second. It takes ownership of ar.
//
// Preconditions: rate > 0.
func NewRateLimitedReader(ar AsyncReader, rate uint64) *RateLimitedReader {
	return &RateLimitedReader{
		AsyncReader: ar,
		limiter:     newRateLimiter(rate),
	}
}

// AddRead implements AsyncReader.AddRead.
func (r *RateLimitedReader) AddRead(id int, off int64, dstFile DestinationFile, dstFR memmap.FileRange, dstMap []byte) {
	r.limiter.issue(id, uint64(len(dstMap)))
	r.AsyncReader.AddRead(id, off, dstFile, dstFR, dstMap)
}

// AddReadv implements AsyncReader.AddReadv.
func (r *RateLimitedReader) AddReadv(id int, off int64, total uint64, dstFile DestinationFile, dstFRs []memmap.FileRange, dstMaps []unix.Iovec) {
	r.limiter.issue(id, total)
	r.AsyncReader.AddReadv(id, off, total, dstFile, dstFRs, dstMaps)
}

// Wait implements AsyncReader.Wait.
func (r *RateLimitedReader) Wait(cs []Completion, minCompletions int) ([]Completion, error) {
	return r.limiter.wait(r.AsyncReader.Wait, cs, minCompletions, nil)
}

// WaitOr implements WaitOrAsyncReader.WaitOr.
func (r *RateLimitedReader) WaitOr(cs []Completion, wake <-chan struct{}) ([]Completion, error) {
	return r.limiter.wait(r.AsyncReader.Wait, cs, 1, wake)
}

// RateLimitedWriter is an AsyncWriter that completes the writes of another no
// sooner than a device storing a fixed number of bytes per second, in the
// order the writes were issued, would: as a disk throttled by cgroup io.max,
// or a remote store, does. It lets tests checkpoint to slow storage where
// they cannot throttle a device, e.g. without privileges.
type RateLimitedWriter struct {
	AsyncWriter
	limiter rateLimiter
}

// NewRateLimitedWriter returns a RateLimitedWriter that completes the writes
// of aw at rate bytes per second. It takes ownership of aw.
//
// Preconditions: rate > 0.
func NewRateLimitedWriter(aw AsyncWriter, rate uint64) *RateLimitedWriter {
	return &RateLimitedWriter{
		AsyncWriter: aw,
		limiter:     newRateLimiter(rate),
	}
}

// AddWrite implements AsyncWriter.AddWrite.
func (w *RateLimitedWriter) AddWrite(id int, srcFile SourceFile, srcFR memmap.FileRange, srcMap []byte) {
	w.limiter.issue(id, uint64(len(srcMap)))
	w.AsyncWriter.AddWrite(id, srcFile, srcFR, srcMap)
}

// AddWritev implements AsyncWriter.AddWritev.
func (w *RateLimitedWriter) AddWritev(id int, total uint64, srcFile SourceFile, srcFRs []memmap.FileRange, srcMaps []unix.Iovec) {
	w.limiter.issue(id, total)
	w.AsyncWriter.AddWritev(id, total, srcFile, srcFRs, srcMaps)
}

// Wait implements AsyncWriter.Wait.
func (w *RateLimitedWriter) Wait(cs []Completion, minCompletions int) ([]Completion, error) {
	return w.limiter.wait(w.AsyncWriter.Wait, cs, minCompletions, nil)
}
