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
	"encoding/binary"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// seekableFile returns data as a seekable zstd file of frames of frameBytes.
func seekableFile(t *testing.T, data []byte, frameBytes int) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	var file, table []byte
	frames := 0
	for off := 0; off < len(data); off += frameBytes {
		frame := enc.EncodeAll(data[off:min(off+frameBytes, len(data))], nil)
		file = append(file, frame...)
		table = binary.LittleEndian.AppendUint32(table, uint32(len(frame)))
		table = binary.LittleEndian.AppendUint32(table, uint32(min(frameBytes, len(data)-off)))
		frames++
	}
	file = binary.LittleEndian.AppendUint32(file, seekableSkippableMagic)
	file = binary.LittleEndian.AppendUint32(file, uint32(len(table)+seekableFooterBytes))
	file = append(file, table...)
	file = binary.LittleEndian.AppendUint32(file, uint32(frames))
	file = append(file, 0)
	return binary.LittleEndian.AppendUint32(file, seekableMagic)
}

func TestSeekTable(t *testing.T) {
	data := bytes.Repeat([]byte("checkpoint pages "), 10000) // 170000 bytes
	file := seekableFile(t, data, 65536)
	physical := int64(len(file))

	// A suffix too short for the table says how much it needs.
	tab, need, err := parseSeekTable(file[len(file)-seekableFooterBytes:], physical)
	if err != nil || tab != nil || need != 8+3*8+seekableFooterBytes {
		t.Fatalf("parseSeekTable(footer) = %v, %d, %v; want nil, %d, nil", tab, need, err, 8+3*8+seekableFooterBytes)
	}
	tab, _, err = parseSeekTable(file, physical)
	if err != nil {
		t.Fatal(err)
	}
	if got := tab.size(); got != int64(len(data)) {
		t.Errorf("size() = %d, want %d", got, len(data))
	}
	for _, c := range []struct {
		off   int64
		frame int
	}{{0, 0}, {65535, 0}, {65536, 1}, {131072, 2}, {169999, 2}} {
		if got := tab.frameAt(c.off); got != c.frame {
			t.Errorf("frameAt(%d) = %d, want %d", c.off, got, c.frame)
		}
	}
	// The frames between the table's offsets decode to the bytes they say.
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if err := dec.Reset(bytes.NewReader(file[tab.physical[1]:tab.physical[3]])); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[65536:]) {
		t.Errorf("frames 1-2 decode to %d bytes unlike the data's %d", len(got), len(data)-65536)
	}

	// A file whose frames do not add up to its size, or without the magic, is refused.
	if _, _, err := parseSeekTable(file, physical+1); err == nil {
		t.Errorf("parseSeekTable of a longer file succeeded")
	}
	bad := bytes.Clone(file)
	bad[len(bad)-1] ^= 0xff
	if _, _, err := parseSeekTable(bad, physical); err == nil {
		t.Errorf("parseSeekTable without the magic succeeded")
	}
	bad = bytes.Clone(file)
	bad[len(bad)-5] = 0x01
	if _, _, err := parseSeekTable(bad, physical); err == nil {
		t.Errorf("parseSeekTable with a reserved descriptor bit succeeded")
	}
}
