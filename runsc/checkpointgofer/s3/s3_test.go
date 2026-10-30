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

package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3/s3test"
)

func init() {
	retryMaxBackoff = time.Millisecond
	pagesRetryFirstBackoff = time.Millisecond
	pagesRetryMaxBackoff = time.Millisecond
}

// newTestFileServer returns a FileServer for the store s.
func newTestFileServer(t *testing.T, s *s3test.Store, opts FileServerOptions) *FileServer {
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	opts.Endpoint = hs.URL
	opts.Bucket = s3test.Bucket
	opts.UsePathStyle = true
	opts.Credentials = credentials.NewStaticCredentialsProvider("key", "secret", "")
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 3
	}
	srv, err := NewFileServer(context.Background(), &opts)
	if err != nil {
		t.Fatalf("NewFileServer: %v", err)
	}
	t.Cleanup(srv.Destroy)
	return srv
}

// openRead opens the file at path for reading from srv.
func openRead(t *testing.T, srv *FileServer, path string) stateio.AsyncReader {
	r, err := srv.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead(%q): %v", path, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// read reads into bufs at off from r and returns the completion.
func read(t *testing.T, r stateio.AsyncReader, off int64, bufs ...[]byte) stateio.Completion {
	if len(bufs) == 1 {
		r.AddRead(0, off, nil, memmap.FileRange{}, bufs[0])
	} else {
		var iovs []unix.Iovec
		var total uint64
		for _, b := range bufs {
			iovs = append(iovs, unix.Iovec{Base: &b[0], Len: uint64(len(b))})
			total += uint64(len(b))
		}
		r.AddReadv(0, off, total, nil, nil, iovs)
	}
	cs, err := r.Wait(nil, 1)
	if err != nil || len(cs) != 1 {
		t.Fatalf("Wait: %v, %v", cs, err)
	}
	return cs[0]
}

func testObject(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7 / 5)
	}
	return b
}

func TestReadRanges(t *testing.T) {
	s := s3test.NewStore(t)
	obj := testObject(100 << 10)
	s.SetObject(checkpointfiles.PagesFileName, obj)
	r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true}), checkpointfiles.PagesFileName)

	buf := make([]byte, 64<<10)
	if c := read(t, r, 4096, buf); c.N != uint64(len(buf)) || c.Err != nil || !bytes.Equal(buf, obj[4096:4096+len(buf)]) {
		t.Errorf("read of 64 KiB at 4 KiB: %d bytes, %v; want all bytes, nil", c.N, c.Err)
	}
	a, b := make([]byte, 8<<10), make([]byte, 8<<10)
	if c := read(t, r, 0, a, b); c.N != 16<<10 || c.Err != nil || !bytes.Equal(append(a, b...), obj[:16<<10]) {
		t.Errorf("readv of 2 x 8 KiB at 0: %d bytes, %v; want all bytes, nil", c.N, c.Err)
	}
	// Past the end of the object, reads return io.EOF, as from a file.
	buf = make([]byte, 64<<10)
	if c := read(t, r, int64(len(obj))-4096, buf); c.N != 4096 || c.Err != io.EOF || !bytes.Equal(buf[:4096], obj[len(obj)-4096:]) {
		t.Errorf("read across the end: %d bytes, %v; want 4096 bytes, EOF", c.N, c.Err)
	}
	if c := read(t, r, int64(len(obj)), buf); c.N != 0 || c.Err != io.EOF {
		t.Errorf("read at the end: %d bytes, %v; want 0 bytes, EOF", c.N, c.Err)
	}
}

// TestPagesReadParallelReads checks that a reader of the pages file says it
// serves as many reads at once as it reads with, each on a connection of its
// own.
func TestPagesReadParallelReads(t *testing.T) {
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, testObject(4096))
	r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, PagesFileReadParallel: 12}), checkpointfiles.PagesFileName)
	pr, ok := r.(stateio.ParallelAsyncReader)
	if !ok {
		t.Fatalf("the pages file's reader, %T, does not say how many reads it serves at once", r)
	}
	if got := pr.ParallelReads(); got != 12 || r.MaxParallel() != 12 {
		t.Errorf("ParallelReads = %d, MaxParallel = %d; want 12 and 12", got, r.MaxParallel())
	}
}

