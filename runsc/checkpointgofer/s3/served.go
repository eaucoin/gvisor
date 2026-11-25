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
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// servedFileEvery is how often, at most, a servedBytes writes its file again
// while its count changes. It is a variable for tests.
var servedFileEvery = 250 * time.Millisecond

// servedFlushBytes is how many bytes of an answer a counted body reads before
// it counts them, as it does at its end.
const servedFlushBytes = 1 << 20

// servedBytes counts the bytes of pages files that a FileServer read from the
// store, and the GETs of them it made, and keeps both in a file
// (FileServerOptions.ServedFile).
type servedBytes struct {
	path string

	mu sync.Mutex
	// ranges holds the byte ranges read of each file, by the path the file is
	// served at, sorted and apart; total is how many bytes they hold.
	ranges map[string][]byteRange
	total  uint64
	// gets is how many GETs of pages files were made, answered or not.
	gets uint64

	// stop is closed by close, once (stopOnce); done is closed once the
	// goroutine that writes the file has written it for the last time.
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// byteRange is the range [start, end) of a file.
type byteRange struct {
	start, end int64
}

// newServedBytes returns a servedBytes that keeps its counts in the file at
// path, written before it returns, then within servedFileEvery of each
// change, until close.
func newServedBytes(path string) *servedBytes {
	s := &servedBytes{
		path:   path,
		ranges: make(map[string][]byteRange),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	s.write(servedCounts{})
	go s.writeMain()
	return s
}

// add counts the bytes [off, off+n) of the file served at p.
func (s *servedBytes) add(p string, off int64, n uint64) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var added uint64
	s.ranges[p], added = withRange(s.ranges[p], byteRange{off, off + int64(n)})
	s.total += added
}

// get counts a GET of a pages file, made as it is made, whether or not the
// store answers it: what tells, while the store is away, that the reads go
// on, as their bytes do not.
func (s *servedBytes) get() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
}

// servedCounts is what a servedBytes keeps in its file.
type servedCounts struct {
	bytes, gets uint64
}

// counts returns s's counts.
func (s *servedBytes) counts() servedCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return servedCounts{s.total, s.gets}
}

// withRange returns rs, sorted and apart, with r added, and the number of
// bytes of r that rs did not hold.
func withRange(rs []byteRange, r byteRange) ([]byteRange, uint64) {
	// The ranges that r touches, or adjoins, are merged with it.
	first, _ := slices.BinarySearchFunc(rs, r.start, func(e byteRange, start int64) int {
		if e.end < start {
			return -1
		}
		return 1
	})
	last := first
	held := uint64(0)
	merged := r
	for ; last < len(rs) && rs[last].start <= r.end; last++ {
		e := rs[last]
		held += uint64(min(e.end, r.end) - max(e.start, r.start))
		merged.start = min(merged.start, e.start)
		merged.end = max(merged.end, e.end)
	}
	return slices.Replace(rs, first, last, merged), uint64(r.end-r.start) - held
}

// count returns a reader of body, the answer to a GET of the file served at
// p from off, that counts the bytes it reads as they arrive, in batches of
// servedFlushBytes; its caller counts the rest with flush once it has read
// what it reads.
func (s *servedBytes) count(body io.Reader, p string, off int64) *countedBody {
	return &countedBody{Reader: body, served: s, path: p, off: off}
}

// countedBody is a body that count returns.
type countedBody struct {
	io.Reader
	served *servedBytes
	path   string
	// off is where the next byte that the body counts is in the file;
	// pending is how many bytes it read past it and has not counted yet.
	off     int64
	pending uint64
}

// Read implements io.Reader.Read.
func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.pending += uint64(n)
	if b.pending >= servedFlushBytes {
		b.flush()
	}
	return n, err
}

// flush counts the bytes read and not counted yet.
func (b *countedBody) flush() {
	b.served.add(b.path, b.off, b.pending)
	b.off += int64(b.pending)
	b.pending = 0
}

// writeMain writes s's file as newServedBytes says.
func (s *servedBytes) writeMain() {
	defer close(s.done)
	var written servedCounts
	ticker := time.NewTicker(servedFileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			s.write(s.counts())
			return
		case <-ticker.C:
			if c := s.counts(); c != written {
				s.write(c)
				written = c
			}
		}
	}
}

// write replaces s's file with one that holds c, written beside it and
// renamed over it; a failure is logged.
func (s *servedBytes) write(c servedCounts) {
	if err := writeFileAtomically(s.path, []byte(fmt.Sprintf("%d %d\n", c.bytes, c.gets))); err != nil {
		log.Warningf("s3: cannot write the bytes of pages read, %d, and the GETs of them, %d, to %s: %v", c.bytes, c.gets, s.path, err)
	}
}

// writeFileAtomically replaces the file at path with one holding b, written
// beside it, which whoever reads path never sees half written.
func writeFileAtomically(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("renaming %s: %w", f.Name(), err)
	}
	return nil
}

// close writes s's file for the last time, and stops writing it. It is
// idempotent, as FileServer.Destroy is.
func (s *servedBytes) close() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}
