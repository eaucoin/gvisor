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
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
)

// Writer implements stateio.AsyncWriter for an S3 object.
//
// Writer gathers writes, which are sequential, into parts of partBytes, and
// uploads them as the parts of a multipart upload, up to maxParallelParts at
// once. A write completes once its bytes are in a part, so that its memory
// may be reused. Finalize uploads the last part and completes the upload, or,
// if the object is smaller than a part, uploads it with one PUT. Close aborts
// an upload that Finalize did not complete, so that the store does not keep
// its parts.
type Writer struct {
	stateio.NoRegisterClientFD

	ctx           context.Context
	client        *s3.Client
	obj           object
	retry         retryPolicy
	maxWriteBytes uint64
	maxRanges     int
	partBytes     int
	subs          chan writeSubmission
	cmps          chan stateio.Completion
	cancel        context.CancelCauseFunc
	worker        sync.WaitGroup

	// subsClosed is true once Finalize or Close closed subs.
	subsClosed bool

	// The following fields are exclusive to workerMain until it exits.

	// part is the part being filled.
	part []byte

	// uploadID is the ID of the multipart upload, or nil if none is in
	// progress. parts is the number of parts whose upload started.
	uploadID *string
	parts    int32

	// uploads limits the number of parts being uploaded, and uploading waits
	// for them.
	uploads   chan struct{}
	uploading sync.WaitGroup

	// mu protects completed and err, which part uploads update.
	mu        sync.Mutex
	completed []types.CompletedPart
	err       error
}

type writeSubmission struct {
	id  int
	src stateio.LocalClientRanges
}

// newWriter returns a Writer that writes to obj.
func newWriter(ctx context.Context, client *s3.Client, obj object, retry retryPolicy, maxWriteBytes uint64, maxRanges, maxParallel, partBytes, maxParallelParts int) *Writer {
	ctx, cancel := context.WithCancelCause(ctx)
	w := &Writer{
		ctx:           ctx,
		client:        client,
		obj:           obj,
		retry:         retry,
		maxWriteBytes: maxWriteBytes,
		maxRanges:     maxRanges,
		partBytes:     partBytes,
		subs:          make(chan writeSubmission, maxParallel),
		cmps:          make(chan stateio.Completion, maxParallel),
		cancel:        cancel,
		part:          make([]byte, 0, partBytes),
		uploads:       make(chan struct{}, maxParallelParts),
	}
	w.worker.Add(1)
	go w.workerMain()
	return w
}

// Close implements stateio.AsyncWriter.Close.
func (w *Writer) Close() error {
	w.cancel(fmt.Errorf("context canceled by Writer.Close"))
	w.stopWorker()
	w.uploading.Wait()
	if w.uploadID == nil {
		return nil
	}
	// Finalize was not called, or failed: abort the upload, so that the
	// store frees its parts.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), w.retry.deadline(0))
	defer cancel()
	if _, err := w.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(w.obj.bucket),
		Key:      aws.String(w.obj.key),
		UploadId: w.uploadID,
	}); err != nil {
		return mapError(err, "AbortMultipartUpload", w.obj)
	}
	w.uploadID = nil
	return nil
}

// MaxWriteBytes implements stateio.AsyncWriter.MaxWriteBytes.
func (w *Writer) MaxWriteBytes() uint64 {
	return w.maxWriteBytes
}

// MaxRanges implements stateio.AsyncWriter.MaxRanges.
func (w *Writer) MaxRanges() int {
	return w.maxRanges
}

// MaxParallel implements stateio.AsyncWriter.MaxParallel.
func (w *Writer) MaxParallel() int {
	return cap(w.subs)
}

// AddWrite implements stateio.AsyncWriter.AddWrite.
func (w *Writer) AddWrite(id int, _ stateio.SourceFile, _ memmap.FileRange, srcMap []byte) {
	w.subs <- writeSubmission{
		id:  id,
		src: stateio.LocalClientMapping(srcMap),
	}
}

// AddWritev implements stateio.AsyncWriter.AddWritev.
func (w *Writer) AddWritev(id int, total uint64, _ stateio.SourceFile, _ []memmap.FileRange, srcMaps []unix.Iovec) {
	w.subs <- writeSubmission{
		id:  id,
		src: stateio.LocalClientMappings(srcMaps),
	}
}

