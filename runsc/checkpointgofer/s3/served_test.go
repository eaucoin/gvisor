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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3/s3test"
)

func init() {
	servedFileEvery = 5 * time.Millisecond
}

func TestWithRange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rs    []byteRange
		r     byteRange
		want  []byteRange
		added uint64
	}{
		{name: "into none", r: byteRange{10, 20}, want: []byteRange{{10, 20}}, added: 10},
		{name: "before", rs: []byteRange{{30, 40}}, r: byteRange{10, 20}, want: []byteRange{{10, 20}, {30, 40}}, added: 10},
		{name: "after", rs: []byteRange{{0, 5}}, r: byteRange{10, 20}, want: []byteRange{{0, 5}, {10, 20}}, added: 10},
		{name: "adjoining both", rs: []byteRange{{0, 10}, {20, 30}}, r: byteRange{10, 20}, want: []byteRange{{0, 30}}, added: 10},
		{name: "overlapping one", rs: []byteRange{{0, 15}}, r: byteRange{10, 20}, want: []byteRange{{0, 20}}, added: 5},
		{name: "held", rs: []byteRange{{0, 30}}, r: byteRange{10, 20}, want: []byteRange{{0, 30}}, added: 0},
		{name: "across several", rs: []byteRange{{0, 5}, {10, 12}, {14, 16}, {30, 40}}, r: byteRange{8, 20}, want: []byteRange{{0, 5}, {8, 20}, {30, 40}}, added: 8},
		{name: "covering all", rs: []byteRange{{2, 4}, {6, 8}}, r: byteRange{0, 10}, want: []byteRange{{0, 10}}, added: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, added := withRange(append([]byteRange(nil), tc.rs...), tc.r)
			if !reflect.DeepEqual(got, tc.want) || added != tc.added {
				t.Errorf("withRange(%v, %v) = %v, %d; want %v, %d", tc.rs, tc.r, got, added, tc.want, tc.added)
			}
		})
	}
}

// chunks is a reader that reads at most n bytes at a time, as a response's
// body does.
type chunks struct {
	r *bytes.Reader
	n int
}

// Read implements io.Reader.Read.
func (c *chunks) Read(p []byte) (int, error) {
	return c.r.Read(p[:min(len(p), c.n)])
}

