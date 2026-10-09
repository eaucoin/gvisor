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

// Package s3test provides an in-memory S3-compatible object store for tests
// of the checkpoint gofer.
package s3test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/sync"
)

// Bucket is the only bucket of a Store.
const Bucket = "bucket"

// Store is an in-memory S3-compatible object store that serves the requests
// of the S3 checkpoint gofer, with path-style addressing. It does not check
// their signatures.
type Store struct {
	t testing.TB

	// Fault, if not nil, is called for each request, numbered from 0 by
	// operation (e.g. "GetObject"), and may answer it itself, in which case
	// it returns true.
	Fault func(w http.ResponseWriter, r *http.Request, op string, n int) bool

	mu       sync.Mutex
	objects  map[string][]byte
	uploads  map[string]map[int][]byte
	nextID   int
	requests []Request
	aborted  int
}

// Request is a request that a Store received.
type Request struct {
	// Op is the S3 operation, e.g. "GetObject".
	Op string

	// Key is the object's key.
	Key string

	// Range is the Range header.
	Range string

	// Bytes is the length of the body.
	Bytes int
}

// NewStore returns an empty Store, which reports errors in requests to t.
func NewStore(t testing.TB) *Store {
	return &Store{
		t:       t,
		objects: make(map[string][]byte),
		uploads: make(map[string]map[int][]byte),
	}
}

// Object returns the object with the given key.
func (s *Store) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key]
	return obj, ok
}

// SetObject sets the object with the given key.
func (s *Store) SetObject(key string, obj []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = obj
}

// Requests returns the requests that s received.
func (s *Store) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Ops returns the operations of the requests that s received.
func (s *Store) Ops() []string {
	var ops []string
	for _, r := range s.Requests() {
		ops = append(ops, r.Op)
	}
	return ops
}

// Uploads returns the number of multipart uploads in progress, and the
// number that were aborted.
func (s *Store) Uploads() (inProgress, aborted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads), s.aborted
}

// opOf returns the S3 operation that r performs.
func opOf(r *http.Request) string {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet:
		return "GetObject"
	case r.Method == http.MethodPut && q.Has("uploadId"):
		return "UploadPart"
	case r.Method == http.MethodPut:
		return "PutObject"
	case r.Method == http.MethodPost && q.Has("uploads"):
		return "CreateMultipartUpload"
	case r.Method == http.MethodPost && q.Has("uploadId"):
		return "CompleteMultipartUpload"
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		return "AbortMultipartUpload"
	}
	return r.Method
}

// ServeHTTP implements http.Handler.ServeHTTP.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := opOf(r)
	key, ok := strings.CutPrefix(r.URL.Path, "/"+Bucket+"/")
	if !ok {
		WriteError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("%s %s: reading the body: %v", op, key, err)
		return
	}
	s.mu.Lock()
	n := 0
	for _, req := range s.requests {
		if req.Op == op {
			n++
		}
	}
	s.requests = append(s.requests, Request{Op: op, Key: key, Range: r.Header.Get("Range"), Bytes: len(body)})
	s.mu.Unlock()
	if s.Fault != nil && s.Fault(w, r, op, n) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	switch op {
	case "GetObject":
		obj, ok := s.objects[key]
		if !ok {
			WriteError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			s.t.Errorf("GetObject %s: Range %q: %v", key, r.Header.Get("Range"), err)
			WriteError(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		if start >= len(obj) {
			WriteError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		end = min(end, len(obj)-1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(obj)))
		w.Header().Set("Content-Length", strconv.Itoa(end+1-start))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(obj[start : end+1])
	case "PutObject":
		s.objects[key] = body
		w.Header().Set("ETag", `"object"`)
	case "CreateMultipartUpload":
		s.nextID++
		id := strconv.Itoa(s.nextID)
		s.uploads[id] = make(map[int][]byte)
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", Bucket, key, id)
	case "UploadPart":
		parts, ok := s.uploads[q.Get("uploadId")]
		if !ok {
			WriteError(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		pn, err := strconv.Atoi(q.Get("partNumber"))
		if err != nil || pn < 1 || pn > 10000 {
			WriteError(w, http.StatusBadRequest, "InvalidArgument")
			return
		}
		parts[pn] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part%d"`, pn))
	case "CompleteMultipartUpload":
		id := q.Get("uploadId")
		parts, ok := s.uploads[id]
		if !ok {
			WriteError(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			s.t.Errorf("CompleteMultipartUpload %s: %v", key, err)
			WriteError(w, http.StatusBadRequest, "MalformedXML")
			return
		}
		if len(req.Parts) != len(parts) {
			s.t.Errorf("CompleteMultipartUpload %s: %d parts, %d uploaded", key, len(req.Parts), len(parts))
		}
		var obj []byte
		for i, p := range req.Parts {
			if p.PartNumber != i+1 || p.ETag != fmt.Sprintf(`"part%d"`, i+1) || parts[p.PartNumber] == nil {
				WriteError(w, http.StatusBadRequest, "InvalidPart")
				return
			}
			obj = append(obj, parts[p.PartNumber]...)
		}
		s.objects[key] = obj
		delete(s.uploads, id)
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"object"</ETag></CompleteMultipartUploadResult>`, Bucket, key)
	case "AbortMultipartUpload":
		delete(s.uploads, q.Get("uploadId"))
		s.aborted++
		w.WriteHeader(http.StatusNoContent)
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		WriteError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// WriteError answers a request with an S3 error.
func WriteError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

// ResetConnection closes the connection of the request answered by w after
// writing head, without completing the response.
func ResetConnection(t testing.TB, w http.ResponseWriter, head string) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("Hijack: %v", err)
		return
	}
	buf.WriteString(head)
	buf.Flush()
	conn.Close()
}