// TestReadStopsAtTheEnd checks that once the store has said where an object
// ends, in the Content-Range of an answer to a GET, a read at or past its end
// ends there, without a GET the store would refuse, as a file's reader reads
// ahead of its end; and that a read across it is answered as before.
func TestReadStopsAtTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		// first is where the read that tells the reader the object's size
		// begins: within it, answered with its part, or past it, refused.
		first int64
	}{
		{name: "told by a part", first: 0},
		{name: "told by a refusal", first: 200 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := s3test.NewStore(t)
			obj := testObject(100 << 10)
			s.SetObject(checkpointfiles.StateFileName, obj)
			r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true}), checkpointfiles.StateFileName)
			buf := make([]byte, 64<<10)
			read(t, r, tc.first, buf)
			asked := len(s.Ops())
			for _, off := range []int64{int64(len(obj)), 128 << 10, 1 << 20} {
				if c := read(t, r, off, buf); c.N != 0 || c.Err != io.EOF {
					t.Errorf("read at %d: %d bytes, %v; want 0 bytes, EOF", off, c.N, c.Err)
				}
			}
			if got := len(s.Ops()); got != asked {
				t.Errorf("%d requests for reads past the end, want none", got-asked)
			}
			// Across the end, a GET of what is left.
			if c := read(t, r, int64(len(obj))-4096, buf); c.N != 4096 || c.Err != io.EOF || !bytes.Equal(buf[:4096], obj[len(obj)-4096:]) {
				t.Errorf("read across the end: %d bytes, %v; want 4096 bytes, EOF", c.N, c.Err)
			}
		})
	}
}

func TestReadRetriesServerErrors(t *testing.T) {
	s := s3test.NewStore(t)
	obj := testObject(64 << 10)
	s.SetObject(checkpointfiles.PagesFileName, obj)
	s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
		switch {
		case op == "GetObject" && n == 0:
			s3test.WriteError(w, http.StatusServiceUnavailable, "SlowDown")
			return true
		case op == "GetObject" && n == 1:
			// The connection resets before the response.
			s3test.ResetConnection(t, w, "")
			return true
		}
		return false
	}
	r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true}), checkpointfiles.PagesFileName)
	buf := make([]byte, len(obj))
	if c := read(t, r, 0, buf); c.N != uint64(len(obj)) || c.Err != nil || !bytes.Equal(buf, obj) {
		t.Errorf("read: %d bytes, %v; want all bytes, nil", c.N, c.Err)
	}
	if got := len(s.Ops()); got != 3 {
		t.Errorf("%d requests, want 3", got)
	}
}

func TestReadResumesShortBodies(t *testing.T) {
	s := s3test.NewStore(t)
	obj := testObject(64 << 10)
	s.SetObject(checkpointfiles.PagesFileName, obj)
	s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
		if op != "GetObject" || n != 0 {
			return false
		}
		// The connection resets after 10000 bytes of the body.
		s3test.ResetConnection(t, w, fmt.Sprintf("HTTP/1.1 206 Partial Content\r\nContent-Length: %d\r\nContent-Range: bytes 0-%d/%d\r\n\r\n%s", len(obj), len(obj)-1, len(obj), obj[:10000]))
		return true
	}
	r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true}), checkpointfiles.PagesFileName)
	a, b := make([]byte, 32<<10), make([]byte, 32<<10)
	if c := read(t, r, 0, a, b); c.N != uint64(len(obj)) || c.Err != nil || !bytes.Equal(append(a, b...), obj) {
		t.Errorf("readv: %d bytes, %v; want all bytes, nil", c.N, c.Err)
	}
	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if got, want := reqs[1].Range, fmt.Sprintf("bytes=10000-%d", len(obj)-1); got != want {
		t.Errorf("second request's range = %q, want %q", got, want)
	}
}

func TestReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		code     string
		want     error
		attempts int
	}{
		{name: "missing object", status: http.StatusNotFound, code: "NoSuchKey", want: unix.ENOENT, attempts: 1},
		{name: "access denied", status: http.StatusForbidden, code: "AccessDenied", want: unix.EACCES, attempts: 1},
		{name: "server errors", status: http.StatusInternalServerError, code: "InternalError", want: unix.EIO, attempts: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := s3test.NewStore(t)
			s.SetObject(checkpointfiles.PagesFileName, testObject(4096))
			s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
				s3test.WriteError(w, tc.status, tc.code)
				return true
			}
			r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, MaxAttempts: 3}), checkpointfiles.PagesFileName)
			c := read(t, r, 0, make([]byte, 4096))
			if c.N != 0 || !errors.Is(c.Err, tc.want) {
				t.Errorf("read: %d bytes, %v; want 0 bytes, %v", c.N, c.Err, tc.want)
			}
			if got := len(s.Ops()); got != tc.attempts {
				t.Errorf("%d requests, want %d", got, tc.attempts)
			}
		})
	}
}

func TestReadDeadline(t *testing.T) {
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, testObject(4096))
	stalled := make(chan struct{})
	t.Cleanup(func() { close(stalled) })
	s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
		// The store stalls.
		select {
		case <-r.Context().Done():
		case <-stalled:
		}
		return true
	}
	const timeout = 200 * time.Millisecond
	r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, RequestTimeout: timeout}), checkpointfiles.PagesFileName)
	start := time.Now()
	c := read(t, r, 0, make([]byte, 4096))
	if c.Err == nil {
		t.Errorf("read from a stalled store succeeded")
	}
	if took := time.Since(start); took > timeout+time.Second {
		t.Errorf("read from a stalled store failed after %v, want about %v", took, timeout)
	}
}

