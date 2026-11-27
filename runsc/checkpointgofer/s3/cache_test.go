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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/sys/unix"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3/s3test"
)

// cacheTestPages is the number of pages of the test image's pages file.
const cacheTestPages = 64

// cacheTestImage returns the pages file of an image of n pages, all of which
// the image refers to, and its pages metadata file, whose page hashes are
// those of pages.
func cacheTestImage(t *testing.T, n uint64) (pages, meta []byte) {
	t.Helper()
	pages = testObject(int(n * cachePage))
	mf := &pb.MemoryFileMetadataProto{
		Version: checkpointimage.MemoryFileMetadataVersion,
		MemAcct: []*pb.MemAcctRangeProto{{Start: 0, End: n * cachePage, KnownCommitted: true}},
		Extents: []*pb.ExtentProto{{Start: 0, End: n * cachePage}},
	}
	for p := uint64(0); p < n; p++ {
		mf.PageHashes = binary.LittleEndian.AppendUint64(mf.PageHashes, xxhash.Sum64(pages[p*cachePage:(p+1)*cachePage]))
	}
	var buf bytes.Buffer
	w := checkpointimage.NewWriter(&buf, &pb.ImageProto{
		Layers:           []*pb.LayerProto{{}},
		PageHash:         checkpointimage.PageHashXXH64,
		PageHashesDigest: make([]byte, sha256.Size),
	})
	if err := checkpointimage.WriteRecord(w, mf); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if _, err := w.Finish(n * cachePage); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return pages, buf.Bytes()
}

// cacheTestDir returns a cache directory staged with a sparse pages file of
// size bytes, and the directory opened.
func cacheTestDir(t *testing.T, size int64) (string, *os.File) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, checkpointfiles.PagesFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, checkpointfiles.PagesFileName), size); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, f
}

// marksOf returns the flags and the marked pages of the marks file of the
// pages file in dir.
func marksOf(t *testing.T, dir string) (flags uint32, marked []uint64) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, checkpointfiles.PagesFileName+cacheMarksSuffix))
	if err != nil {
		t.Fatalf("reading the marks: %v", err)
	}
	if !bytes.HasPrefix(b, []byte(cacheMarksMagic)) {
		t.Fatalf("marks %q do not begin with the magic", b[:min(len(b), 8)])
	}
	b = b[len(cacheMarksMagic):]
	if v := binary.LittleEndian.Uint32(b); v != cacheMarksVersion {
		t.Fatalf("marks of version %d", v)
	}
	flags = binary.LittleEndian.Uint32(b[4:])
	pages := binary.LittleEndian.Uint64(b[8:])
	b = b[16+sha256.Size:]
	for p := uint64(0); p < pages; p++ {
		if binary.LittleEndian.Uint64(b[p/64*8:])&(1<<(p%64)) != 0 {
			marked = append(marked, p)
		}
	}
	return flags, marked
}

// TestCacheWritesThrough checks that the pages a restore reads are written to
// the cache at their offsets and marked, that completing the cache reads the
// pages that were not read for itself, and that it then says, and its marks
// file says, that the cache is complete.
func TestCacheWritesThrough(t *testing.T) {
	pages, meta := cacheTestImage(t, cacheTestPages)
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, pages)
	s.SetObject(checkpointfiles.PagesMetadataFileName, meta)
	dir, cacheDir := cacheTestDir(t, int64(len(pages)))
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, CacheDir: cacheDir})

	// The restore reads all but the last 8 pages.
	r := openRead(t, srv, checkpointfiles.PagesFileName)
	upTo := (cacheTestPages - 8) * cachePage
	for off := 0; off < upTo; off += 16 * cachePage {
		buf := make([]byte, min(16*cachePage, upTo-off))
		if c := read(t, r, int64(off), buf); c.N != uint64(len(buf)) || c.Err != nil || !bytes.Equal(buf, pages[off:off+len(buf)]) {
			t.Fatalf("read at %d: %d bytes, %v", off, c.N, c.Err)
		}
	}
	st := srv.CacheStatus()
	if !st.Enabled || !st.Complete || st.Files != 1 || st.Referenced != uint64(len(pages)) || st.Held != uint64(len(pages)) || st.Filled != 8*cachePage || st.Mismatched != 0 || st.Error != "" {
		t.Errorf("cache status %+v: want enabled, complete, 1 file, %d bytes referenced and held, %d filled", st, len(pages), 8*cachePage)
	}
	if got, err := os.ReadFile(filepath.Join(dir, checkpointfiles.PagesFileName)); err != nil || !bytes.Equal(got, pages) {
		t.Errorf("cached pages file differs from the object (%v)", err)
	}
	if flags, marked := marksOf(t, dir); flags&cacheMarksComplete == 0 || len(marked) != cacheTestPages {
		t.Errorf("marks: flags %#x, %d pages marked; want complete, %d", flags, len(marked), cacheTestPages)
	}
	// Asked again, it says the same and reads nothing more.
	gets := len(s.Ops())
	if again := srv.CacheStatus(); again != st || len(s.Ops()) != gets {
		t.Errorf("cache status asked again: %+v after %d more requests, want %+v and none", again, len(s.Ops())-gets, st)
	}
}

