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
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
)

// errShortBody is returned by Reader.readOnce if a response's body ended
// before it delivered its bytes, as when its connection resets.
var errShortBody = errors.New("response body ended early")

// pagesRetryWatchdogTimeout is how long a GET of a read retried for as long as
// the sandbox runs (Reader.whileRunning) may go without the store beginning
// to answer, or sending a byte of its answer, before the read counts as
// retrying (Reader.Retrying), as when the store is away: the GET goes on,
// within its own deadline, and the read no longer counts once a byte comes. A
// store that accepts requests and never answers them is thus known as such
// within it, well within the timeouts that save/restore binaries are given
// (tens of seconds), which the time spent waiting for such a store does not
// count against (pkg/sentry/control's SaveRestoreExec); a store that is only
// slow still delivers. It is a variable for tests.
var pagesRetryWatchdogTimeout = 10 * time.Second

// Reader implements stateio.WaitOrAsyncReader for an S3 object, with one
// ranged GET per read.
type Reader struct {
	stateio.NoRegisterClientFD

	client       *s3.Client
	obj          object
	retry        retryPolicy
	whileRunning bool
	maxReadBytes uint64
	maxRanges    int
	subs         chan readSubmission
	cmps         chan stateio.Completion
	cancel       context.CancelCauseFunc
	workers      sync.WaitGroup

	// retrying is the number of reads that failed and have delivered no byte
	// since, as they wait to be made again (see read).
	retrying atomic.Int32

	// size is the object's size, once the store has said it in the
	// Content-Range of an answer to a GET, or -1 before: a read that would
	// begin at or past it then ends there, without a GET the store would
	// refuse (see read). It is kept for as long as the Reader is, as a
	// checkpoint's objects, its layers' among them, are written once and
	// never change while they are read.
	size atomic.Int64
}

type readSubmission struct {
	id    int
	off   int64
	total uint64
	dst   stateio.LocalClientRanges
}

// newReader returns a Reader that reads from obj. If whileRunning, its reads
// retry what may succeed later for as long as ctx lasts (see Reader.read).
func newReader(ctx context.Context, client *s3.Client, obj object, policy retryPolicy, whileRunning bool, maxReadBytes uint64, maxRanges, maxParallel int) *Reader {
	ctx, cancel := context.WithCancelCause(ctx)
	r := &Reader{
		client:       client,
		obj:          obj,
		retry:        policy,
		whileRunning: whileRunning,
		maxReadBytes: maxReadBytes,
		maxRanges:    maxRanges,
		subs:         make(chan readSubmission, maxParallel),
		cmps:         make(chan stateio.Completion, maxParallel),
		cancel:       cancel,
	}
	r.size.Store(-1)
	r.workers.Add(maxParallel)
	for range maxParallel {
		go r.workerMain(ctx)
	}
	return r
}

// readState is a read's part in its Reader's count of the reads that retry
// (Reader.retrying), and the watchdog of its GETs (pagesRetryWatchdogTimeout).
type readState struct {
	r *Reader

	mu sync.Mutex
	// retrying is true while the read counts in r.retrying.
	retrying bool
	// watched is true while a GET of the read is watched, for up to timeout
	// past last, the time of the store's last sign of answering.
	watched bool
	timeout time.Duration
	last    time.Time
	timer   *time.Timer
}

// set has the read count, or no longer count, as retrying.
func (s *readState) set(retrying bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(retrying)
}

// Preconditions: s.mu must be locked.
func (s *readState) setLocked(retrying bool) {
	if retrying == s.retrying {
		return
	}
	s.retrying = retrying
	if retrying {
		s.r.retrying.Add(1)
	} else {
		s.r.retrying.Add(-1)
	}
}

// watch begins to watch a GET of the read: if the store gives no sign of
// answering (progress) for timeout, the read counts as retrying, until it does.
func (s *readState) watch(timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watched = true
	s.timeout = timeout
	s.last = time.Now()
	if s.timer == nil {
		s.timer = time.AfterFunc(timeout, s.fire)
	} else {
		s.timer.Reset(timeout)
	}
}

// progress tells of a sign of the store answering the GET watched: its answer
// begun, or a byte of it.
func (s *readState) progress() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = time.Now()
	s.setLocked(false)
}

// unwatch ends the watch of the read's GET.
func (s *readState) unwatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watched = false
	if s.timer != nil {
		s.timer.Stop()
	}
}

// fire is the watchdog's: the read counts as retrying if the store gave no
// sign of answering for the watch's timeout, or else is watched on.
func (s *readState) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.watched {
		return
	}
	if left := s.timeout - time.Since(s.last); left > 0 {
		s.timer.Reset(left)
		return
	}
	s.setLocked(true)
}