func TestReadWaitOr(t *testing.T) {
	s := s3test.NewStore(t)
	r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true}), checkpointfiles.PagesFileName)
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	cs, err := r.(stateio.WaitOrAsyncReader).WaitOr(nil, wake)
	if len(cs) != 0 || err != nil {
		t.Errorf("WaitOr with a wake pending = %v, %v; want no completions, nil", cs, err)
	}
}

// awayFor returns a Store fault that fails the first n GetObjects as a store
// that is away does, its connections reset before any response.
func awayFor(t *testing.T, n int) func(w http.ResponseWriter, r *http.Request, op string, i int) bool {
	return func(w http.ResponseWriter, r *http.Request, op string, i int) bool {
		if op != "GetObject" || i >= n {
			return false
		}
		s3test.ResetConnection(t, w, "")
		return true
	}
}

func TestPagesReadRetriesWhileRunning(t *testing.T) {
	for _, tc := range []struct {
		name         string
		whileRunning bool
		path         string
		want         error
		attempts     int
	}{
		// Away for longer than MaxAttempts: the pages file's reads wait for it.
		{name: "pages file", whileRunning: true, path: checkpointfiles.PagesFileName, attempts: 11},
		{name: "a layer's pages file", whileRunning: true, path: checkpointimage.LayerPath(checkpointimage.Digest{1}, checkpointfiles.PagesFileName), attempts: 11},
		// Without PagesRetryWhileRunning, or of another file, they fail as before.
		{name: "pages file, as before", path: checkpointfiles.PagesFileName, want: unix.EIO, attempts: 3},
		{name: "state file", whileRunning: true, path: checkpointfiles.StateFileName, want: unix.EIO, attempts: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := s3test.NewStore(t)
			obj := testObject(64 << 10)
			s.SetObject(tc.path, obj)
			s.Fault = awayFor(t, 10)
			r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, MaxAttempts: 3, PagesRetryWhileRunning: tc.whileRunning}), tc.path)
			buf := make([]byte, len(obj))
			c := read(t, r, 0, buf)
			if tc.want == nil && (c.N != uint64(len(obj)) || c.Err != nil || !bytes.Equal(buf, obj)) {
				t.Errorf("read: %d bytes, %v; want all bytes, nil", c.N, c.Err)
			}
			if tc.want != nil && (c.N != 0 || !errors.Is(c.Err, tc.want)) {
				t.Errorf("read: %d bytes, %v; want 0 bytes, %v", c.N, c.Err, tc.want)
			}
			if got := len(s.Ops()); got != tc.attempts {
				t.Errorf("%d requests, want %d", got, tc.attempts)
			}
		})
	}
}

// TestPagesReadRetrying checks that a reader of a pages file read for as long
// as the sandbox runs says it retries while one of its reads waits for the
// store, and not before, nor once the read delivered: one that is away, from
// its first attempt on, whatever MaxAttempts says; one that accepts requests
// and never answers them, once pagesRetryWatchdogTimeout has passed; and one
// that is slow to begin to answer, or stops in the middle of its answer, for
// longer than that, though it then delivers, its GET going on rather than cut.
func TestPagesReadRetrying(t *testing.T) {
	watchdog := pagesRetryWatchdogTimeout
	pagesRetryWatchdogTimeout = 200 * time.Millisecond
	t.Cleanup(func() { pagesRetryWatchdogTimeout = watchdog })
	const stall = 5 * 200 * time.Millisecond
	for _, tc := range []struct {
		name string
		// fault is the store's, until back is closed; it answers as it
		// does by default once fault returns false.
		fault func(t *testing.T, w http.ResponseWriter, r *http.Request, back <-chan struct{}) bool
		// gets is the most requests the store may have had once the
		// reader says it retries, if positive.
		gets int
	}{
		{
			name: "away",
			fault: func(t *testing.T, w http.ResponseWriter, r *http.Request, back <-chan struct{}) bool {
				s3test.ResetConnection(t, w, "")
				return true
			},
			gets: 2,
		},
		{
			name: "silent",
			fault: func(t *testing.T, w http.ResponseWriter, r *http.Request, back <-chan struct{}) bool {
				select {
				case <-r.Context().Done():
					return true
				case <-back:
					// Back as it waited: it answers.
					return false
				}
			},
		},
		{
			name: "slow to answer",
			fault: func(t *testing.T, w http.ResponseWriter, r *http.Request, back <-chan struct{}) bool {
				// It answers, once the test saw the read retrying.
				<-back
				return false
			},
			gets: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := s3test.NewStore(t)
			obj := testObject(64 << 10)
			s.SetObject(checkpointfiles.PagesFileName, obj)
			back := make(chan struct{})
			s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
				select {
				case <-back:
					return false
				default:
				}
				return tc.fault(t, w, r, back)
			}
			// MaxAttempts and RequestTimeout as by default, which the
			// client's own attempts of a GET would take tens of seconds of.
			ar := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, MaxAttempts: defaultMaxAttempts, PagesRetryWhileRunning: true}), checkpointfiles.PagesFileName)
			r, ok := ar.(stateio.RetryingAsyncReader)
			if !ok {
				t.Fatalf("the pages file's reader, %T, does not say whether it retries", ar)
			}
			if r.Retrying() {
				t.Errorf("Retrying before any read: got true, want false")
			}
			buf := make([]byte, len(obj))
			done := make(chan stateio.Completion, 1)
			start := time.Now()
			go func() { done <- read(t, ar, 0, buf) }()
			deadline := start.Add(10 * time.Second)
			for !r.Retrying() {
				if time.Now().After(deadline) {
					t.Fatalf("Retrying while the store does not answer: got false for 10 s, want true")
				}
				time.Sleep(time.Millisecond)
			}
			if took := time.Since(start); took > stall {
				t.Errorf("Retrying turned true %v after the read began, want within %v", took, stall)
			}
			if got := len(s.Ops()); tc.gets > 0 && got > tc.gets {
				t.Errorf("Retrying turned true after %d requests, want at most %d", got, tc.gets)
			}
			close(back)
			if c := <-done; c.N != uint64(len(obj)) || c.Err != nil || !bytes.Equal(buf, obj) {
				t.Errorf("read: %d bytes, %v; want all bytes, nil", c.N, c.Err)
			}
			if r.Retrying() {
				t.Errorf("Retrying once the read delivered: got true, want false")
			}
		})
	}
}

