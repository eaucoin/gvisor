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
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
)

// errShortBody is returned by Reader.readOnce if a response's body ended
// before it delivered its bytes, as when its connection resets.
var errShortBody = errors.New("response body ended early")

// Reader implements stateio.WaitOrAsyncReader for an S3 object, with one
// ranged GET per read.
type Reader struct {
	stateio.NoRegisterClientFD

	client       *s3.Client
	obj          object
	retry        retryPolicy
	maxReadBytes uint64
	maxRanges    int
	subs         chan readSubmission
	cmps         chan stateio.Completion
	cancel       context.CancelCauseFunc
	workers      sync.WaitGroup
}

type readSubmission struct {
	id    int
	off   int64
	total uint64
	dst   stateio.LocalClientRanges
}

// newReader returns a Reader that reads from obj.
func newReader(ctx context.Context, client *s3.Client, obj object, retry retryPolicy, maxReadBytes uint64, maxRanges, maxParallel int) *Reader {
	ctx, cancel := context.WithCancelCause(ctx)
	r := &Reader{
		client:       client,
		obj:          obj,
		retry:        retry,
		maxReadBytes: maxReadBytes,
		maxRanges:    maxRanges,
		subs:         make(chan readSubmission, maxParallel),
		cmps:         make(chan stateio.Completion, maxParallel),
		cancel:       cancel,
	}
	r.workers.Add(maxParallel)
	for range maxParallel {
		go r.workerMain(ctx)
	}
	return r
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
func (r *Reader) read(ctx context.Context, sub *readSubmission) (uint64, error) {
	var done uint64
	dst := sub.dst
	for failures := 0; ; {
		n, err := r.readOnce(ctx, sub.off+int64(done), sub.total-done, &dst)
		done += n
		if !errors.Is(err, errShortBody) {
			return done, err
		}
		if n != 0 {
			failures = 0
		} else if failures++; failures == r.retry.maxAttempts {
			return done, mapError(err, "GetObject", r.obj)
		}
		dst = dst.DropFirst(n)
	}
}

// readOnce reads up to total bytes at off into dst with one GET. It returns
// io.EOF if the object ends before them.
func (r *Reader) readOnce(ctx context.Context, off int64, total uint64, dst *stateio.LocalClientRanges) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.retry.deadline(total))
	defer cancel()
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.obj.bucket),
		Key:    aws.String(r.obj.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, off+int64(total)-1)),
	})
	if err != nil {
		var respErr *awshttp.ResponseError
		if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable {
			// off is at or past the end of the object.
			return 0, io.EOF
		}
		return 0, mapError(err, "GetObject", r.obj)
	}
	defer out.Body.Close()
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
		n, err := io.ReadFull(out.Body, m)
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
