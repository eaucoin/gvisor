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
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
)

const (
	// cacheChunkBytes is the size of the buffers that the bytes of a pages
	// file are read into, before they are copied to the client's memory and
	// written to the cache.
	cacheChunkBytes = 1 << 20

	// cacheQueueBytes bounds the bytes waiting to be written to the cache: a
	// chunk read while they would be more is not written, and leaves a hole.
	cacheQueueBytes = 64 << 20

	// cacheAlign is the alignment of O_DIRECT writes.
	cacheAlign = 4096
)

// cacheChunk is a chunk of a pages file waiting to be written to the cache.
type cacheChunk struct {
	off int64
	buf []byte
}

// cacheWriter writes the bytes of a pages file that a Reader reads into a
// local file, at their offsets, with O_DIRECT, by one goroutine, behind the
// reads: a read never waits for the cache, whose queue drops what does not
// fit.
type cacheWriter struct {
	path  string
	f     *os.File
	queue chan cacheChunk
	free  chan []byte
	done  chan struct{}

	// allocated is the number of buffers mapped.
	allocated atomic.Int32

	// written and dropped count the bytes written and not written; writing
	// and copying the time spent in pwrite(2) and copying to the client.
	written atomic.Uint64
	dropped atomic.Uint64
	writing atomic.Int64
	copying atomic.Int64
	err     atomic.Value
}

// newCacheWriter returns a cacheWriter writing to path, for a Reader making
// maxParallel reads at once.
func newCacheWriter(path string, maxParallel int) (*cacheWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|unix.O_DIRECT, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	queued := cacheQueueBytes / cacheChunkBytes
	w := &cacheWriter{
		path:  path,
		f:     f,
		queue: make(chan cacheChunk, queued),
		free:  make(chan []byte, queued+maxParallel),
		done:  make(chan struct{}),
	}
	go w.main()
	return w, nil
}

func (w *cacheWriter) main() {
	defer close(w.done)
	for c := range w.queue {
		begun := time.Now()
		if _, err := unix.Pwrite(int(w.f.Fd()), c.buf, c.off); err != nil {
			if w.err.Load() == nil {
				w.err.Store(err)
			}
			w.dropped.Add(uint64(len(c.buf)))
		} else {
			w.written.Add(uint64(len(c.buf)))
		}
		w.writing.Add(int64(time.Since(begun)))
		w.free <- c.buf[:cap(c.buf)]
	}
}

// buffer returns a buffer of cacheChunkBytes, or nil if every buffer is in
// use: the bytes are then read to the client's memory alone.
func (w *cacheWriter) buffer() []byte {
	select {
	case buf := <-w.free:
		return buf
	default:
	}
	if int(w.allocated.Load()) >= cap(w.free) {
		return nil
	}
	buf, err := unix.Mmap(-1, 0, cacheChunkBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil
	}
	w.allocated.Add(1)
	return buf
}

// write queues buf, the bytes of the pages file at off, to be written; buf
// is the writer's again once they are. Bytes that O_DIRECT cannot write (not
// aligned) or that do not fit in the queue are dropped.
func (w *cacheWriter) write(off int64, buf []byte) {
	if off%cacheAlign != 0 || len(buf)%cacheAlign != 0 {
		w.drop(buf)
		return
	}
	select {
	case w.queue <- cacheChunk{off: off, buf: buf}:
	default:
		w.drop(buf)
	}
}

// drop counts buf's bytes as not written and returns it to the writer.
func (w *cacheWriter) drop(buf []byte) {
	w.dropped.Add(uint64(len(buf)))
	w.free <- buf[:cap(buf)]
}

// close waits for the bytes queued to be written, syncs and closes the file,
// and logs what was written.
func (w *cacheWriter) close() {
	close(w.queue)
	<-w.done
	begun := time.Now()
	syncErr := unix.Fdatasync(int(w.f.Fd()))
	synced := time.Since(begun)
	w.f.Close()
	for range w.allocated.Load() {
		unix.Munmap(<-w.free)
	}
	log.Infof("S3 read trace: cache %s written %d dropped %d bytes; pwrite %v, copy %v, fdatasync %v (%v); write error %v", w.path, w.written.Load(), w.dropped.Load(), time.Duration(w.writing.Load()), time.Duration(w.copying.Load()), synced, syncErr, w.err.Load())
}
