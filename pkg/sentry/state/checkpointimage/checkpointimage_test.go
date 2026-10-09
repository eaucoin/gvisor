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
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
)

const page = PageSize

// testMemoryFile returns the metadata of a MemoryFile whose known-committed
// pages are [0, 8) and [16, 20), with the given extents and a hash per page.
func testMemoryFile(extents ...*pb.ExtentProto) *pb.MemoryFileMetadataProto {
	mf := &pb.MemoryFileMetadataProto{
		Version: MemoryFileMetadataVersion,
		Chunks:  []*pb.ChunkInfoProto{{}},
		MemAcct: []*pb.MemAcctRangeProto{
			{Start: 0, End: 4 * page, KnownCommitted: true},
			{Start: 4 * page, End: 8 * page, Kind: 1, KnownCommitted: true},
			{Start: 8 * page, End: 16 * page},
			{Start: 16 * page, End: 20 * page, KnownCommitted: true},
		},
		Extents: extents,
	}
	for i := 0; i < 12; i++ {
		mf.PageHashes = binary.LittleEndian.AppendUint64(mf.PageHashes, uint64(100+i))
	}
	return mf
}

func digestOf(s string) Digest {
	return sha256.Sum256([]byte(s))
}

func testImage(layers ...Digest) *pb.ImageProto {
	ip := &pb.ImageProto{
		Layers:           []*pb.LayerProto{{}},
		PageHash:         PageHashXXH64,
		PageHashesDigest: make([]byte, sha256.Size),
	}
	for _, d := range layers {
		ip.Layers = append(ip.Layers, &pb.LayerProto{Digest: d[:], PagesSize: 64 * page})
	}
	return ip
}

// writeImage writes an image with the given MemoryFiles and returns its pages
// metadata file.
func writeImage(t testing.TB, ip *pb.ImageProto, pagesSize uint64, mfs ...*pb.MemoryFileMetadataProto) ([]byte, *Image) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, ip)
	for _, mf := range mfs {
		if err := WriteRecord(w, mf); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	img, err := w.Finish(pagesSize)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return buf.Bytes(), img
}

func TestHeader(t *testing.T) {
	h := Header{Major: MajorVersion, Minor: MinorVersion, Length: 12345}
	b := h.Encode()
	got, err := ParseHeader(b[:])
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if got != h {
		t.Errorf("ParseHeader = %+v, want %+v", got, h)
	}

	for _, tc := range []struct {
		name string
		h    Header
	}{
		{"older major", Header{Major: MajorVersion - 1, Length: 1}},
		{"newer major", Header{Major: MajorVersion + 1, Length: 1}},
		{"newer minor", Header{Major: MajorVersion, Minor: MinorVersion + 1, Length: 1}},
		{"flags", Header{Major: MajorVersion, Flags: 1, Length: 1}},
		{"too long", Header{Major: MajorVersion, Length: MaxBodySize + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.h.Encode()
			if _, err := ParseHeader(b[:]); !errors.Is(err, ErrFormat) {
				t.Errorf("ParseHeader(%+v) = %v, want ErrFormat", tc.h, err)
			}
		})
	}

	t.Run("bad magic", func(t *testing.T) {
		b := h.Encode()
		b[0] = 'G'
		if _, err := ParseHeader(b[:]); !errors.Is(err, ErrFormat) {
			t.Errorf("ParseHeader = %v, want ErrFormat", err)
		}
	})
	t.Run("bad CRC", func(t *testing.T) {
		b := h.Encode()
		b[16]++ // length
		if _, err := ParseHeader(b[:]); !errors.Is(err, ErrFormat) {
			t.Errorf("ParseHeader = %v, want ErrFormat", err)
		}
	})
}

func TestDigest(t *testing.T) {
	d := digestOf("x")
	got, err := ParseDigest(d.String())
	if err != nil || got != d {
		t.Errorf("ParseDigest(%q) = %v, %v; want %v", d.String(), got, err, d)
	}
	for _, s := range []string{"", "00", strings.Repeat("g", 64), d.String() + "00"} {
		if _, err := ParseDigest(s); err == nil {
			t.Errorf("ParseDigest(%q) succeeded", s)
		}
	}
}