// TestCacheMarksNoMismatchedPage checks that a page whose bytes do not match
// its hash in the image is not marked, read again or not, and that the cache
// then says it is not complete.
func TestCacheMarksNoMismatchedPage(t *testing.T) {
	pages, meta := cacheTestImage(t, cacheTestPages)
	corrupt := bytes.Clone(pages)
	corrupt[5*cachePage+100] ^= 0xff
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, corrupt)
	s.SetObject(checkpointfiles.PagesMetadataFileName, meta)
	dir, cacheDir := cacheTestDir(t, int64(len(pages)))
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, CacheDir: cacheDir})
	r := openRead(t, srv, checkpointfiles.PagesFileName)
	buf := make([]byte, 16*cachePage)
	if c := read(t, r, 0, buf); c.N != uint64(len(buf)) || c.Err != nil {
		t.Fatalf("read: %d bytes, %v", c.N, c.Err)
	}
	st := srv.CacheStatus()
	if st.Complete || st.Held != uint64(len(pages))-cachePage || st.Mismatched == 0 {
		t.Errorf("cache status %+v: want incomplete, all but one page held, mismatches counted", st)
	}
	flags, marked := marksOf(t, dir)
	if flags&cacheMarksComplete != 0 || len(marked) != cacheTestPages-1 {
		t.Errorf("marks: flags %#x, %d pages marked; want incomplete, %d", flags, len(marked), cacheTestPages-1)
	}
	for _, p := range marked {
		if p == 5 {
			t.Errorf("the corrupted page 5 is marked")
		}
	}
}

// TestCacheWithoutCacheDir checks that a FileServer without a cache directory
// says it keeps no cache.
func TestCacheWithoutCacheDir(t *testing.T) {
	srv := newTestFileServer(t, s3test.NewStore(t), FileServerOptions{AllowCheckpointReads: true})
	if st := srv.CacheStatus(); st.Enabled {
		t.Errorf("cache status %+v, want none", st)
	}
}

// TestCacheDropsWhatTheQueueCannotHold checks that bytes read while the
// cache's queue is full are not written, so that the read does not wait for
// the cache, and that completing the cache reads them for itself.
func TestCacheDropsWhatTheQueueCannotHold(t *testing.T) {
	defer func(was int) { cacheQueueBytes = was }(cacheQueueBytes)
	cacheQueueBytes = cacheChunkBytes
	// 8 chunks: the cache's writer takes 1, waiting, and its queue 1.
	pages, meta := cacheTestImage(t, 8*cacheChunkBytes/cachePage)
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, pages)
	s.SetObject(checkpointfiles.PagesMetadataFileName, meta)
	dir, cacheDir := cacheTestDir(t, int64(len(pages)))
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, CacheDir: cacheDir})
	r := openRead(t, srv, checkpointfiles.PagesFileName)
	f := srv.cache.file(checkpointfiles.PagesFileName)
	// The writers wait to mark what they write, so the queue fills.
	f.mu.Lock()
	buf := make([]byte, len(pages))
	c := read(t, r, 0, buf)
	f.mu.Unlock()
	if c.N != uint64(len(pages)) || c.Err != nil || !bytes.Equal(buf, pages) {
		t.Fatalf("read: %d bytes, %v", c.N, c.Err)
	}
	st := srv.CacheStatus()
	if st.Dropped == 0 || st.Filled != st.Dropped || !st.Complete {
		t.Errorf("cache status %+v: want bytes dropped, as many filled, complete", st)
	}
	if got, err := os.ReadFile(filepath.Join(dir, checkpointfiles.PagesFileName)); err != nil || !bytes.Equal(got, pages) {
		t.Errorf("cached pages file differs from the object (%v)", err)
	}
}

