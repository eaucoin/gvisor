// Copyright 2025 The gVisor Authors.
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
	"fmt"
	"io"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/rand"
)

// bytesBufferWriter wraps bytes.Buffer to hide bytes.Buffer.ReadFrom,
// ensuring that io.CopyBuffer performs a buffered copy.
type bytesBufferWriter struct {
	b bytes.Buffer
}

// Write implements io.Writer.Write.
func (w *bytesBufferWriter) Write(src []byte) (int, error) {
	return w.b.Write(src)
}

func TestBufReader(t *testing.T) {
	const dataLen = 32 << 10 // 32 KiB
	data := make([]byte, dataLen)
	_, _ = rand.Read(data)

	for _, maxReadBytes := range []int{dataLen / 64, dataLen / 32, dataLen / 2, dataLen, dataLen * 2} {
		for _, maxParallel := range []int{1, 2, 32, 64} {
			for _, maxBufSize := range []int{0, maxReadBytes - 1, 2*maxReadBytes + 1} {
				for _, readBytes := range []int{maxReadBytes - 2, maxReadBytes - 1, maxReadBytes, maxReadBytes + 1, maxReadBytes * maxParallel, maxReadBytes*maxParallel + 1} {
					t.Run(fmt.Sprintf("MaxReadBytes%d_MaxParallel%d_MaxBufSize%d_ReadBytes%d", maxReadBytes, maxParallel, maxBufSize, readBytes), func(t *testing.T) {
						r, err := NewBufReader(NewIOReader(bytes.NewReader(data), uint64(maxReadBytes), 1 /* maxRanges */, maxParallel), maxBufSize)
						if err != nil {
							t.Fatalf("NewBufReader failed: %v", err)
						}
						defer r.Close()
						var w bytesBufferWriter
						copyBuf := make([]byte, readBytes)
						n, err := io.CopyBuffer(&w, r, copyBuf)
						if n != dataLen || err != nil {
							t.Fatalf("io.CopyBuffer: got (%d, %v), want (%d, nil)", n, err, dataLen)
						}
						if !bytes.Equal(data, w.b.Bytes()) {
							t.Errorf("Bytes differ")
						}
					})
				}
			}
		}
	}
}

// signalingReaderAt is an io.ReaderAt that signals each of its reads on read,
// where read has room.
type signalingReaderAt struct {
	r    io.ReaderAt
	read chan<- struct{}
}

// ReadAt implements io.ReaderAt.ReadAt.
func (r signalingReaderAt) ReadAt(dst []byte, off int64) (int, error) {
	select {
	case r.read <- struct{}{}:
	default:
	}
	return r.r.ReadAt(dst, off)
}

// TestBufReaderPrefetch checks that Prefetch reads before any Read, and that
// Reads after it return the data in order.
func TestBufReaderPrefetch(t *testing.T) {
	const dataLen = 32 << 10 // 32 KiB
	data := make([]byte, dataLen)
	_, _ = rand.Read(data)
	read := make(chan struct{}, 1)
	r, err := NewBufReader(NewIOReader(signalingReaderAt{bytes.NewReader(data), read}, dataLen/8 /* maxReadBytes */, 1 /* maxRanges */, 4 /* maxParallel */), 0 /* size */)
	if err != nil {
		t.Fatalf("NewBufReader failed: %v", err)
	}
	defer r.Close()

	r.Prefetch()
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		t.Fatalf("Prefetch read nothing")
	}
	var w bytesBufferWriter
	if n, err := io.Copy(&w, r); n != dataLen || err != nil {
		t.Fatalf("io.Copy: got (%d, %v), want (%d, nil)", n, err, dataLen)
	}
	if !bytes.Equal(data, w.b.Bytes()) {
		t.Errorf("Bytes differ")
	}
}