// TestPagesReadRetryingMidBody checks that a reader of a pages file read for
// as long as the sandbox runs says it retries while the store, having begun to
// answer one of its reads, sends no byte of it for longer than
// pagesRetryWatchdogTimeout, and no longer once it sends the rest, which the
// read delivers.
func TestPagesReadRetryingMidBody(t *testing.T) {
	watchdog := pagesRetryWatchdogTimeout
	pagesRetryWatchdogTimeout = 200 * time.Millisecond
	t.Cleanup(func() { pagesRetryWatchdogTimeout = watchdog })
	obj := testObject(64 << 10)
	stalled := make(chan struct{})
	resume := make(chan struct{})
	// A store of its own, which sends half of the object, stalls, then sends
	// the rest.
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(obj)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(obj)-1, len(obj)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(obj[:len(obj)/2])
		w.(http.Flusher).Flush()
		close(stalled)
		<-resume
		w.Write(obj[len(obj)/2:])
	}))
	t.Cleanup(hs.Close)
	srv, err := NewFileServer(context.Background(), &FileServerOptions{
		Endpoint:               hs.URL,
		Bucket:                 s3test.Bucket,
		UsePathStyle:           true,
		Credentials:            credentials.NewStaticCredentialsProvider("key", "secret", ""),
		AllowCheckpointReads:   true,
		PagesRetryWhileRunning: true,
	})
	if err != nil {
		t.Fatalf("NewFileServer: %v", err)
	}
	t.Cleanup(srv.Destroy)
	ar := openRead(t, srv, checkpointfiles.PagesFileName)
	r := ar.(stateio.RetryingAsyncReader)
	buf := make([]byte, len(obj))
	done := make(chan stateio.Completion, 1)
	go func() { done <- read(t, ar, 0, buf) }()
	<-stalled
	deadline := time.Now().Add(10 * time.Second)
	for !r.Retrying() {
		if time.Now().After(deadline) {
			t.Fatalf("Retrying while the store sends nothing of its answer: got false for 10 s, want true")
		}
		time.Sleep(time.Millisecond)
	}
	close(resume)
	if c := <-done; c.N != uint64(len(obj)) || c.Err != nil || !bytes.Equal(buf, obj) {
		t.Errorf("read: %d bytes, %v; want all bytes, nil", c.N, c.Err)
	}
	if r.Retrying() {
		t.Errorf("Retrying once the read delivered: got true, want false")
	}
}

func TestPagesReadRetriesStalledStoreWhileRunning(t *testing.T) {
	s := s3test.NewStore(t)
	obj := testObject(4096)
	s.SetObject(checkpointfiles.PagesFileName, obj)
	s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
		if n >= 2 {
			return false
		}
		// The store stalls, past each attempt's time.
		<-r.Context().Done()
		return true
	}
	r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, MaxAttempts: 1, RequestTimeout: 100 * time.Millisecond, PagesRetryWhileRunning: true}), checkpointfiles.PagesFileName)
	buf := make([]byte, len(obj))
	if c := read(t, r, 0, buf); c.N != uint64(len(obj)) || c.Err != nil || !bytes.Equal(buf, obj) {
		t.Errorf("read: %d bytes, %v; want all bytes, nil", c.N, c.Err)
	}
	if got := len(s.Ops()); got != 3 {
		t.Errorf("%d requests, want 3", got)
	}
}

