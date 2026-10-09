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
	"net/http"
	"net/http/httptest"
	"slices"
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
