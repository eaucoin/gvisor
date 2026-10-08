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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc64"
	"io"
	"os"

	"google.golang.org/protobuf/proto"
	"gvisor.dev/gvisor/pkg/log"
	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
)

// WriteRecord writes m to w as a record: its length as a little-endian
// uint64, then m.
func WriteRecord(w io.Writer, m proto.Message) error {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return fmt.Errorf("failed to marshal %T: %w", m, err)
	}
	var lengthBuf [8]byte
	binary.LittleEndian.PutUint64(lengthBuf[:], uint64(len(data)))
	if _, err := w.Write(lengthBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadRecord reads a record written by WriteRecord from r into m. limit
// bounds the length of the record.
func ReadRecord(r io.Reader, m proto.Message, limit uint64) error {
	var lengthBuf [8]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return fmt.Errorf("failed to read %T length: %w", m, err)
	}
	length := binary.LittleEndian.Uint64(lengthBuf[:])
	if length > limit {
		return formatErrorf("%T of %d bytes exceeds the limit of %d bytes", m, length, limit)
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(length)))
	if err != nil {
		return fmt.Errorf("failed to read %T: %w", m, err)
	}
	if uint64(len(data)) != length {
		return fmt.Errorf("failed to read %T: %w", m, io.ErrUnexpectedEOF)
	}
	if err := proto.Unmarshal(data, m); err != nil {
		return formatErrorf("failed to unmarshal %T: %v", m, err)
	}
	return nil
}

// ReadMetadata reads, checks and parses the metadata of a pages metadata file
// from r: its header, body and trailer. It checks the header and trailer
// before parsing the body, and validates everything it parses: an Image it
// returns is consistent, and refers to its layers only within their pages
// files. It leaves r at the page hashes section, which ReadHashes reads.
func ReadMetadata(r io.Reader) (*Image, error) {
	digest := sha256.New()
	r = io.TeeReader(r, digest)

	var hdrBuf [HeaderSize]byte
	if _, err := io.ReadFull(r, hdrBuf[:]); err != nil {
		return nil, fmt.Errorf("failed to read pages metadata header: %w", err)
	}
	hdr, err := ParseHeader(hdrBuf[:])
	if err != nil {
		return nil, err
	}
	// Grow the body as it is read, so that a short file is detected without
	// allocating the length its header claims.
	body, err := io.ReadAll(io.LimitReader(r, int64(hdr.Length)))
	if err != nil {
		return nil, fmt.Errorf("failed to read pages metadata body: %w", err)
	}
	if uint64(len(body)) != hdr.Length {
		return nil, formatErrorf("body is %d bytes, header says %d", len(body), hdr.Length)
	}
	var trailer [TrailerSize]byte
	if _, err := io.ReadFull(r, trailer[:]); err != nil {
		return nil, fmt.Errorf("failed to read pages metadata trailer: %w", err)
	}
	if got, want := binary.LittleEndian.Uint64(trailer[:]), crc64.Checksum(body, crcTable); got != want {
		return nil, formatErrorf("body CRC is %#016x, want %#016x", got, want)
	}

	img, err := parseBody(body)
	if err != nil {
		return nil, err
	}
	img.Header = hdr
	copy(img.Digest[:], digest.Sum(nil))
	return img, nil
}

// maxHashesSize is the largest page hashes section that readers accept: that
// of an image of 1 TiB of memory.
const maxHashesSize = 2 << 30