func TestParseLayerPath(t *testing.T) {
	d := digestOf("x")
	p := LayerPath(d, checkpointfiles.PagesFileName)
	if gotD, gotName, ok := ParseLayerPath(p); !ok || gotD != d || gotName != checkpointfiles.PagesFileName {
		t.Errorf("ParseLayerPath(%q) = %v, %q, %t; want %v, %q, true", p, gotD, gotName, ok, d, checkpointfiles.PagesFileName)
	}
	for _, p := range []string{
		checkpointfiles.PagesFileName,
		"layers/" + d.String(),
		"layers/" + d.String() + "/",
		"layers/" + d.String() + "/fs/" + checkpointfiles.PagesFileName,
		"layers/" + strings.ToUpper(d.String()) + "/" + checkpointfiles.PagesFileName,
		"layers/00/" + checkpointfiles.PagesFileName,
		"/layers/" + d.String() + "/" + checkpointfiles.PagesFileName,
		"other/" + d.String() + "/" + checkpointfiles.PagesFileName,
	} {
		if _, _, ok := ParseLayerPath(p); ok {
			t.Errorf("ParseLayerPath(%q) succeeded", p)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	parent := digestOf("parent")
	ip := testImage(parent)
	ip.PrivateMemoryFiles = []*pb.ResourceIDProto{{ContainerName: "c", Path: "/tmp"}}
	ip.WorkingSet = &pb.WorkingSetProto{
		Version:  WorkingSetVersion,
		Unit:     page,
		WindowNs: 3e9,
		Extents:  []*pb.FileRangeProto{{Start: 16 * page, End: 17 * page}, {Start: 0, End: page}},
	}
	main := testMemoryFile(
		&pb.ExtentProto{Start: 0, End: 2 * page, Layer: 1, Offset: 10 * page},
		&pb.ExtentProto{Start: 2 * page, End: 8 * page, Layer: 0, Offset: 4 * page},
		&pb.ExtentProto{Start: 16 * page, End: 20 * page, Layer: 0, Offset: 0},
	)
	private := testMemoryFile()
	data, written := writeImage(t, ip, 10*page, main, private)

	r := bytes.NewReader(data)
	img, err := ReadMetadata(r)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	// The metadata ends where the page hashes start: 12 pages of each
	// MemoryFile.
	metaSize := len(data) - 2*12*8
	if r.Len() != 2*12*8 {
		t.Errorf("ReadMetadata left %d bytes, want the %d bytes of page hashes", r.Len(), 2*12*8)
	}
	if want := Digest(sha256.Sum256(data[:metaSize])); img.Digest != want || written.Digest != want {
		t.Errorf("digests: read %v, written %v, want %v", img.Digest, written.Digest, want)
	}
	if img.Header.Length != uint64(metaSize-HeaderSize-TrailerSize) {
		t.Errorf("header length %d for metadata of %d bytes", img.Header.Length, metaSize)
	}
	if !proto.Equal(img.Proto, written.Proto) {
		t.Errorf("image metadata: read %v, written %v", img.Proto, written.Proto)
	}
	if got := img.Layers(); len(got) != 2 || got[0].PagesSize != 10*page || got[1].Digest != parent {
		t.Errorf("Layers() = %+v", got)
	}
	if got, want := img.CommittedBytes(0), uint64(12*page); got != want {
		t.Errorf("CommittedBytes(0) = %d, want %d", got, want)
	}
	// The page hashes are not read yet.
	if img.MemoryFileImage(0).HasHashes() {
		t.Errorf("MemoryFileImage(0) has page hashes before ReadHashes")
	}

	// The records reader yields the records as written, without their page
	// hashes.
	records := img.MemoryFileRecords()
	for i, want := range []*pb.MemoryFileMetadataProto{main, private} {
		var got pb.MemoryFileMetadataProto
		if err := ReadRecord(records, &got, MaxBodySize); err != nil {
			t.Fatalf("ReadRecord %d: %v", i, err)
		}
		want = proto.Clone(want).(*pb.MemoryFileMetadataProto)
		want.PageHashes = nil
		if !proto.Equal(&got, want) {
			t.Errorf("record %d = %v, want %v", i, &got, want)
		}
	}
	if records.Len() != 0 {
		t.Errorf("%d bytes after the records", records.Len())
	}

	if err := img.ReadHashes(r); err != nil {
		t.Fatalf("ReadHashes: %v", err)
	}
	for i, want := range []*pb.MemoryFileMetadataProto{main, private} {
		if !proto.Equal(img.MemoryFiles[i], want) || !proto.Equal(written.MemoryFiles[i], want) {
			t.Errorf("MemoryFile %d: read %v, written %v, want %v", i, img.MemoryFiles[i], written.MemoryFiles[i], want)
		}
	}
}

// TestReadHashesAsync checks that MemoryFileImage waits for the page hashes
// that ReadHashesAsync reads.
func TestReadHashesAsync(t *testing.T) {
	data, _ := writeImage(t, testImage(), 0, testMemoryFile())
	pr, pw := io.Pipe()
	go func() {
		pw.Write(data)
		pw.Close()
	}()
	img, err := ReadMetadata(pr)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	closed := make(chan struct{})
	img.ReadHashesAsync(pr, func() { close(closed) })
	if !img.MemoryFileImage(0).HasHashes() {
		t.Errorf("MemoryFileImage(0) has no page hashes after ReadHashesAsync")
	}
	<-closed

	// A corrupt page hashes section leaves the image without page hashes.
	data[len(data)-1] ^= 1
	r := bytes.NewReader(data)
	img, err = ReadMetadata(r)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	img.ReadHashesAsync(r, func() {})
	if img.MemoryFileImage(0).HasHashes() {
		t.Errorf("MemoryFileImage(0) has page hashes from a corrupt section")
	}
}

// TestPruneLayers checks that Finish drops layers that no extent refers to,
// orders the others by first reference, and renumbers extents.
func TestPruneLayers(t *testing.T) {
	a, b, c := digestOf("a"), digestOf("b"), digestOf("c")
	ip := testImage(a, b, c)
	main := testMemoryFile(
		&pb.ExtentProto{Start: 0, End: page, Layer: 3, Offset: 0},
		&pb.ExtentProto{Start: page, End: 2 * page, Layer: 1, Offset: page},
		&pb.ExtentProto{Start: 2 * page, End: 3 * page, Layer: 3, Offset: 2 * page},
	)
	_, img := writeImage(t, ip, 0, main)
	layers := img.Layers()
	if len(layers) != 3 || layers[1].Digest != c || layers[2].Digest != a {
		t.Fatalf("layers = %+v, want [self, c, a]", layers)
	}
	var got []uint32
	for _, e := range img.MemoryFiles[0].Extents {
		got = append(got, e.Layer)
	}
	if want := []uint32{1, 2, 1}; !equal(got, want) {
		t.Errorf("extent layers = %v, want %v", got, want)
	}
}

func equal[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestValidation(t *testing.T) {
	ext := func(start, end uint64, layer uint32, off uint64) *pb.ExtentProto {
		return &pb.ExtentProto{Start: start * page, End: end * page, Layer: layer, Offset: off * page}
	}
	for _, tc := range []struct {
		name   string
		ip     func(*pb.ImageProto)
		mf     func(*pb.MemoryFileMetadataProto)
		inline bool
	}{
		{name: "version 1", mf: func(mf *pb.MemoryFileMetadataProto) { mf.Version = 1 }},
		{name: "overlapping extents", mf: func(mf *pb.MemoryFileMetadataProto) {
			mf.Extents = append(mf.Extents, ext(0, 2, 0, 0), ext(1, 3, 0, 4))
		}},
		{name: "unsorted extents", mf: func(mf *pb.MemoryFileMetadataProto) {
			mf.Extents = append(mf.Extents, ext(2, 3, 0, 0), ext(0, 1, 0, 1))
		}},
		{name: "empty extent", mf: func(mf *pb.MemoryFileMetadataProto) { mf.Extents = append(mf.Extents, ext(1, 1, 0, 0)) }},
		{name: "unaligned extent", mf: func(mf *pb.MemoryFileMetadataProto) {
			mf.Extents = append(mf.Extents, &pb.ExtentProto{Start: 1, End: page})
		}},
		{name: "unaligned offset", mf: func(mf *pb.MemoryFileMetadataProto) {
			mf.Extents = append(mf.Extents, &pb.ExtentProto{Start: 0, End: page, Offset: 1})
		}},
		{name: "unknown layer", mf: func(mf *pb.MemoryFileMetadataProto) { mf.Extents = append(mf.Extents, ext(0, 1, 2, 0)) }},
		{name: "beyond layer", mf: func(mf *pb.MemoryFileMetadataProto) { mf.Extents = append(mf.Extents, ext(0, 2, 1, 63)) }},
		{name: "offset overflow", mf: func(mf *pb.MemoryFileMetadataProto) {
			mf.Extents = append(mf.Extents, &pb.ExtentProto{Start: 0, End: 2 * page, Layer: 1, Offset: ^uint64(0) &^ (page - 1)})
		}},
		{name: "extent in uncommitted memory", mf: func(mf *pb.MemoryFileMetadataProto) { mf.Extents = append(mf.Extents, ext(7, 9, 0, 0)) }},
		{name: "extent beyond memory", mf: func(mf *pb.MemoryFileMetadataProto) { mf.Extents = append(mf.Extents, ext(19, 21, 0, 0)) }},
		{name: "page hashes in the body", mf: func(mf *pb.MemoryFileMetadataProto) { mf.PageHashes = make([]byte, 12*8) }},
		{name: "page hashes digest without hash", ip: func(ip *pb.ImageProto) { ip.PageHash = 0 }},
		{name: "short page hashes digest", ip: func(ip *pb.ImageProto) { ip.PageHashesDigest = ip.PageHashesDigest[1:] }},
		{name: "unknown hash", ip: func(ip *pb.ImageProto) { ip.PageHash = 2 }},
		{name: "overlapping accounting", mf: func(mf *pb.MemoryFileMetadataProto) { mf.MemAcct[1].Start = 3 * page }},
		{name: "unaligned accounting", mf: func(mf *pb.MemoryFileMetadataProto) { mf.MemAcct[3].End-- }},
		{name: "extents inline", inline: true, mf: func(mf *pb.MemoryFileMetadataProto) { mf.Extents = append(mf.Extents, ext(0, 1, 0, 0)) }},
		{name: "no layers", ip: func(ip *pb.ImageProto) { ip.Layers = nil }},
		{name: "layer 0 digest", ip: func(ip *pb.ImageProto) { ip.Layers[0].Digest = ip.Layers[1].Digest }},
		{name: "short digest", ip: func(ip *pb.ImageProto) { ip.Layers[1].Digest = ip.Layers[1].Digest[1:] }},
		{name: "duplicate layer", ip: func(ip *pb.ImageProto) { ip.Layers = append(ip.Layers, ip.Layers[1]) }},
		{name: "unaligned pages size", ip: func(ip *pb.ImageProto) { ip.Layers[1].PagesSize++ }},
		{name: "working set version", ip: func(ip *pb.ImageProto) { ip.WorkingSet = &pb.WorkingSetProto{Version: 2, Unit: page} }},
		{name: "working set unit", ip: func(ip *pb.ImageProto) { ip.WorkingSet = &pb.WorkingSetProto{Version: 1, Unit: 3 * page} }},
		{name: "working set extent", ip: func(ip *pb.ImageProto) {
			ip.WorkingSet = &pb.WorkingSetProto{Version: 1, Unit: page, Extents: []*pb.FileRangeProto{{Start: page, End: page}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := testImage(digestOf("parent"))
			mf := testMemoryFile()
			mf.PageHashes = nil // as in the body
			if tc.ip != nil {
				tc.ip(ip)
			}
			if tc.mf != nil {
				tc.mf(mf)
			}
			err := ValidateImage(ip)
			if err == nil {
				err = ValidateMemoryFile(mf, ip, tc.inline)
			}
			if !errors.Is(err, ErrFormat) {
				t.Errorf("validation = %v, want ErrFormat", err)
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		ip := testImage(digestOf("parent"))
		ip.Layers[0].PagesSize = 6 * page
		mf := testMemoryFile(ext(0, 2, 1, 62), ext(2, 8, 0, 0), ext(16, 20, 1, 0))
		mf.PageHashes = nil // as in the body
		if err := ValidateImage(ip); err != nil {
			t.Fatalf("ValidateImage: %v", err)
		}
		if err := ValidateMemoryFile(mf, ip, false); err != nil {
			t.Errorf("ValidateMemoryFile: %v", err)
		}
	})
}

// TestFinishValidates checks that an invalid image is not written.
func TestFinishValidates(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, testImage())
	mf := testMemoryFile(&pb.ExtentProto{Start: 0, End: page, Offset: page})
	if err := WriteRecord(w, mf); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	// The pages file is one page long, so the extent is beyond it.
	if _, err := w.Finish(page); !errors.Is(err, ErrFormat) {
		t.Errorf("Finish = %v, want ErrFormat", err)
	}
	if buf.Len() != 0 {
		t.Errorf("Finish wrote %d bytes", buf.Len())
	}
}

func TestReadImageRejects(t *testing.T) {
	data, _ := writeImage(t, testImage(), 0, testMemoryFile())
	// The page hashes section: 12 pages.
	metaSize := len(data) - 12*8
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"truncated header", func(b []byte) []byte { return b[:HeaderSize-1] }},
		{"truncated body", func(b []byte) []byte { return b[:HeaderSize+10] }},
		{"truncated trailer", func(b []byte) []byte { return b[:metaSize-1] }},
		{"flipped body bit", func(b []byte) []byte { b[HeaderSize+20] ^= 1; return b }},
		{"flipped trailer bit", func(b []byte) []byte { b[metaSize-1] ^= 1; return b }},
		{"truncated page hashes", func(b []byte) []byte { return b[:len(b)-1] }},
		{"flipped page hash bit", func(b []byte) []byte { b[len(b)-1] ^= 1; return b }},
		{"trailing data", func(b []byte) []byte { return append(b, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.edit(bytes.Clone(data))
			if _, err := ReadImage(bytes.NewReader(b)); err == nil {
				t.Errorf("ReadImage succeeded")
			}
		})
	}
}

func TestMemoryFileImage(t *testing.T) {
	mf := testMemoryFile(
		&pb.ExtentProto{Start: page, End: 3 * page, Layer: 1, Offset: 10 * page},
		&pb.ExtentProto{Start: 17 * page, End: 19 * page, Layer: 0, Offset: 0},
	)
	m := NewMemoryFileImage(mf)
	if !m.HasHashes() {
		t.Fatalf("HasHashes() = false")
	}
	// Page hashes are 100 + the index of the page among committed pages.
	for _, tc := range []struct {
		page      uint64
		hash      uint64
		hasHash   bool
		hasExtent bool
		layer     uint32
		off       uint64
	}{
		{page: 0, hash: 100, hasHash: true},
		{page: 1, hash: 101, hasHash: true, hasExtent: true, layer: 1, off: 10 * page},
		{page: 2, hash: 102, hasHash: true, hasExtent: true, layer: 1, off: 11 * page},
		{page: 7, hash: 107, hasHash: true},
		{page: 8},
		{page: 15},
		{page: 16, hash: 108, hasHash: true},
		{page: 18, hash: 110, hasHash: true, hasExtent: true, layer: 0, off: page},
		{page: 20},
	} {
		off := tc.page * page
		h, ok := m.HashAt(off)
		if ok != tc.hasHash || h != tc.hash {
			t.Errorf("HashAt(page %d) = %d, %t; want %d, %t", tc.page, h, ok, tc.hash, tc.hasHash)
		}
		e, ok := m.ExtentAt(off)
		if ok != tc.hasExtent || (ok && (e.Layer != tc.layer || e.OffsetOf(off) != tc.off)) {
			t.Errorf("ExtentAt(page %d) = %+v, %t", tc.page, e, ok)
		}
	}
	// A cursor agrees with HashAt and ExtentAt.
	c := m.Cursor()
	for off := uint64(0); off < 21*page; off += page {
		e, hasExtent, h, hasHash := c.Lookup(off)
		wantE, wantHasExtent := m.ExtentAt(off)
		wantH, wantHasHash := m.HashAt(off)
		if e != wantE || hasExtent != wantHasExtent || h != wantH || hasHash != wantHasHash {
			t.Errorf("Lookup(%#x) = %+v, %t, %d, %t; want %+v, %t, %d, %t", off, e, hasExtent, h, hasHash, wantE, wantHasExtent, wantH, wantHasHash)
		}
	}

	// Without page hashes, a MemoryFile with known-committed pages lacks
	// them; one without has all it can have.
	mf.PageHashes = nil
	if NewMemoryFileImage(mf).HasHashes() {
		t.Errorf("HasHashes() = true without page hashes")
	}
	if !NewMemoryFileImage(&pb.MemoryFileMetadataProto{}).HasHashes() {
		t.Errorf("HasHashes() = false for a MemoryFile without known-committed pages")
	}
}

// writeImageDir writes an image directory with a pages metadata file and a
// pages file of pagesSize bytes, and returns its digest.
func writeImageDir(t *testing.T, dir string, ip *pb.ImageProto, pagesSize uint64) Digest {
	t.Helper()
	data, img := writeImage(t, ip, pagesSize, testMemoryFile())
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointfiles.PagesMetadataFileName), data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointfiles.PagesFileName), make([]byte, pagesSize), 0644); err != nil {
		t.Fatal(err)
	}
	return img.Digest
}

func TestFindLayers(t *testing.T) {
	root := t.TempDir()
	// Three images to use as layers; their contents differ by pages size.
	inImage := writeImageDir(t, filepath.Join(root, "a"), testImage(), page)
	inStore := writeImageDir(t, filepath.Join(root, "b"), testImage(), 2*page)
	direct := writeImageDir(t, filepath.Join(root, "c"), testImage(), 3*page)

	image := filepath.Join(root, "image")
	if err := os.MkdirAll(filepath.Join(image, LayersDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(image, LayersDir, inImage.String())); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "b"), filepath.Join(store, inStore.String())); err != nil {
		t.Fatal(err)
	}

	ip := testImage(inImage, inStore, direct)
	ip.Layers[1].PagesSize = page
	ip.Layers[2].PagesSize = 2 * page
	ip.Layers[3].PagesSize = 3 * page
	mf := testMemoryFile(
		&pb.ExtentProto{Start: 0, End: page, Layer: 1},
		&pb.ExtentProto{Start: page, End: 2 * page, Layer: 2},
		&pb.ExtentProto{Start: 2 * page, End: 3 * page, Layer: 3},
	)
	_, img := writeImage(t, ip, 0, mf)

	dirs, err := FindLayers(img, image, []string{store, filepath.Join(root, "c")})
	if err != nil {
		t.Fatalf("FindLayers: %v", err)
	}
	want := []string{
		filepath.Join(image, LayersDir, inImage.String()),
		filepath.Join(store, inStore.String()),
		filepath.Join(root, "c"),
	}
	if !equal(dirs, want) {
		t.Errorf("FindLayers = %q, want %q", dirs, want)
	}

	t.Run("missing", func(t *testing.T) {
		if _, err := FindLayers(img, image, []string{store}); !errors.Is(err, ErrLayerNotFound) {
			t.Errorf("FindLayers = %v, want ErrLayerNotFound", err)
		}
	})
	t.Run("wrong digest", func(t *testing.T) {
		// An image directory whose digest is not a layer's is not one.
		other := writeImageDir(t, filepath.Join(root, "other"), testImage(), 4*page)
		if other == direct {
			t.Fatal("digests collide")
		}
		if _, err := FindLayers(img, image, []string{store, filepath.Join(root, "other")}); !errors.Is(err, ErrLayerNotFound) {
			t.Errorf("FindLayers = %v, want ErrLayerNotFound", err)
		}
	})
	t.Run("wrong pages size", func(t *testing.T) {
		if err := os.Truncate(filepath.Join(root, "c", checkpointfiles.PagesFileName), page); err != nil {
			t.Fatal(err)
		}
		if _, err := FindLayers(img, image, []string{store, filepath.Join(root, "c")}); !errors.Is(err, ErrFormat) {
			t.Errorf("FindLayers = %v, want ErrFormat", err)
		}
	})
}

func TestReadRecordLimit(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, testImage()); err != nil {
		t.Fatal(err)
	}
	var ip pb.ImageProto
	if err := ReadRecord(bytes.NewReader(buf.Bytes()), &ip, uint64(buf.Len()-9)); !errors.Is(err, ErrFormat) {
		t.Errorf("ReadRecord with a small limit = %v, want ErrFormat", err)
	}
	if err := ReadRecord(bytes.NewReader(buf.Bytes()[:buf.Len()-1]), &ip, MaxBodySize); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadRecord of a truncated record = %v, want io.ErrUnexpectedEOF", err)
	}
}
