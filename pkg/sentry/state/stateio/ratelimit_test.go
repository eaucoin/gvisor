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

package stateio

import (
	"bytes"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// TestRateLimitedReader checks that reads issued together complete in the
// order they were issued, each once the bytes of the reads before it and its
// own have been delivered at the reader's rate, with the data read.
func TestRateLimitedReader(t *testing.T) {
	const (
		readBytes = 256 << 10
		reads     = 4
		rate      = 8 << 20 // 31.25 ms per read
	)
	data := make([]byte, reads*readBytes)
	_, _ = rand.Read(data)
	r := NewRateLimitedReader(NewIOReader(bytes.NewReader(data), readBytes, 1 /* maxRanges */, reads /* maxParallel */), rate)
	defer r.Close()

	buf := make([]byte, len(data))
	start := time.Now()
	for i := range reads {
		r.AddRead(i, int64(i*readBytes), nil, memmap.FileRange{}, buf[i*readBytes:(i+1)*readBytes])
	}
	var cs []Completion
	for i := range reads {
		var err error
		cs, err = r.Wait(cs[:0], 1 /* minCompletions */)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		elapsed := time.Since(start)
		if len(cs) != 1 {
			t.Fatalf("Wait returned %d completions, want 1", len(cs))
		}
		if c := cs[0]; c.ID != i || c.N != readBytes || c.Err != nil {
			t.Errorf("completion %d is %+v, want read %d of %d bytes", i, c, i, readBytes)
		}
		if want := time.Duration((i + 1) * readBytes * int(time.Second) / rate); elapsed < want {
			t.Errorf("read %d completed after %v, want at least %v", i, elapsed, want)
		}
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("read data differs from the file's")
	}
}

// TestRateLimitedReaderWaitOr checks that WaitOr returns when woken while a
// read is not due yet, and that the read completes later.
func TestRateLimitedReaderWaitOr(t *testing.T) {
	const (
		readBytes = 1 << 20
		rate      = 4 << 20 // 250 ms per read
	)
	data := make([]byte, readBytes)
	r := NewRateLimitedReader(NewIOReader(bytes.NewReader(data), readBytes, 1 /* maxRanges */, 1 /* maxParallel */), rate)
	defer r.Close()

	start := time.Now()
	r.AddRead(0, 0, nil, memmap.FileRange{}, make([]byte, readBytes))
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	cs, err := r.WaitOr(nil, wake)
	if err != nil {
		t.Fatalf("WaitOr: %v", err)
	}
	if len(cs) != 0 {
		t.Fatalf("WaitOr returned %+v before the read was due", cs)
	}
	if len(wake) != 0 {
		t.Errorf("WaitOr did not receive from wake")
	}
	if cs, err = r.Wait(cs, 1 /* minCompletions */); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(cs) != 1 || cs[0].N != readBytes {
		t.Errorf("Wait returned %+v, want the read", cs)
	}
	if elapsed, want := time.Since(start), time.Duration(readBytes*int(time.Second)/rate); elapsed < want {
		t.Errorf("read completed after %v, want at least %v", elapsed, want)
	}
}

// TestRateLimitedWriter checks that writes issued together complete in the
// order they were issued, each once the bytes of the writes before it and its
// own have been stored at the writer's rate, and write their data.
func TestRateLimitedWriter(t *testing.T) {
	const (
		writeBytes = 256 << 10
		writes     = 4
		rate       = 8 << 20 // 31.25 ms per write
	)
	data := make([]byte, writes*writeBytes)
	_, _ = rand.Read(data)
	var file bytes.Buffer
	w := NewRateLimitedWriter(NewIOWriter(&file, writeBytes, 1 /* maxRanges */, writes /* maxParallel */), rate)
	defer w.Close()

	start := time.Now()
	for i := range writes {
		w.AddWrite(i, nil, memmap.FileRange{}, data[i*writeBytes:(i+1)*writeBytes])
	}
	var cs []Completion
	for i := range writes {
		var err error
		cs, err = w.Wait(cs[:0], 1 /* minCompletions */)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		elapsed := time.Since(start)
		if len(cs) != 1 {
			t.Fatalf("Wait returned %d completions, want 1", len(cs))
		}
		if c := cs[0]; c.ID != i || c.N != writeBytes || c.Err != nil {
			t.Errorf("completion %d is %+v, want write %d of %d bytes", i, c, i, writeBytes)
		}
		if want := time.Duration((i + 1) * writeBytes * int(time.Second) / rate); elapsed < want {
			t.Errorf("write %d completed after %v, want at least %v", i, elapsed, want)
		}
	}
	if err := w.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !bytes.Equal(file.Bytes(), data) {
		t.Errorf("written data differs from the data written")
	}
}