// ReadHashes reads the page hashes section of img's pages metadata file from
// r, positioned after the trailer (as ReadMetadata leaves it), checks it
// against the digest in img's body and that the file ends there, and sets the
// page hashes of img's MemoryFiles.
func (img *Image) ReadHashes(r io.Reader) error {
	var sizes []uint64
	var total uint64
	for i := range img.MemoryFiles {
		var n uint64
		if img.Proto.GetPageHash() != 0 {
			n = img.CommittedBytes(i) / PageSize * 8
		}
		if n > maxHashesSize-total {
			return formatErrorf("page hashes exceed the maximum of %d bytes", uint64(maxHashesSize))
		}
		sizes = append(sizes, n)
		total += n
	}
	hashes, err := io.ReadAll(io.LimitReader(r, int64(total)))
	if err != nil {
		return fmt.Errorf("failed to read page hashes: %w", err)
	}
	if uint64(len(hashes)) != total {
		return formatErrorf("page hashes section is %d bytes, want %d", len(hashes), total)
	}
	var extra [1]byte
	if n, _ := io.ReadFull(r, extra[:]); n != 0 {
		return formatErrorf("data after the page hashes")
	}
	if img.Proto.GetPageHash() != 0 {
		if got := sha256.Sum256(hashes); !bytes.Equal(got[:], img.Proto.GetPageHashesDigest()) {
			return formatErrorf("page hashes digest is %x, the body says %x", got, img.Proto.GetPageHashesDigest())
		}
	}
	for i, mf := range img.MemoryFiles {
		if n := sizes[i]; n != 0 {
			mf.PageHashes, hashes = hashes[:n:n], hashes[n:]
		}
	}
	return nil
}

// ReadHashesAsync calls img.ReadHashes(r) in a new goroutine, then done.
// MemoryFileImage waits for it to complete.
func (img *Image) ReadHashesAsync(r io.Reader, done func()) {
	ch := make(chan struct{})
	img.hashesRead = ch
	go func() {
		defer close(ch)
		defer done()
		if err := img.ReadHashes(r); err != nil {
			log.Warningf("Image %v: reading page hashes failed, saves relative to it will fail: %v", img.Digest, err)
			for _, mf := range img.MemoryFiles {
				mf.PageHashes = nil
			}
		}
	}()
}

// ReadImage reads a whole pages metadata file from r: its metadata
// (ReadMetadata), then its page hashes (Image.ReadHashes).
func ReadImage(r io.Reader) (*Image, error) {
	img, err := ReadMetadata(r)
	if err != nil {
		return nil, err
	}
	if err := img.ReadHashes(r); err != nil {
		return nil, err
	}
	return img, nil
}

// ReadMetadataFile reads the whole pages metadata file at path; see
// ReadImage.
func ReadMetadataFile(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := ReadImage(bufio.NewReader(f))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return img, nil
}

// parseBody parses and validates the body of a pages metadata file.
func parseBody(body []byte) (*Image, error) {
	br := bytes.NewReader(body)
	img := &Image{Proto: &pb.ImageProto{}}
	if err := ReadRecord(br, img.Proto, uint64(len(body))); err != nil {
		return nil, err
	}
	if err := ValidateImage(img.Proto); err != nil {
		return nil, err
	}
	img.records = body[len(body)-br.Len():]
	n := 1 + len(img.Proto.GetPrivateMemoryFiles())
	img.MemoryFiles = make([]*pb.MemoryFileMetadataProto, n)
	for i := range img.MemoryFiles {
		mf := &pb.MemoryFileMetadataProto{}
		if err := ReadRecord(br, mf, uint64(len(body))); err != nil {
			return nil, fmt.Errorf("MemoryFile %d: %w", i, err)
		}
		if err := ValidateMemoryFile(mf, img.Proto, false /* inline */); err != nil {
			return nil, fmt.Errorf("MemoryFile %d: %w", i, err)
		}
		img.MemoryFiles[i] = mf
	}
	if br.Len() != 0 {
		return nil, formatErrorf("%d bytes after the last MemoryFile", br.Len())
	}
	return img, nil
}

// A Writer writes a pages metadata file. The metadata of each MemoryFile, page
// hashes included, is written to it as a record (see WriteRecord), in the
// order the image's ImageProto describes; Finish then completes the image and
// writes the file, with the page hashes in its page hashes section.
type Writer struct {
	w     io.Writer
	image *pb.ImageProto
	body  bytes.Buffer
}

// NewWriter returns a Writer of an image described by ip, which it takes
// ownership of. ip.Layers[0].PagesSize is set by Finish.
func NewWriter(w io.Writer, ip *pb.ImageProto) *Writer {
	return &Writer{w: w, image: ip}
}

// Write implements io.Writer.Write. It buffers MemoryFile metadata records.
func (w *Writer) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

