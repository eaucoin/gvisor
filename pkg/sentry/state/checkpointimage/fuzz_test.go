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

package checkpointimage

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"google.golang.org/protobuf/proto"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
)

// seedImages returns valid pages metadata files.
func seedImages(tb testing.TB) [][]byte {
	parent := digestOf("parent")
	ip := testImage(parent)
	ip.WorkingSet = &pb.WorkingSetProto{Version: WorkingSetVersion, Unit: page, Extents: []*pb.FileRangeProto{{Start: 0, End: page}}}
	ip.PrivateMemoryFiles = []*pb.ResourceIDProto{{ContainerName: "c", Path: "/tmp"}}
	withLayers, _ := writeImage(tb, ip, 4*page, testMemoryFile(
		&pb.ExtentProto{Start: 0, End: 4 * page, Layer: 1, Offset: 8 * page},
		&pb.ExtentProto{Start: 4 * page, End: 8 * page, Layer: 0, Offset: 0},
	), testMemoryFile())
	empty, _ := writeImage(tb, testImage(), 0, &pb.MemoryFileMetadataProto{Version: MemoryFileMetadataVersion})
	return [][]byte{withLayers, empty}
}

// checkImage checks that an image accepted by a reader is consistent. hashes
// is true if its page hashes were read.
func checkImage(t *testing.T, img *Image, hashes bool) {
	if err := ValidateImage(img.Proto); err != nil {
		t.Fatalf("accepted image fails validation: %v", err)
	}
	if hashes {
		if err := validateHashes(img); err != nil {
			t.Fatalf("accepted image has invalid page hashes: %v", err)
		}
	}
	layers := img.Layers()
	for i, mf := range img.MemoryFiles {
		body := proto.Clone(mf).(*pb.MemoryFileMetadataProto)
		body.PageHashes = nil
		if err := ValidateMemoryFile(body, img.Proto, false); err != nil {
			t.Fatalf("accepted MemoryFile %d fails validation: %v", i, err)
		}
		if img.CommittedBytes(i) > 1<<30 {
			// Possible without page hashes; too long to walk page by page.
			continue
		}
		m := img.MemoryFileImage(i)
		c := m.Cursor()
		for _, cr := range m.committed {
			for off := cr.start; off < cr.end; off += PageSize {
				e, hasExtent, _, hasHash := c.Lookup(off)
				if hasHash != m.HasHashes() {
					t.Fatalf("MemoryFile %d: committed page %#x has no hash", i, off)
				}
				if hasExtent && e.OffsetOf(off)+PageSize > layers[e.Layer].PagesSize {
					t.Fatalf("MemoryFile %d: page %#x is beyond layer %d", i, off, e.Layer)
				}
			}
		}
	}
}

// FuzzReadImage checks that any input is either refused or read as a
// consistent image.
func FuzzReadImage(f *testing.F) {
	for _, data := range seedImages(f) {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		img, err := ReadImage(bytes.NewReader(data))
		if err != nil {
			return
		}
		metaSize := HeaderSize + img.Header.Length + TrailerSize
		if img.Digest != sha256.Sum256(data[:metaSize]) {
			t.Errorf("digest %v is not the SHA-256 of the file's metadata", img.Digest)
		}
		checkImage(t, img, true)
	})
}

// FuzzParseBody does the same as FuzzReadImage past the checksums, which
// random inputs rarely get through.
func FuzzParseBody(f *testing.F) {
	for _, data := range seedImages(f) {
		hdr, err := ParseHeader(data[:HeaderSize])
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data[HeaderSize : HeaderSize+hdr.Length])
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		img, err := parseBody(body)
		if err != nil {
			return
		}
		checkImage(t, img, false)
	})
}

// FuzzParseHeader checks that headers are parsed exactly as encoded.
func FuzzParseHeader(f *testing.F) {
	h := Header{Major: MajorVersion, Minor: MinorVersion, Length: 1 << 20}
	b := h.Encode()
	f.Add(b[:])
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHeader(data)
		if err != nil {
			return
		}
		if got := h.Encode(); !bytes.Equal(got[:], data) {
			t.Errorf("ParseHeader(%x) = %+v, which encodes as %x", data, h, got)
		}
	})
}