func TestPagesReadFailuresWhileRunning(t *testing.T) {
	// What a request is refused for recurs: it fails at once, as before.
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{name: "missing object", status: http.StatusNotFound, code: "NoSuchKey", want: unix.ENOENT},
		{name: "access denied", status: http.StatusForbidden, code: "AccessDenied", want: unix.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := s3test.NewStore(t)
			s.SetObject(checkpointfiles.PagesFileName, testObject(4096))
			s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
				s3test.WriteError(w, tc.status, tc.code)
				return true
			}
			r := openRead(t, newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, PagesRetryWhileRunning: true}), checkpointfiles.PagesFileName)
			if c := read(t, r, 0, make([]byte, 4096)); c.N != 0 || !errors.Is(c.Err, tc.want) {
				t.Errorf("read: %d bytes, %v; want 0 bytes, %v", c.N, c.Err, tc.want)
			}
			if got := len(s.Ops()); got != 1 {
				t.Errorf("%d requests, want 1", got)
			}
		})
	}
}

func TestPagesReadWhileRunningEndsWithReader(t *testing.T) {
	// Each read waits for an hour, as near as makes no matter, before it tries
	// again.
	defer func(first, most time.Duration) { pagesRetryFirstBackoff, pagesRetryMaxBackoff = first, most }(pagesRetryFirstBackoff, pagesRetryMaxBackoff)
	pagesRetryFirstBackoff, pagesRetryMaxBackoff = time.Hour, time.Hour
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, testObject(4096))
	// The store is away for good.
	s.Fault = awayFor(t, math.MaxInt)
	r, err := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, PagesRetryWhileRunning: true}).OpenRead(checkpointfiles.PagesFileName)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	r.AddRead(0, 0, nil, memmap.FileRange{}, make([]byte, 4096))
	time.Sleep(100 * time.Millisecond)
	// Its reader closed, as the sandbox's end closes it, the read ends,
	// waiting for no backoff.
	start := time.Now()
	r.Close()
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %v while a read retried, want under a second", took)
	}
	cs, err := r.Wait(nil, 1)
	if err != nil || len(cs) != 1 || cs[0].Err == nil {
		t.Errorf("Wait after Close = %v, %v; want the read failed", cs, err)
	}
}

func TestParsePagesRetry(t *testing.T) {
	for _, tc := range []struct {
		value        string
		whileRunning bool
		err          bool
	}{
		{value: ""},
		{value: "while-running", whileRunning: true},
		{value: "forever", err: true},
	} {
		whileRunning, err := ParsePagesRetry(tc.value)
		if whileRunning != tc.whileRunning || (err != nil) != tc.err {
			t.Errorf("ParsePagesRetry(%q) = %t, %v; want %t, an error %t", tc.value, whileRunning, err, tc.whileRunning, tc.err)
		}
	}
}

func TestBackoffBefore(t *testing.T) {
	defer func(first, most time.Duration) { pagesRetryFirstBackoff, pagesRetryMaxBackoff = first, most }(pagesRetryFirstBackoff, pagesRetryMaxBackoff)
	pagesRetryFirstBackoff, pagesRetryMaxBackoff = time.Second, 10*time.Second
	for failures := 1; failures <= 70; failures++ {
		ceiling := min(10*time.Second, time.Second<<min(failures-1, 31))
		for range 100 {
			if d := backoffBefore(failures); d <= 0 || d > ceiling {
				t.Fatalf("backoffBefore(%d) = %v, want in (0, %v]", failures, d, ceiling)
			}
		}
	}
}

// write writes chunks to w in order and finalizes it.
func write(t *testing.T, w stateio.AsyncWriter, chunks ...[]byte) error {
	for _, chunk := range chunks {
		w.AddWrite(0, nil, memmap.FileRange{}, chunk)
		cs, err := w.Wait(nil, 1)
		if err != nil || len(cs) != 1 {
			t.Fatalf("Wait: %v, %v", cs, err)
		}
		if cs[0].Err != nil {
			return cs[0].Err
		}
		if cs[0].N != uint64(len(chunk)) {
			t.Fatalf("write of %d bytes wrote %d", len(chunk), cs[0].N)
		}
	}
	return w.Finalize()
}

// chunked splits b into chunks of varying sizes.
func chunked(b []byte) [][]byte {
	var chunks [][]byte
	for i := 1; len(b) != 0; i++ {
		n := min(len(b), 1000*i)
		chunks = append(chunks, b[:n])
		b = b[n:]
	}
	return chunks
}

func TestWriteSmallObject(t *testing.T) {
	s := s3test.NewStore(t)
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointWrites: true})
	w, err := srv.OpenWrite(checkpointfiles.StateFileName)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	obj := testObject(100 << 10)
	if err := write(t, w, chunked(obj)...); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if got, _ := s.Object(checkpointfiles.StateFileName); !bytes.Equal(got, obj) {
		t.Errorf("object differs from what was written")
	}
	if got, want := s.Ops(), []string{"PutObject"}; !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func TestWriteMultipart(t *testing.T) {
	s := s3test.NewStore(t)
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointWrites: true})
	obj := object{bucket: s3test.Bucket, key: "big"}
	const partBytes = 64 << 10
	w := newWriter(srv.ctx, srv.client, obj, srv.retry, 32<<10, 1, 4, partBytes, 2)
	data := testObject(4*partBytes + 1000)
	if err := write(t, w, chunked(data)...); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if got, _ := s.Object(obj.key); !bytes.Equal(got, data) {
		t.Errorf("object differs from what was written")
	}
	var parts []int
	for _, req := range s.Requests() {
		if req.Op == "UploadPart" {
			parts = append(parts, req.Bytes)
		}
	}
	// Parts are uploaded in parallel.
	slices.Sort(parts)
	if want := []int{1000, partBytes, partBytes, partBytes, partBytes}; !slices.Equal(parts, want) {
		t.Errorf("parts of %v bytes, want %v", parts, want)
	}
	if inProgress, aborted := s.Uploads(); inProgress != 0 || aborted != 0 {
		t.Errorf("%d uploads left, %d aborted; want none", inProgress, aborted)
	}
}

