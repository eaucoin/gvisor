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
	"os"
	"path/filepath"
	"testing"

	"github.com/cespare/xxhash/v2"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
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
	// 8 chunks: the cache's 4 writers take 4, waiting, and its queue 1.
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