// TestCacheMarksAFileNoReadTouches checks that a staged pages file that the
// restore never reads, as a layer whose pages are all freed before they are
// loaded, is filled and marked complete all the same.
func TestCacheMarksAFileNoReadTouches(t *testing.T) {
	pages, meta := cacheTestImage(t, cacheTestPages)
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, pages)
	s.SetObject(checkpointfiles.PagesMetadataFileName, meta)
	dir, cacheDir := cacheTestDir(t, int64(len(pages)))
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, CacheDir: cacheDir})
	st := srv.CacheStatus()
	if !st.Complete || st.Files != 1 || st.Filled != uint64(len(pages)) {
		t.Errorf("cache status %+v: want complete, 1 file, all %d bytes filled", st, len(pages))
	}
	if got, err := os.ReadFile(filepath.Join(dir, checkpointfiles.PagesFileName)); err != nil || !bytes.Equal(got, pages) {
		t.Errorf("cached pages file differs from the object (%v)", err)
	}
	if flags, marked := marksOf(t, dir); flags&cacheMarksComplete == 0 || len(marked) != cacheTestPages {
		t.Errorf("marks: flags %#x, %d pages marked; want complete, %d", flags, len(marked), cacheTestPages)
	}
}

// TestCacheWithoutRoom checks that a staged pages file whose blocks cannot be
// allocated, as the node's disk is full, is not cached: not written, and
// without marks.
func TestCacheWithoutRoom(t *testing.T) {
	defer func(was func(int, uint32, int64, int64) error) { fallocate = was }(fallocate)
	fallocate = func(int, uint32, int64, int64) error { return unix.ENOSPC }
	pages, meta := cacheTestImage(t, cacheTestPages)
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, pages)
	s.SetObject(checkpointfiles.PagesMetadataFileName, meta)
	dir, cacheDir := cacheTestDir(t, int64(len(pages)))
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, CacheDir: cacheDir})
	r := openRead(t, srv, checkpointfiles.PagesFileName)
	buf := make([]byte, len(pages))
	if c := read(t, r, 0, buf); c.N != uint64(len(pages)) || c.Err != nil || !bytes.Equal(buf, pages) {
		t.Fatalf("read: %d bytes, %v", c.N, c.Err)
	}
	if st := srv.CacheStatus(); st.Complete || st.Files != 0 || st.Held != 0 {
		t.Errorf("cache status %+v: want incomplete, no file", st)
	}
	if got, err := os.ReadFile(filepath.Join(dir, checkpointfiles.PagesFileName)); err != nil || !bytes.Equal(got, make([]byte, len(pages))) {
		t.Errorf("the pages file not cached was written (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, checkpointfiles.PagesFileName+cacheMarksSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pages file not cached has marks (%v)", err)
	}
}