func TestWriteAbortsFailedUploads(t *testing.T) {
	for _, tc := range []struct {
		name string
		// failPart is the number of the part whose upload fails.
		failPart int
		// finalize is true if Finalize is called.
		finalize bool
	}{
		{name: "failed part", failPart: 1, finalize: true},
		{name: "failed last part", failPart: 2, finalize: true},
		{name: "not finalized", failPart: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := s3test.NewStore(t)
			s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
				if op == "UploadPart" && n == tc.failPart {
					s3test.WriteError(w, http.StatusForbidden, "AccessDenied")
					return true
				}
				return false
			}
			srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointWrites: true})
			obj := object{bucket: s3test.Bucket, key: "big"}
			const partBytes = 64 << 10
			w := newWriter(srv.ctx, srv.client, obj, srv.retry, 32<<10, 1, 4, partBytes, 2)
			data := testObject(2*partBytes + 1000)
			if tc.finalize {
				if err := write(t, w, chunked(data)...); !errors.Is(err, unix.EACCES) {
					t.Errorf("writing with a failed part: %v, want EACCES", err)
				}
			} else {
				for _, chunk := range chunked(data[:2*partBytes]) {
					w.AddWrite(0, nil, memmap.FileRange{}, chunk)
					if _, err := w.Wait(nil, 1); err != nil {
						t.Fatalf("Wait: %v", err)
					}
				}
			}
			if err := w.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if inProgress, aborted := s.Uploads(); inProgress != 0 || aborted != 1 {
				t.Errorf("%d uploads left, %d aborted; want none left, 1 aborted", inProgress, aborted)
			}
			if _, ok := s.Object(obj.key); ok {
				t.Errorf("object created from a failed upload")
			}
		})
	}
}

func TestOpenPermissions(t *testing.T) {
	layerPages := checkpointimage.LayerPath(checkpointimage.Digest{1}, checkpointfiles.PagesFileName)
	layerMeta := checkpointimage.LayerPath(checkpointimage.Digest{1}, checkpointfiles.PagesMetadataFileName)
	for _, tc := range []struct {
		opts  FileServerOptions
		path  string
		write bool
		ok    bool
	}{
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: checkpointfiles.StateFileName, ok: true},
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: checkpointfiles.PagesFileName, ok: true},
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: layerMeta, ok: true},
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: layerPages, ok: true},
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: checkpointimage.LayerPath(checkpointimage.Digest{1}, checkpointfiles.StateFileName)},
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: checkpointfiles.StateFileName, write: true},
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: checkpointfiles.FSCheckpointManifestFileName},
		{opts: FileServerOptions{AllowCheckpointReads: true}, path: "other"},
		{opts: FileServerOptions{AllowFSCheckpointReads: true}, path: checkpointfiles.FSCheckpointMultiTarFileName, ok: true},
		{opts: FileServerOptions{AllowFSCheckpointReads: true}, path: checkpointfiles.StateFileName},
		{opts: FileServerOptions{AllowCheckpointWrites: true}, path: checkpointfiles.PagesFileName, write: true, ok: true},
		{opts: FileServerOptions{AllowCheckpointWrites: true}, path: layerPages, write: true},
		{opts: FileServerOptions{AllowCheckpointWrites: true}, path: checkpointfiles.PagesFileName},
		{opts: FileServerOptions{AllowFSCheckpointWrites: true}, path: checkpointfiles.FSCheckpointManifestFileName, write: true, ok: true},
		{opts: FileServerOptions{AllowFSCheckpointWrites: true}, path: checkpointfiles.StateFileName, write: true},
	} {
		srv := newTestFileServer(t, s3test.NewStore(t), tc.opts)
		var c io.Closer
		var err error
		if tc.write {
			c, err = srv.OpenWrite(tc.path)
		} else {
			c, err = srv.OpenRead(tc.path)
		}
		if err == nil {
			c.Close()
		}
		if tc.ok != (err == nil) || (err != nil && !errors.Is(err, fs.ErrPermission)) {
			t.Errorf("%+v: opening %q (write %t): %v, want success %t", tc.opts, tc.path, tc.write, err, tc.ok)
		}
	}
}

