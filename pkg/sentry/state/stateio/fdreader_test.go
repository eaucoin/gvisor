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
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

var _ WaitOrAsyncReader = (*FDReader)(nil)

// TestFDReaderWaitOr checks that FDReader.WaitOr returns when its wake channel
// is readable, without waiting for an inflight read, and otherwise when the
// read completes.
func TestFDReaderWaitOr(t *testing.T) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatalf("Pipe failed: %v", err)
	}
	defer unix.Close(fds[1])
	// NewFDReader takes ownership of the read end.
	r := NewFDReader(int32(fds[0]), 4096, 1, 1)
	defer r.Close()

	// A read at the current offset of the pipe's empty read end blocks until
	// the pipe has data.
	buf := make([]byte, 8)
	r.AddReadv(0 /* id */, -1 /* off */, uint64(len(buf)), nil, []memmap.FileRange{{0, uint64(len(buf))}}, []unix.Iovec{{Base: &buf[0], Len: uint64(len(buf))}})

	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	cs, err := r.WaitOr(nil, wake)
	if err != nil || len(cs) != 0 {
		t.Fatalf("WaitOr with wake readable returned (%v, %v), want no completions", cs, err)
	}

	if _, err := unix.Write(fds[1], []byte("complete")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	cs, err = r.WaitOr(nil, wake)
	if err != nil || len(cs) != 1 || cs[0].ID != 0 || cs[0].N != uint64(len(buf)) || cs[0].Err != nil {
		t.Fatalf("WaitOr returned (%v, %v), want the completion of read 0 of %d bytes", cs, err, len(buf))
	}
	if string(buf) != "complete" {
		t.Errorf("read %q, want %q", buf, "complete")
	}
}
