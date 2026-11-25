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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3/s3test"
)

// testSignature stands for the signature of a URL presigned for a test.
const testSignature = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// urlRequest is a request by URL, as the store got it.
type urlRequest struct {
	path   string
	query  string
	header http.Header
}

// urlStore serves a Store's objects by the URLs it gives them (urlOf), and
// keeps the requests it gets.
type urlStore struct {
	*s3test.Store
	hs *httptest.Server

	mu       sync.Mutex
	requests []urlRequest
}

// newURLStore returns a urlStore of a new Store.
func newURLStore(t *testing.T) *urlStore {
	s := &urlStore{Store: s3test.NewStore(t)}
	s.hs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, urlRequest{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
		s.mu.Unlock()
		s.Store.ServeHTTP(w, r)
	}))
	t.Cleanup(s.hs.Close)
	return s
}

// urlOf returns the URL of the object key, as one presigned for GET with
// Signature Version 4 is, whose signature the store does not check.
func (s *urlStore) urlOf(key string) string {
	return s.signedURL(key, testSignature, time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), 3600)
}

// signedURL returns the URL of the object key as urlOf does, of the signature
// signature, signed at signed for expires seconds.
func (s *urlStore) signedURL(key, signature string, signed time.Time, expires int) string {
	return fmt.Sprintf("%s/%s/%s?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=KEY%%2F20261125%%2Fus-east-1%%2Fs3%%2Faws4_request&X-Amz-Date=%s&X-Amz-Expires=%d&X-Amz-SignedHeaders=host&X-Amz-Signature=%s", s.hs.URL, s3test.Bucket, key, signed.UTC().Format("20060102T150405Z"), expires, signature)
}

// writeURLs replaces the URLs file at path with one holding urls, as their
// host does, readable by its owner alone.
func writeURLs(t *testing.T, path string, urls map[string]string) {
	t.Helper()
	b, err := json.Marshal(urls)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}

// gotRequests returns the requests that s got.
func (s *urlStore) gotRequests() []urlRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]urlRequest(nil), s.requests...)
}