// TestCountedBody checks that the bytes of an answer are counted as they
// arrive, in batches, rather than once the whole answer is read.
func TestCountedBody(t *testing.T) {
	s := newServedBytes(filepath.Join(t.TempDir(), "served"))
	defer s.close()
	body := s.count(&chunks{r: bytes.NewReader(make([]byte, 3*servedFlushBytes/2)), n: 64 << 10}, checkpointfiles.PagesFileName, 4096)
	buf := make([]byte, 64<<10)
	for read := 0; read < 3*servedFlushBytes/2; {
		n, err := body.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		read += n
		s.mu.Lock()
		total := s.total
		s.mu.Unlock()
		if want := uint64(read/servedFlushBytes) * servedFlushBytes; total != want {
			t.Fatalf("%d bytes read, %d counted; want %d", read, total, want)
		}
	}
	body.flush()
	if got, want := s.ranges[checkpointfiles.PagesFileName], []byteRange{{4096, 4096 + 3*servedFlushBytes/2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("counted %v, want %v", got, want)
	}
}

// TestServedFile checks that a FileServer with a ServedFile keeps in it the
// bytes of pages files it read, the image's and its layers', each byte counted
// once, and those of no other file, and the GETs of them it made, those the
// store does not answer too, whether or not its reads of pages files are
// retried while the sandbox runs; that it writes the file at once, then
// replaces it whole as the counts grow, so that it always holds whole counts;
// and that it writes it as it is destroyed.
func TestServedFile(t *testing.T) {
	for _, whileRunning := range []bool{false, true} {
		t.Run(fmt.Sprintf("PagesRetryWhileRunning=%t", whileRunning), func(t *testing.T) {
			testServedFile(t, whileRunning)
		})
	}
}

// testServedFile is TestServedFile with the FileServer's reads of pages files
// retried while the sandbox runs if whileRunning (see
// FileServerOptions.PagesRetryWhileRunning), as a restore's are: a read the
// store does not answer then retries until the store is back, its GETs counted
// meanwhile, and its bytes once they come.
func testServedFile(t *testing.T, whileRunning bool) {
	layerPages := checkpointimage.LayerPath(checkpointimage.Digest{1}, checkpointfiles.PagesFileName)
	s := s3test.NewStore(t)
	s.SetObject(checkpointfiles.PagesFileName, testObject(64<<10))
	s.SetObject(layerPages, testObject(32<<10))
	s.SetObject(checkpointfiles.StateFileName, testObject(16<<10))
	path := filepath.Join(t.TempDir(), "served")
	var storeAway atomicbitops.Bool
	s.Fault = func(w http.ResponseWriter, r *http.Request, op string, n int) bool {
		if !storeAway.Load() {
			return false
		}
		s3test.ResetConnection(t, w, "")
		return true
	}
	srv := newTestFileServer(t, s, FileServerOptions{AllowCheckpointReads: true, ServedFile: path, PagesRetryWhileRunning: whileRunning})
	whole := regexp.MustCompile(`^[0-9]+ [0-9]+\n$`)
	held := func() string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the served file: %v", err)
		}
		if !whole.Match(b) {
			t.Fatalf("the served file holds %q, not a whole count", b)
		}
		return string(b)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for got := held(); got != want; got = held() {
			if time.Now().After(deadline) {
				t.Fatalf("the served file holds %q, want %q", got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("0 0\n")

	// Read meanwhile, as the file is replaced, it is always whole.
	stop := make(chan struct{})
	reading := make(chan struct{})
	go func() {
		defer close(reading)
		for {
			select {
			case <-stop:
				return
			default:
				if b, err := os.ReadFile(path); err != nil || !whole.Match(b) {
					t.Errorf("the served file, read as it is replaced: %q, %v; want a whole count", b, err)
					return
				}
			}
		}
	}()
	pages := openRead(t, srv, checkpointfiles.PagesFileName)
	read(t, pages, 0, make([]byte, 16<<10))
	read(t, pages, 8<<10, make([]byte, 16<<10))
	read(t, openRead(t, srv, layerPages), 0, make([]byte, 32<<10))
	read(t, openRead(t, srv, checkpointfiles.StateFileName), 0, make([]byte, 16<<10))
	gets := 0
	for _, r := range s.Requests() {
		if r.Op == "GetObject" && strings.HasSuffix(r.Key, checkpointfiles.PagesFileName) {
			gets++
		}
	}
	waitFor(fmt.Sprintf("%d %d\n", 24<<10+32<<10, gets))

	// A GET the store does not answer, as one that is away does not, is
	// counted all the same, and no byte with it.
	storeAway.Store(true)
	away := openRead(t, srv, checkpointfiles.PagesFileName)
	away.AddRead(0, 32<<10, nil, memmap.FileRange{}, make([]byte, 4<<10))
	completed := make(chan []stateio.Completion, 1)
	go func() {
		cs, err := away.Wait(nil, 1)
		if err != nil {
			t.Errorf("waiting for a read the store does not answer: %v", err)
		}
		completed <- cs
	}()
	if !whileRunning {
		if cs := <-completed; len(cs) != 1 || cs[0].Err == nil {
			t.Fatalf("a read the store does not answer: %v; want it failed", cs)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		got := held()
		var n, made int
		if _, err := fmt.Sscanf(got, "%d %d\n", &n, &made); err != nil {
			t.Fatalf("the served file holds %q: %v", got, err)
		}
		if n != 24<<10+32<<10 {
			t.Fatalf("the served file holds %q once a read the store does not answer is made: its bytes changed", got)
		}
		if made > gets+1 || (!whileRunning && made > gets) {
			gets = made
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the served file holds %q, want more than %d GETs, a read the store does not answer made", got, gets)
		}
	}
	want := fmt.Sprintf("%d %d\n", 24<<10+32<<10, gets)
	if whileRunning {
		// Retried while the store is away, the read is answered once it is
		// back, its bytes counted then.
		storeAway.Store(false)
		if cs := <-completed; len(cs) != 1 || cs[0].Err != nil || cs[0].N != 4<<10 {
			t.Fatalf("a read retried until the store is back: %v; want its 4096 bytes", cs)
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
			var n, made int
			want = held()
			if _, err := fmt.Sscanf(want, "%d %d\n", &n, &made); err != nil {
				t.Fatalf("the served file holds %q: %v", want, err)
			}
			if n == 24<<10+32<<10+4<<10 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the served file holds %q, want %d bytes, a read retried until the store is back answered", want, 24<<10+32<<10+4<<10)
			}
		}
	}
	close(stop)
	<-reading

	srv.Destroy()
	if got := held(); got != want {
		t.Errorf("the served file once the FileServer is destroyed holds %q, want %q", got, want)
	}
	if left, err := filepath.Glob(path + ".*"); err != nil || len(left) != 0 {
		t.Errorf("files left beside the served file: %v, %v", left, err)
	}
}