// Wait implements stateio.AsyncWriter.Wait.
func (w *Writer) Wait(cs []stateio.Completion, minCompletions int) ([]stateio.Completion, error) {
	return stateio.CompletionChanWait(w.cmps, cs, minCompletions)
}

// Reserve implements stateio.AsyncWriter.Reserve.
func (w *Writer) Reserve(n uint64) {
	// no-op
}

// Finalize implements stateio.AsyncWriter.Finalize.
func (w *Writer) Finalize() error {
	// No writes are in flight, so workerMain has no write left; stop it to
	// take over its state.
	w.stopWorker()
	if w.uploadID == nil {
		if err := w.uploadErr(); err != nil {
			// CreateMultipartUpload failed.
			return err
		}
		// The object is smaller than a part.
		ctx, cancel := context.WithTimeout(w.ctx, w.retry.deadline(uint64(len(w.part))))
		defer cancel()
		if _, err := w.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(w.obj.bucket),
			Key:         aws.String(w.obj.key),
			Body:        bytes.NewReader(w.part),
			ContentType: aws.String(contentType),
		}); err != nil {
			return mapError(err, "PutObject", w.obj)
		}
		return nil
	}
	if len(w.part) != 0 {
		w.uploadPart()
	}
	w.uploading.Wait()
	if err := w.uploadErr(); err != nil {
		return err
	}
	slices.SortFunc(w.completed, func(a, b types.CompletedPart) int {
		return int(*a.PartNumber - *b.PartNumber)
	})
	ctx, cancel := context.WithTimeout(w.ctx, w.retry.deadline(0))
	defer cancel()
	if _, err := w.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(w.obj.bucket),
		Key:             aws.String(w.obj.key),
		UploadId:        w.uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: w.completed},
	}); err != nil {
		return mapError(err, "CompleteMultipartUpload", w.obj)
	}
	w.uploadID = nil
	return nil
}

// stopWorker stops workerMain and waits for it to exit.
func (w *Writer) stopWorker() {
	if !w.subsClosed {
		close(w.subs)
		w.subsClosed = true
	}
	w.worker.Wait()
}

func (w *Writer) workerMain() {
	defer w.worker.Done()
	for sub := range w.subs {
		var done uint64
		for _, src := range sub.src.Mappings {
			for len(src) != 0 {
				n := min(len(src), w.partBytes-len(w.part))
				w.part = append(w.part, src[:n]...)
				src = src[n:]
				done += uint64(n)
				if len(w.part) == w.partBytes {
					w.uploadPart()
				}
			}
		}
		// Once an upload failed, the object cannot be completed: fail
		// every write.
		w.cmps <- stateio.Completion{
			ID:  sub.id,
			N:   done,
			Err: w.uploadErr(),
		}
	}
}

// uploadPart starts uploading w.part as the next part of the multipart
// upload, which it starts if needed, and gives w an empty part to fill. It
// waits while maxParallelParts parts are being uploaded.
func (w *Writer) uploadPart() {
	part := w.part
	w.part = make([]byte, 0, w.partBytes)
	if w.uploadErr() != nil {
		return
	}
	if w.uploadID == nil {
		ctx, cancel := context.WithTimeout(w.ctx, w.retry.deadline(0))
		out, err := w.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket:      aws.String(w.obj.bucket),
			Key:         aws.String(w.obj.key),
			ContentType: aws.String(contentType),
		})
		cancel()
		if err != nil {
			w.setUploadErr(mapError(err, "CreateMultipartUpload", w.obj))
			return
		}
		w.uploadID = out.UploadId
	}
	w.parts++
	uploadID, partNumber := w.uploadID, w.parts
	w.uploads <- struct{}{}
	w.uploading.Add(1)
	go func() {
		defer w.uploading.Done()
		defer func() { <-w.uploads }()
		ctx, cancel := context.WithTimeout(w.ctx, w.retry.deadline(uint64(len(part))))
		defer cancel()
		out, err := w.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(w.obj.bucket),
			Key:        aws.String(w.obj.key),
			UploadId:   uploadID,
			PartNumber: aws.Int32(partNumber),
			Body:       bytes.NewReader(part),
		})
		if err != nil {
			w.setUploadErr(mapError(err, "UploadPart", w.obj))
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.completed = append(w.completed, types.CompletedPart{
			ETag:       out.ETag,
			PartNumber: aws.Int32(partNumber),
		})
	}()
}

func (w *Writer) uploadErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *Writer) setUploadErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
}