// TestCacheStatusTimesOut checks that a cache that CacheStatus cannot
// complete in time, as the store stalls, is not complete, and that it is not
// written after: neither by the fill, nor by a read that follows.
func TestCacheStatusTimesOut(t *testing.T) {
	defer func(was time.Duration) { cacheStatusTimeout = was }(cacheStatusTimeout)
	cacheStatusTimeout = 200 * time.Millisecond
	pages, meta := cacheTestImage(t, cacheTestPages)
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, pages)
	s.SetObject(checkpointfiles.PagesMetadataFileName, meta)
	stalled := make(chan struct{})
	s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
		if op != "GetObject" || !strings.HasSuffix(r.URL.Path, "/"+checkpointfiles.PagesFileName) {
			return false
		}
		// The store stalls on the pages file, until released.
		select {
		case <-r.Context().Done():
			return true
		case <-stalled:
			return false
		}
	}
	dir, cacheDir := cacheTestDir(t, int64(len(pages)))
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, CacheDir: cacheDir})
	start := time.Now()
	st := srv.CacheStatus()
	if took := time.Since(start); took > cacheStatusTimeout+time.Second {
		t.Errorf("CacheStatus took %v, more than its %v and a second", took, cacheStatusTimeout)
	}
	if st.Complete || st.Error == "" {
		t.Errorf("cache status %+v: want incomplete, saying why", st)
	}

	// Released, the store answers a read, which the cache does not write.
	close(stalled)
	r := openRead(t, srv, checkpointfiles.PagesFileName)
	buf := make([]byte, len(pages))
	if c := read(t, r, 0, buf); c.N != uint64(len(pages)) || c.Err != nil || !bytes.Equal(buf, pages) {
		t.Fatalf("read: %d bytes, %v", c.N, c.Err)
	}
	srv.cache.pending.Wait()
	if got, err := os.ReadFile(filepath.Join(dir, checkpointfiles.PagesFileName)); err != nil || !bytes.Equal(got, make([]byte, len(pages))) {
		t.Errorf("the cache was written after its status (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, checkpointfiles.PagesFileName+cacheMarksSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cache has marks after its status (%v)", err)
	}
}

// TestCacheWriterYieldsWhileLoading checks that the cache's writer waits
// between its writes while the restore loads, as long as each write took for
// the duty it leaves the restore's reads, and no longer once the restore has
// loaded every page.
func TestCacheWriterYieldsWhileLoading(t *testing.T) {
	var (
		mu    sync.Mutex
		waits []time.Duration
	)
	defer func(was func(time.Duration)) { cacheSleep = was }(cacheSleep)
	cacheSleep = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
	}
	waited := func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), waits...)
	}
	pages, meta := cacheTestImage(t, 4*cacheChunkBytes/cachePage)
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, pages)
	s.SetObject(checkpointfiles.PagesMetadataFileName, meta)
	_, cacheDir := cacheTestDir(t, int64(len(pages)))
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, CacheDir: cacheDir})
	r := openRead(t, srv, checkpointfiles.PagesFileName)

	// Loading: a wait after each write.
	buf := make([]byte, 2*cacheChunkBytes)
	if c := read(t, r, 0, buf); c.N != uint64(len(buf)) || c.Err != nil {
		t.Fatalf("read: %d bytes, %v", c.N, c.Err)
	}
	srv.cache.pending.Wait()
	// The writer waits after a write is done, which pending says: the second
	// wait may begin after pending.Wait returns.
	for deadline := time.Now().Add(10 * time.Second); len(waited()) < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if got := waited(); len(got) != 2 {
		t.Fatalf("the writer waited %v after 2 writes while loading, want once after each", got)
	}

	// Loaded: the writes queued then, behind a writer held, are written
	// without a wait.
	f := srv.cache.file(checkpointfiles.PagesFileName)
	f.mu.Lock()
	if c := read(t, r, int64(len(buf)), buf); c.N != uint64(len(buf)) || c.Err != nil {
		f.mu.Unlock()
		t.Fatalf("read: %d bytes, %v", c.N, c.Err)
	}
	statused := make(chan stateipc.CacheStatus)
	go func() { statused <- srv.CacheStatus() }()
	for !srv.cache.loaded.Load() {
		time.Sleep(time.Millisecond)
	}
	f.mu.Unlock()
	if st := <-statused; !st.Complete {
		t.Errorf("cache status %+v, want complete", st)
	}
	if got := waited(); len(got) != 2 {
		t.Errorf("the writer waited %v, want no wait once loaded", got[2:])
	}
}