// newURLFileServer returns a FileServer that reads each of paths, under
// "image/" in s, by its URL (urlOf).
func newURLFileServer(t *testing.T, s *urlStore, paths []string, opts FileServerOptions) *FileServer {
	opts.Bucket = s3test.Bucket
	opts.ObjectPrefix = "image/"
	urls := make(map[string]string, len(paths))
	for _, p := range paths {
		urls[p] = s.urlOf("image/" + p)
	}
	opts.URLsFile = filepath.Join(t.TempDir(), "urls.json")
	writeURLs(t, opts.URLsFile, urls)
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

// TestReadByURL checks that a FileServer given URLs reads each file at its
// URL as given, its query whole and nothing added to it, with the Range and
// If-Match it asks, and with neither a signature of its own nor any header of
// the store's (x-amz-*), which the URL's signature does not sign.
func TestReadByURL(t *testing.T) {
	layerPages := checkpointimage.LayerPath(checkpointimage.Digest{1}, checkpointfiles.PagesFileName)
	paths := []string{checkpointfiles.StateFileName, checkpointfiles.PagesFileName, layerPages}
	s := newURLStore(t)
	objs := make(map[string][]byte)
	for i, p := range paths {
		objs[p] = testObject((i + 1) << 14)
		s.SetObject("image/"+p, objs[p])
	}
	etag := s3test.ETag(objs[checkpointfiles.PagesFileName])
	for _, whileRunning := range []bool{false, true} {
		srv := newURLFileServer(t, s, paths, FileServerOptions{
			AllowCheckpointReads:   true,
			PagesRetryWhileRunning: whileRunning,
			ETags:                  map[string]string{checkpointfiles.PagesFileName: etag},
		})
		for _, p := range paths {
			obj := objs[p]
			buf := make([]byte, len(obj)-4096)
			if c := read(t, openRead(t, srv, p), 4096, buf); c.N != uint64(len(buf)) || c.Err != nil || !bytes.Equal(buf, obj[4096:]) {
				t.Errorf("read of %s at 4 KiB: %d bytes, %v; want the rest of the object", p, c.N, c.Err)
			}
		}
	}
	reqs := s.gotRequests()
	if len(reqs) != 2*len(paths) {
		t.Fatalf("%d requests, want %d", len(reqs), 2*len(paths))
	}
	for i, req := range reqs {
		p := paths[i%len(paths)]
		want := s.urlOf("image/" + p)
		if got := s.hs.URL + req.path + "?" + req.query; got != want {
			t.Errorf("request %d asked %q, want %q", i, got, want)
		}
		for name := range req.header {
			if lower := strings.ToLower(name); lower == "authorization" || strings.HasPrefix(lower, "x-amz-") {
				t.Errorf("request %d of %s sent %s: %q, which the URL does not sign", i, p, name, req.header.Get(name))
			}
		}
		if got := req.header.Get("Range"); !strings.HasPrefix(got, "bytes=4096-") {
			t.Errorf("request %d of %s asked range %q, want from 4096", i, p, got)
		}
		wantIfMatch := ""
		if p == checkpointfiles.PagesFileName {
			wantIfMatch = etag
		}
		if got := req.header.Get("If-Match"); got != wantIfMatch {
			t.Errorf("request %d of %s with If-Match %q, want %q", i, p, got, wantIfMatch)
		}
	}
}

// TestReadByURLRefused checks that a read by a URL that the store refuses, as
// one past its expiry or of another object, fails at once with EACCES,
// without retrying, whether or not its pages reads retry while the sandbox
// runs: a refusal recurs.
func TestReadByURLRefused(t *testing.T) {
	for _, whileRunning := range []bool{false, true} {
		s := newURLStore(t)
		s.SetObject("image/"+checkpointfiles.PagesFileName, testObject(4096))
		s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
			s3test.WriteError(w, http.StatusForbidden, "AccessDenied")
			return true
		}
		srv := newURLFileServer(t, s, []string{checkpointfiles.PagesFileName}, FileServerOptions{AllowCheckpointReads: true, PagesRetryWhileRunning: whileRunning})
		c := read(t, openRead(t, srv, checkpointfiles.PagesFileName), 0, make([]byte, 4096))
		if c.N != 0 || !errors.Is(c.Err, unix.EACCES) {
			t.Errorf("read with pages retry %t: %d bytes, %v; want 0 bytes, EACCES", whileRunning, c.N, c.Err)
		}
		if got := len(s.gotRequests()); got != 1 {
			t.Errorf("read with pages retry %t: %d requests, want 1", whileRunning, got)
		}
	}
}

// allLines is a log.Emitter that keeps every line logged.
type allLines struct {
	mu    sync.Mutex
	lines []string
}

// Emit implements log.Emitter.Emit.
func (e *allLines) Emit(_ int, _ log.Level, _ time.Time, format string, v ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lines = append(e.lines, fmt.Sprintf(format, v...))
}

// TestReadByURLErrorsHideQuery checks that neither the error of a read by URL
// whose store cannot be reached, nor what the FileServer logs of it, holds
// the URL's query, which grants reads: the URL is named without it.
func TestReadByURLErrorsHideQuery(t *testing.T) {
	s := newURLStore(t)
	srv := newURLFileServer(t, s, []string{checkpointfiles.PagesFileName}, FileServerOptions{AllowCheckpointReads: true, MaxAttempts: 1})
	s.hs.Close()
	lines := &allLines{}
	prev := log.Log()
	log.SetTarget(lines)
	defer log.SetTarget(prev.Emitter)

	c := read(t, openRead(t, srv, checkpointfiles.PagesFileName), 0, make([]byte, 4096))
	if c.Err == nil {
		t.Fatalf("read with the store gone: %d bytes, no error", c.N)
	}
	if strings.Contains(c.Err.Error(), testSignature) {
		t.Errorf("the read's error holds the URL's signature: %v", c.Err)
	}
	if !strings.Contains(c.Err.Error(), "/image/"+checkpointfiles.PagesFileName) {
		t.Errorf("the read's error does not name the object: %v", c.Err)
	}
	lines.mu.Lock()
	defer lines.mu.Unlock()
	for _, line := range lines.lines {
		if strings.Contains(line, testSignature) || strings.Contains(line, "X-Amz-Credential") {
			t.Errorf("logged the URL's query: %q", line)
		}
	}
}