// TestLayersPrefix checks where the files of an image's layers are read from:
// under the object prefix, in layers/<digest>/, or, with a layers prefix, under
// it, in <digest>/; the image's own files are under the object prefix either
// way.
func TestLayersPrefix(t *testing.T) {
	d := checkpointimage.Digest{1}
	layerMeta := checkpointimage.LayerPath(d, checkpointfiles.PagesMetadataFileName)
	layerPages := checkpointimage.LayerPath(d, checkpointfiles.PagesFileName)
	for _, tc := range []struct {
		layersPrefix string
		// keys holds the key of the object read for each file.
		keys map[string]string
	}{
		{
			keys: map[string]string{
				checkpointfiles.StateFileName: "image/" + checkpointfiles.StateFileName,
				layerMeta:                     "image/" + layerMeta,
				layerPages:                    "image/" + layerPages,
			},
		},
		{
			layersPrefix: "workspace/layers/",
			keys: map[string]string{
				checkpointfiles.StateFileName: "image/" + checkpointfiles.StateFileName,
				layerMeta:                     "workspace/layers/" + d.String() + "/" + checkpointfiles.PagesMetadataFileName,
				layerPages:                    "workspace/layers/" + d.String() + "/" + checkpointfiles.PagesFileName,
			},
		},
	} {
		s := s3test.NewStore(t)
		for _, key := range tc.keys {
			s.SetObject(key, []byte(key))
		}
		srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, ObjectPrefix: "image/", LayersPrefix: tc.layersPrefix})
		for path, key := range tc.keys {
			buf := make([]byte, len(key))
			if c := read(t, openRead(t, srv, path), 0, buf); c.N != uint64(len(key)) || string(buf) != key {
				t.Errorf("layers prefix %q: reading %q got %q (%d bytes, %v), want the object %q", tc.layersPrefix, path, buf[:c.N], c.N, c.Err, key)
			}
		}
	}
}

// socketPath returns the path of a Unix domain socket in a new directory,
// short enough for a socket's address.
func socketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "s3-socket")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "store.sock")
}

// serveOnSocket serves s on a new Unix domain socket at path until the server
// it returns is closed, or the test ends.
func serveOnSocket(t *testing.T, s *s3test.Store, path string) *http.Server {
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	hs := &http.Server{Handler: s}
	go hs.Serve(l)
	t.Cleanup(func() { hs.Close() })
	return hs
}

// newSocketFileServer returns a FileServer for the store served on the socket
// at path, at an endpoint whose host does not resolve.
func newSocketFileServer(t *testing.T, path string, opts FileServerOptions) *FileServer {
	opts.Endpoint = "http://store.invalid"
	opts.Socket = path
	opts.Bucket = s3test.Bucket
	opts.UsePathStyle = true
	opts.Credentials = credentials.NewStaticCredentialsProvider("key", "secret", "")
	opts.MaxAttempts = 3
	srv, err := NewFileServer(context.Background(), &opts)
	if err != nil {
		t.Fatalf("NewFileServer: %v", err)
	}
	t.Cleanup(srv.Destroy)
	return srv
}

// TestSocket checks that a FileServer with a Socket reads and writes the store
// served on it, whatever its Endpoint's host, and not through a proxy.
func TestSocket(t *testing.T) {
	s := s3test.NewStore(t)
	path := socketPath(t)
	serveOnSocket(t, s, path)
	srv := newSocketFileServer(t, path, FileServerOptions{AllowCheckpointReads: true, AllowCheckpointWrites: true})
	obj := testObject(64 << 10)
	w, err := srv.OpenWrite(checkpointfiles.StateFileName)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if err := write(t, w, chunked(obj)...); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, ok := s.Object(checkpointfiles.StateFileName); !ok || !bytes.Equal(got, obj) {
		t.Fatalf("the store holds %d bytes (found %t), want the %d written", len(got), ok, len(obj))
	}
	buf := make([]byte, len(obj))
	if c := read(t, openRead(t, srv, checkpointfiles.StateFileName), 0, buf); c.N != uint64(len(obj)) || c.Err != nil || !bytes.Equal(buf, obj) {
		t.Errorf("read: %d bytes, %v; want all bytes, nil", c.N, c.Err)
	}
	if tr := newTransport(path, 1); tr.Proxy != nil {
		t.Errorf("the transport of a socket has a proxy")
	}
}