// Finish completes the image and writes the pages metadata file. pagesSize is
// the size of the image's own pages file.
//
// Finish drops the layers that no extent refers to, and orders the others by
// first reference, renumbering extents accordingly: an image refers only to
// the layers that hold its data, so a chain shortens itself as pages are
// rewritten. It validates the result as ReadImage does, so that a save fails
// rather than writing an image that cannot be restored. The Image it returns
// has its page hashes.
func (w *Writer) Finish(pagesSize uint64) (*Image, error) {
	ip := w.image
	if len(ip.GetLayers()) == 0 {
		return nil, fmt.Errorf("image has no layers")
	}
	ip.Layers[0].PagesSize = pagesSize

	n := 1 + len(ip.GetPrivateMemoryFiles())
	mfs := make([]*pb.MemoryFileMetadataProto, n)
	for i := range mfs {
		mfs[i] = &pb.MemoryFileMetadataProto{}
		if err := ReadRecord(&w.body, mfs[i], uint64(w.body.Len())); err != nil {
			return nil, fmt.Errorf("MemoryFile %d: %w", i, err)
		}
	}
	if w.body.Len() != 0 {
		return nil, fmt.Errorf("%d bytes written after the last MemoryFile", w.body.Len())
	}
	pruneLayers(ip, mfs)

	// Move the page hashes to the page hashes section.
	var hashes []byte
	mfHashes := make([][]byte, n)
	for i, mf := range mfs {
		mfHashes[i] = mf.PageHashes
		hashes = append(hashes, mf.PageHashes...)
		mf.PageHashes = nil
	}
	ip.PageHashesDigest = nil
	if ip.GetPageHash() != 0 {
		d := sha256.Sum256(hashes)
		ip.PageHashesDigest = d[:]
	}

	var body bytes.Buffer
	if err := WriteRecord(&body, ip); err != nil {
		return nil, err
	}
	for _, mf := range mfs {
		if err := WriteRecord(&body, mf); err != nil {
			return nil, err
		}
	}
	img, err := parseBody(body.Bytes())
	if err != nil {
		return nil, fmt.Errorf("saved image is invalid: %w", err)
	}
	for i, mf := range img.MemoryFiles {
		mf.PageHashes = mfHashes[i]
	}
	if err := validateHashes(img); err != nil {
		return nil, fmt.Errorf("saved image is invalid: %w", err)
	}
	img.Header = Header{Major: MajorVersion, Minor: MinorVersion, Length: uint64(body.Len())}
	hdr := img.Header.Encode()
	var trailer [TrailerSize]byte
	binary.LittleEndian.PutUint64(trailer[:], crc64.Checksum(body.Bytes(), crcTable))
	digest := sha256.New()
	out := io.MultiWriter(w.w, digest)
	for _, b := range [][]byte{hdr[:], body.Bytes(), trailer[:]} {
		if _, err := out.Write(b); err != nil {
			return nil, err
		}
	}
	if _, err := w.w.Write(hashes); err != nil {
		return nil, err
	}
	copy(img.Digest[:], digest.Sum(nil))
	return img, nil
}

// validateHashes checks that every MemoryFile of img has one page hash per
// known-committed page if img has page hashes, and none otherwise.
func validateHashes(img *Image) error {
	for i, mf := range img.MemoryFiles {
		var want uint64
		if img.Proto.GetPageHash() != 0 {
			want = img.CommittedBytes(i) / PageSize * 8
		}
		if got := uint64(len(mf.GetPageHashes())); got != want {
			return formatErrorf("MemoryFile %d: %d bytes of page hashes, want %d", i, got, want)
		}
	}
	return nil
}

// pruneLayers removes from ip the layers that no extent of mfs refers to,
// orders the others by first reference, and renumbers the extents.
func pruneLayers(ip *pb.ImageProto, mfs []*pb.MemoryFileMetadataProto) {
	const unmapped = ^uint32(0)
	remap := make([]uint32, len(ip.GetLayers()))
	for i := range remap {
		remap[i] = unmapped
	}
	remap[0] = 0
	layers := []*pb.LayerProto{ip.Layers[0]}
	for _, mf := range mfs {
		for _, e := range mf.GetExtents() {
			l := e.GetLayer()
			if l >= uint32(len(remap)) {
				// Left for validation to report.
				continue
			}
			if remap[l] == unmapped {
				remap[l] = uint32(len(layers))
				layers = append(layers, ip.Layers[l])
			}
			e.Layer = remap[l]
		}
	}
	ip.Layers = layers
}