// progressReader tells s of each byte it reads of the answer to a GET.
type progressReader struct {
	io.Reader
	s *readState
}

// Read implements io.Reader.Read.
func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.Reader.Read(b)
	if n > 0 {
		p.s.progress()
	}
	return n, err
}

// Retrying implements stateio.RetryingAsyncReader.Retrying: true while one of
// r's reads, of a pages file read for as long as the sandbox runs, failed as
// one that may succeed later does and has delivered no byte since.
func (r *Reader) Retrying() bool {
	return r.retrying.Load() > 0
}

// Close implements stateio.AsyncReader.Close.
func (r *Reader) Close() error {
	r.cancel(fmt.Errorf("context canceled by Reader.Close"))
	r.workers.Wait()
	return nil
}

// MaxReadBytes implements stateio.AsyncReader.MaxReadBytes.
func (r *Reader) MaxReadBytes() uint64 {
	return r.maxReadBytes
}

// MaxRanges implements stateio.AsyncReader.MaxRanges.
func (r *Reader) MaxRanges() int {
	return r.maxRanges
}

// MaxParallel implements stateio.AsyncReader.MaxParallel.
func (r *Reader) MaxParallel() int {
	return cap(r.subs)
}

// AddRead implements stateio.AsyncReader.AddRead.
func (r *Reader) AddRead(id int, off int64, _ stateio.DestinationFile, _ memmap.FileRange, dstMap []byte) {
	r.subs <- readSubmission{
		id:    id,
		off:   off,
		total: uint64(len(dstMap)),
		dst:   stateio.LocalClientMapping(dstMap),
	}
}

// AddReadv implements stateio.AsyncReader.AddReadv.
func (r *Reader) AddReadv(id int, off int64, total uint64, _ stateio.DestinationFile, _ []memmap.FileRange, dstMaps []unix.Iovec) {
	r.subs <- readSubmission{
		id:    id,
		off:   off,
		total: total,
		dst:   stateio.LocalClientMappings(dstMaps),
	}
}

// Wait implements stateio.AsyncReader.Wait.
func (r *Reader) Wait(cs []stateio.Completion, minCompletions int) ([]stateio.Completion, error) {
	return stateio.CompletionChanWait(r.cmps, cs, minCompletions)
}

// WaitOr implements stateio.WaitOrAsyncReader.WaitOr.
func (r *Reader) WaitOr(cs []stateio.Completion, wake <-chan struct{}) ([]stateio.Completion, error) {
	return stateio.CompletionChanWaitOr(r.cmps, cs, wake)
}

func (r *Reader) workerMain(ctx context.Context) {
	defer r.workers.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case sub := <-r.subs:
			n, err := r.read(ctx, &sub)
			r.cmps <- stateio.Completion{
				ID:  sub.id,
				N:   n,
				Err: err,
			}
		}
	}
}

// read reads the bytes of sub and returns their number. The client retries
// each GET until it gets a response; if the response's body ends early, read
// continues with a GET of the rest, until retry.maxAttempts GETs in a row
// deliver nothing.
//
// If r.whileRunning, a GET that fails as one may that would succeed later (its
// connection failing, its time running out, the store failing or throttling
// it) or that delivers nothing is made again after a backoff (backoffBefore),
// for as long as ctx lasts, rather than failing the read: a store that is away
// for a while delays the read, rather than failing it for good.
func (r *Reader) read(ctx context.Context, sub *readSubmission) (uint64, error) {
	var done uint64
	dst := sub.dst
	s := &readState{r: r}
	defer s.set(false)
	for failures := 0; ; {
		// A file's reader reads ahead of its end, as stateio.BufReader does
		// in parallel: once the store has said where the object ends, a read
		// past it ends there.
		if size := r.size.Load(); size >= 0 && sub.off+int64(done) >= size {
			return done, io.EOF
		}
		n, err := r.readOnce(ctx, sub.off+int64(done), sub.total-done, &dst, s)
		done += n
		if r.whileRunning && transient(err) {
			if n != 0 {
				failures = 0
				s.set(false)
			} else {
				failures++
				if failures == 1 {
					log.Warningf("s3.GetObject on %s failed, retried for as long as the sandbox runs: %v", r.obj, err)
				}
				s.set(true)
				select {
				case <-ctx.Done():
					return done, mapError(err, "GetObject", r.obj)
				case <-time.After(backoffBefore(failures)):
				}
			}
			dst = dst.DropFirst(n)
			continue
		}
		switch {
		case err == nil || err == io.EOF:
			return done, err
		case !errors.Is(err, errShortBody):
			return done, mapError(err, "GetObject", r.obj)
		}
		if n != 0 {
			failures = 0
		} else if failures++; failures == r.retry.maxAttempts {
			return done, mapError(err, "GetObject", r.obj)
		}
		dst = dst.DropFirst(n)
	}
}