// TestURLsRefuse checks what a FileServer given URLs refuses: a file without
// a URL, credentials beside them, writes, a URLs file others may read, and
// URLs that are not absolute HTTP(S) URLs, whose error does not hold them.
func TestURLsRefuse(t *testing.T) {
	s := newURLStore(t)
	srv := newURLFileServer(t, s, []string{checkpointfiles.PagesFileName}, FileServerOptions{AllowCheckpointReads: true})
	if _, err := srv.OpenRead(checkpointfiles.StateFileName); err == nil {
		t.Errorf("OpenRead of a file without a URL: got success, want an error")
	}
	if len(s.gotRequests()) != 0 {
		t.Errorf("the store got a request for a file without a URL")
	}
	dir := t.TempDir()
	file := func(name string, urls map[string]string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		writeURLs(t, path, urls)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := file("good.json", map[string]string{checkpointfiles.PagesFileName: s.urlOf("image/pages.img")}, 0o600)
	for name, opts := range map[string]FileServerOptions{
		"credentials":            {AllowCheckpointReads: true, URLsFile: good, Credentials: credentials.NewStaticCredentialsProvider("key", "secret", "")},
		"a profile":              {AllowCheckpointReads: true, URLsFile: good, Profile: "p"},
		"writes":                 {AllowCheckpointWrites: true, URLsFile: good},
		"a file others may read": {AllowCheckpointReads: true, URLsFile: file("read.json", map[string]string{checkpointfiles.PagesFileName: s.urlOf("image/pages.img")}, 0o644)},
		"no file":                {AllowCheckpointReads: true, URLsFile: filepath.Join(dir, "none.json")},
		"a relative URL":         {AllowCheckpointReads: true, URLsFile: file("relative.json", map[string]string{checkpointfiles.PagesFileName: "/bucket/image/pages.img?X-Amz-Signature=" + testSignature}, 0o600)},
		"another scheme":         {AllowCheckpointReads: true, URLsFile: file("scheme.json", map[string]string{checkpointfiles.PagesFileName: "ftp://host/pages.img?X-Amz-Signature=" + testSignature}, 0o600)},
		"an unparsable URL":      {AllowCheckpointReads: true, URLsFile: file("unparsable.json", map[string]string{checkpointfiles.PagesFileName: "http://[::1/pages.img?X-Amz-Signature=" + testSignature}, 0o600)},
	} {
		opts.Bucket = s3test.Bucket
		srv, err := NewFileServer(context.Background(), &opts)
		if err == nil {
			srv.Destroy()
			t.Errorf("NewFileServer with URLs and %s: got success, want an error", name)
			continue
		}
		if strings.Contains(err.Error(), testSignature) {
			t.Errorf("NewFileServer with URLs and %s: the error holds a URL's signature: %v", name, err)
		}
	}
}

// signedStore serves a Store's objects by URL to the signatures it takes
// alone, refusing others (403) as a store refuses a URL past its expiry, and
// resets the connections of reads while away is true, as a store away does.
type signedStore struct {
	*urlStore
	mu    sync.Mutex
	valid map[string]bool
	away  bool
}

func newSignedStore(t *testing.T) *signedStore {
	s := &signedStore{urlStore: newURLStore(t), valid: make(map[string]bool)}
	s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.away {
			s3test.ResetConnection(t, w, "")
			return true
		}
		if !s.valid[r.URL.Query().Get("X-Amz-Signature")] {
			s3test.WriteError(w, http.StatusForbidden, "AccessDenied")
			return true
		}
		return false
	}
	return s
}

// takes has the store take only the signatures sigs from now.
func (s *signedStore) takes(sigs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.valid = make(map[string]bool)
	for _, sig := range sigs {
		s.valid[sig] = true
	}
}

func (s *signedStore) setAway(away bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.away = away
}