// TestSocketComesBack checks that the reads of the pages file of a FileServer
// with a Socket and PagesRetryWhileRunning wait for the store while nothing
// serves its socket, as when the process serving it restarts, and continue
// once a new socket at its path does.
func TestSocketComesBack(t *testing.T) {
	for _, whileRunning := range []bool{true, false} {
		t.Run(fmt.Sprintf("while running %t", whileRunning), func(t *testing.T) {
			s := s3test.NewStore(t)
			obj := testObject(64 << 10)
			s.SetObject(checkpointfiles.PagesFileName, obj)
			path := socketPath(t)
			first := serveOnSocket(t, s, path)
			r := openRead(t, newSocketFileServer(t, path, FileServerOptions{AllowCheckpointReads: true, PagesRetryWhileRunning: whileRunning}), checkpointfiles.PagesFileName)
			buf := make([]byte, len(obj)/2)
			if c := read(t, r, 0, buf); c.Err != nil {
				t.Fatalf("read before the store went away: %v", c.Err)
			}
			// The store goes away with its socket, and comes back on a new one
			// after its reader's attempts, as many as MaxAttempts says, failed.
			first.Close()
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the socket of a closed listener remains: %v", err)
			}
			done := make(chan stateio.Completion, 1)
			go func() { done <- read(t, r, int64(len(buf)), buf) }()
			var c stateio.Completion
			select {
			case c = <-done:
			case <-time.After(time.Second):
				serveOnSocket(t, s, path)
				c = <-done
			}
			if whileRunning && (c.N != uint64(len(buf)) || c.Err != nil || !bytes.Equal(buf, obj[len(buf):])) {
				t.Errorf("read: %d bytes, %v; want all bytes, nil", c.N, c.Err)
			}
			if !whileRunning && !errors.Is(c.Err, unix.EIO) {
				t.Errorf("read: %d bytes, %v; want 0 bytes, %v", c.N, c.Err, unix.EIO)
			}
		})
	}
}

// TestNewFileServerSocket checks which Sockets NewFileServer accepts.
func TestNewFileServerSocket(t *testing.T) {
	path := socketPath(t)
	serveOnSocket(t, s3test.NewStore(t), path)
	file := filepath.Join(filepath.Dir(path), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for _, tc := range []struct {
		name     string
		socket   string
		endpoint string
		ok       bool
	}{
		{name: "socket", socket: path, endpoint: "http://store", ok: true},
		{name: "no endpoint", socket: path},
		{name: "relative path", socket: filepath.Base(path), endpoint: "http://store"},
		{name: "missing", socket: path + ".missing", endpoint: "http://store"},
		{name: "not a socket", socket: file, endpoint: "http://store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "relative path" {
				t.Chdir(filepath.Dir(path))
			}
			srv, err := NewFileServer(context.Background(), &FileServerOptions{
				Endpoint:    tc.endpoint,
				Socket:      tc.socket,
				Bucket:      s3test.Bucket,
				Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""),
			})
			if err == nil {
				srv.Destroy()
			}
			if (err == nil) != tc.ok {
				t.Errorf("NewFileServer with socket %q, endpoint %q: %v, want success %t", tc.socket, tc.endpoint, err, tc.ok)
			}
		})
	}
}

// TestNewFileServerErrorsHoldNoCredentials checks that an error about the
// options does not print the credentials, which would reach the logs.
func TestNewFileServerErrorsHoldNoCredentials(t *testing.T) {
	const secret = "secret-access-key"
	_, err := NewFileServer(context.Background(), &FileServerOptions{
		Bucket:      s3test.Bucket,
		Credentials: credentials.NewStaticCredentialsProvider("key", secret, "session-token"),
		MaxAttempts: -1,
	})
	if err == nil {
		t.Fatalf("NewFileServer with negative MaxAttempts succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, secret) || strings.Contains(msg, "session-token") {
		t.Errorf("NewFileServer error holds the credentials: %v", err)
	}
}

// TestCheckFileMode checks which owners and permissions of options and
// credentials files the checkpoint gofer accepts.
func TestCheckFileMode(t *testing.T) {
	const user = 1000
	for _, tc := range []struct {
		perm   fs.FileMode
		owner  uint32
		secret bool
		ok     bool
	}{
		{perm: 0o600, owner: user, secret: true, ok: true},
		{perm: 0o400, owner: 0, secret: true, ok: true},
		{perm: 0o644, owner: user, ok: true},
		{perm: 0o644, owner: user, secret: true},
		{perm: 0o640, owner: user, secret: true},
		{perm: 0o604, owner: user, secret: true},
		{perm: 0o620, owner: user},
		{perm: 0o602, owner: user},
		{perm: 0o600, owner: user + 1, secret: true},
		{perm: 0o644, owner: user + 1},
	} {
		err := checkFileMode("file", tc.perm, tc.owner, user, tc.secret)
		if (err == nil) != tc.ok {
			t.Errorf("checkFileMode(mode %#o, owner %d, secret %t) = %v, want success %t", tc.perm, tc.owner, tc.secret, err, tc.ok)
		}
	}
}

// TestNewFileServerCredentialsFileMode checks that a credentials file that
// users other than its owner may read is refused.
func TestNewFileServerCredentialsFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("[default]\naws_access_key_id = key\naws_secret_access_key = secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for _, tc := range []struct {
		perm fs.FileMode
		ok   bool
	}{
		{perm: 0o600, ok: true},
		{perm: 0o644},
	} {
		if err := os.Chmod(path, tc.perm); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		srv, err := NewFileServer(context.Background(), &FileServerOptions{
			Endpoint:              "http://127.0.0.1:1",
			Bucket:                s3test.Bucket,
			SharedCredentialsFile: path,
		})
		if err == nil {
			srv.Destroy()
		}
		if (err == nil) != tc.ok {
			t.Errorf("NewFileServer with a credentials file of mode %#o: %v, want success %t", tc.perm, err, tc.ok)
		}
	}
}