// learnSize records the size of r's object that contentRange, the
// Content-Range of an answer to a GET ("bytes <first>-<last>/<size>", or
// "bytes */<size>"), says, if it says one.
func (r *Reader) learnSize(contentRange string) {
	_, total, ok := strings.Cut(contentRange, "/")
	if !ok {
		return
	}
	if size, err := strconv.ParseInt(total, 10, 64); err == nil && size >= 0 {
		r.size.Store(size)
	}
}

// pagesRetryFirstBackoff and pagesRetryMaxBackoff bound how long a read of
// Reader.whileRunning waits before it makes a GET again (backoffBefore). They
// are variables for tests.
var (
	pagesRetryFirstBackoff = time.Second
	pagesRetryMaxBackoff   = 10 * time.Second
)

// backoffBefore returns how long a read waits before its GET after failures
// GETs in a row delivered nothing: a random time up to a ceiling that is
// pagesRetryFirstBackoff, then twice as long each time, at most
// pagesRetryMaxBackoff ("full jitter"), so that the reads that failed
// together do not all come back at once.
func backoffBefore(failures int) time.Duration {
	ceiling := pagesRetryMaxBackoff
	if failures < 32 {
		ceiling = min(ceiling, pagesRetryFirstBackoff<<(failures-1))
	}
	return rand.N(ceiling) + 1
}

// transient returns true if err, of a GET, may not recur when the GET is made
// again: a short body, its attempt's time running out, or what the AWS SDK
// retries (connection errors, server errors, throttling), each after its
// client's own attempts. Errors of the request itself, as a missing object or
// a refused credential, recur, as does a GET cancelled as its reader closes.
func transient(err error) bool {
	switch {
	case err == nil || err == io.EOF:
		return false
	case errors.Is(err, errShortBody), errors.Is(err, context.DeadlineExceeded):
		return true
	}
	return retry.IsErrorRetryables(retry.DefaultRetryables).IsErrorRetryable(err) == aws.TrueTernary
}

// readOnce reads up to total bytes at off into dst with one GET. It returns
// io.EOF if the object ends before them. Its errors are the client's, which
// read maps.
func (r *Reader) readOnce(ctx context.Context, off int64, total uint64, dst *stateio.LocalClientRanges, s *readState) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.retry.deadline(total))
	defer cancel()
	var optFns []func(*s3.Options)
	if r.whileRunning {
		// Made once by the client, as the read retries it after a backoff of
		// its own; watched, so that the read counts as retrying while the
		// store neither begins to answer nor sends a byte.
		optFns = append(optFns, func(o *s3.Options) { o.RetryMaxAttempts = 1 })
		s.watch(pagesRetryWatchdogTimeout)
		defer s.unwatch()
	}
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.obj.bucket),
		Key:    aws.String(r.obj.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, off+int64(total)-1)),
	}, optFns...)
	if err != nil {
		var respErr *awshttp.ResponseError
		if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable {
			// off is at or past the end of the object, which the answer
			// says, as "bytes */<size>".
			if respErr.Response != nil {
				r.learnSize(respErr.Response.Header.Get("Content-Range"))
			}
			return 0, io.EOF
		}
		return 0, err
	}
	r.learnSize(aws.ToString(out.ContentRange))
	defer out.Body.Close()
	body := io.Reader(out.Body)
	if r.whileRunning {
		// The store began to answer: the read no longer counts as retrying,
		// and each byte of its answer after keeps it from counting again.
		s.progress()
		body = progressReader{out.Body, s}
	}
	want := total
	if out.ContentLength != nil {
		want = min(want, uint64(*out.ContentLength))
	}
	var done uint64
	var bodyErr error
	for _, m := range dst.Mappings {
		if done == want {
			break
		}
		m = m[:min(uint64(len(m)), want-done)]
		n, err := io.ReadFull(body, m)
		done += uint64(n)
		if err != nil {
			bodyErr = err
			break
		}
	}
	switch done {
	case total:
		return done, nil
	case want:
		return done, io.EOF
	default:
		return done, fmt.Errorf("%w after %d of %d bytes: %v", errShortBody, done, want, bodyErr)
	}
}