// TestURLsSignedAgain checks that a FileServer reads its URLs file again as
// its URLs near their expiry, and once where the store refuses one if the
// file has changed since, as their host signs them again; that a refusal is
// otherwise final; and that a restore whose store is away for longer than its
// URLs last reads on once it is back, by the URLs signed again meanwhile.
func TestURLsSignedAgain(t *testing.T) {
	oldCheck := urlsCheckEvery
	urlsCheckEvery = 0
	defer func() { urlsCheckEvery = oldCheck }()
	s := newSignedStore(t)
	s.SetObject("image/"+checkpointfiles.PagesFileName, testObject(16<<10))
	path := filepath.Join(t.TempDir(), "urls.json")
	sign := func(sig string, signed time.Time, expires int) {
		writeURLs(t, path, map[string]string{checkpointfiles.PagesFileName: s.signedURL("image/"+checkpointfiles.PagesFileName, sig, signed, expires)})
	}
	asked := func() []string {
		var sigs []string
		for _, req := range s.gotRequests() {
			q, _ := url.ParseQuery(req.query)
			sigs = append(sigs, q.Get("X-Amz-Signature"))
		}
		return sigs
	}

	// Signed for an hour, now: read by its URL, the file not read again.
	sign("first", time.Now(), 3600)
	s.takes("first")
	srv, err := NewFileServer(context.Background(), &FileServerOptions{AllowCheckpointReads: true, Bucket: s3test.Bucket, ObjectPrefix: "image/", URLsFile: path, MaxAttempts: 1, PagesRetryWhileRunning: true})
	if err != nil {
		t.Fatalf("NewFileServer: %v", err)
	}
	defer srv.Destroy()
	r := openRead(t, srv, checkpointfiles.PagesFileName)
	if c := read(t, r, 0, make([]byte, 4096)); c.Err != nil {
		t.Fatalf("read: %v", c.Err)
	}
	// Signed again, the URL before still taken and far from its expiry: still read by the first.
	sign("second", time.Now(), 3600)
	s.takes("first", "second")
	if c := read(t, r, 0, make([]byte, 4096)); c.Err != nil {
		t.Fatalf("read: %v", c.Err)
	}
	if got := asked(); strings.Join(got, ",") != "first,first" {
		t.Errorf("reads asked with %v, want the first URL twice", got)
	}

	// Refused, the file changed since: asked again once, at the URL signed again.
	s.takes("second")
	if c := read(t, r, 0, make([]byte, 4096)); c.Err != nil {
		t.Errorf("read refused as the file changed: %v, want it read by the URL signed again", c.Err)
	}
	if got := asked(); strings.Join(got[2:], ",") != "first,second" {
		t.Errorf("reads asked with %v after the refusal, want the first then the second", got[2:])
	}
	// Refused, the file unchanged since: final, asked once.
	s.takes()
	if c := read(t, r, 0, make([]byte, 4096)); !errors.Is(c.Err, unix.EACCES) {
		t.Errorf("read refused, the file unchanged: %v, want EACCES", c.Err)
	}
	if got := asked(); len(got) != 5 {
		t.Errorf("%d requests, want 5: a refusal of an unchanged file is not asked again", len(got))
	}

	// Its URLs near their expiry (signed 50 min ago for an hour): the file read again before a read.
	sign("third", time.Now().Add(-50*time.Minute), 3600)
	s.takes("third")
	if c := read(t, r, 0, make([]byte, 4096)); c.Err != nil {
		t.Fatalf("read: %v", c.Err)
	}
	sign("fourth", time.Now(), 3600)
	s.takes("fourth")
	if c := read(t, r, 0, make([]byte, 4096)); c.Err != nil {
		t.Errorf("read as the URLs near their expiry: %v, want it read by the URL signed again", c.Err)
	}
	if got := asked(); strings.Join(got[len(got)-2:], ",") != "third,fourth" {
		t.Errorf("the last reads asked with %v, want the third then the fourth URL, none refused", got[len(got)-2:])
	}

	// The store away for longer than the URLs last, the host signing them again meanwhile: read once it is back.
	s.setAway(true)
	done := make(chan stateio.Completion, 1)
	go func() { done <- read(t, r, 0, make([]byte, 4096)) }()
	time.Sleep(50 * time.Millisecond)
	sign("fifth", time.Now(), 3600)
	s.takes("fifth")
	s.setAway(false)
	select {
	case c := <-done:
		if c.Err != nil {
			t.Errorf("read across the store's absence: %v, want it read once the store is back", c.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("read across the store's absence did not end once the store was back")
	}
}
