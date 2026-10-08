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

// Package checkpointimage reads and writes the pages metadata file of a
// checkpoint image (checkpointfiles.PagesMetadataFileName), which describes
// where the data of every saved MemoryFile page is.
//
// The file is a header, a body, a trailer and a page hashes section:
//
//	header, HeaderSize bytes, little-endian:
//	  magic   [8]byte  Magic
//	  major   uint16   MajorVersion; readers refuse any other major version
//	  minor   uint16   MinorVersion; readers refuse minor versions above theirs
//	  flags   uint32   reserved, 0
//	  length  uint64   length of the body in bytes
//	  crc     uint64   CRC-64 of the preceding 24 bytes of the header
//	body:
//	  uint64 length + ImageProto
//	  for each MemoryFile (the application's, then the private ones in
//	  ImageProto.private_memory_files order):
//	    uint64 length + MemoryFileMetadataProto
//	trailer, TrailerSize bytes:
//	  crc     uint64   CRC-64 of the body
//	page hashes section:
//	  for each MemoryFile, in the same order: one hash (ImageProto.page_hash),
//	  a little-endian uint64, per known-committed page in MemoryFile offset
//	  order
//
// CRC-64 is Go's hash/crc64 with the ECMA-182 polynomial (CRC-64/XZ). The
// header and trailer are checked before anything in the body is parsed.
//
// The page hashes, 2 MiB per GiB of saved memory, are only needed to save an
// image relative to this one or to verify its pages, so they follow the
// metadata that a restore reads, which stays small: ReadMetadata reads the
// header, body and trailer, and ReadHashes the page hashes section, whose
// SHA-256 the body records (ImageProto.page_hashes_digest).
//
// An image may refer to the pages files of other images, its layers, which it
// identifies by the SHA-256 of the header, body and trailer of their pages
// metadata file: an image's Digest is its identity. The digest covers every
// extent and, through the body's digest of the page hashes, every page hash,
// so it identifies the image's memory transitively. Because every extent names
// the layer that holds its data, a restore reads each range from its layer
// directly, however long the chain of images that produced it.
//
// This package needs no sandbox: runsc and the image tools use it as much as
// the Sentry does.
package checkpointimage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc64"
)

// Format constants.
const (
	// Magic starts every pages metadata file.
	Magic = "gVisorPM"

	// MajorVersion is the major version of the format that this package
	// reads and writes. Readers refuse any other major version.
	MajorVersion = 2

	// MinorVersion is the minor version of the format that this package
	// writes. Readers refuse minor versions above their own, and accept
	// lower ones.
	MinorVersion = 0

	// HeaderSize is the size of the header in bytes.
	HeaderSize = 32

	// TrailerSize is the size of the trailer in bytes.
	TrailerSize = 8

	// MaxBodySize is the largest body that readers accept.
	MaxBodySize = 1 << 30

	// MemoryFileMetadataVersion is the MemoryFileMetadataProto.version of
	// this format.
	MemoryFileMetadataVersion = 2

	// PageHashXXH64 is ImageProto.page_hash for XXH64 (seed 0) of each 4 KiB
	// page.
	PageHashXXH64 = 1

	// PageSize is the size of the pages that extents and page hashes
	// describe.
	PageSize = 4096
)

var crcTable = crc64.MakeTable(crc64.ECMA)

// ErrFormat is wrapped by every error that reports a malformed or
// inconsistent pages metadata file.
var ErrFormat = errors.New("invalid checkpoint pages metadata")

func formatErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, args...))
}

// Header is the header of a pages metadata file.
type Header struct {
	Major  uint16
	Minor  uint16
	Flags  uint32
	Length uint64
}

// Encode returns the encoded header.
func (h Header) Encode() [HeaderSize]byte {
	var b [HeaderSize]byte
	copy(b[:], Magic)
	binary.LittleEndian.PutUint16(b[8:], h.Major)
	binary.LittleEndian.PutUint16(b[10:], h.Minor)
	binary.LittleEndian.PutUint32(b[12:], h.Flags)
	binary.LittleEndian.PutUint64(b[16:], h.Length)
	binary.LittleEndian.PutUint64(b[24:], crc64.Checksum(b[:24], crcTable))
	return b
}

// ParseHeader parses and checks the header in b, which must be HeaderSize
// bytes long. It refuses headers that this package cannot read.
func ParseHeader(b []byte) (Header, error) {
	if len(b) != HeaderSize {
		return Header{}, formatErrorf("header is %d bytes, want %d", len(b), HeaderSize)
	}
	if string(b[:8]) != Magic {
		return Header{}, formatErrorf("bad magic %q", b[:8])
	}
	if got, want := binary.LittleEndian.Uint64(b[24:]), crc64.Checksum(b[:24], crcTable); got != want {
		return Header{}, formatErrorf("header CRC is %#016x, want %#016x", got, want)
	}
	h := Header{
		Major:  binary.LittleEndian.Uint16(b[8:]),
		Minor:  binary.LittleEndian.Uint16(b[10:]),
		Flags:  binary.LittleEndian.Uint32(b[12:]),
		Length: binary.LittleEndian.Uint64(b[16:]),
	}
	if h.Major != MajorVersion || h.Minor > MinorVersion {
		return Header{}, formatErrorf("version %d.%d is not supported by this build (%d.%d)", h.Major, h.Minor, MajorVersion, MinorVersion)
	}
	if h.Flags != 0 {
		return Header{}, formatErrorf("unknown flags %#x", h.Flags)
	}
	if h.Length > MaxBodySize {
		return Header{}, formatErrorf("body length %d exceeds the maximum of %d", h.Length, uint64(MaxBodySize))
	}
	return h, nil
}

// Digest identifies an image: it is the SHA-256 of its pages metadata file.
type Digest [sha256.Size]byte

// String returns d in lowercase hexadecimal, the form used in file names and
// on the command line.
func (d Digest) String() string {
	return hex.EncodeToString(d[:])
}

// ParseDigest parses the hexadecimal form of a digest.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(d) {
		return Digest{}, fmt.Errorf("%q is not a hexadecimal SHA-256 digest", s)
	}
	copy(d[:], b)
	return d, nil
}

// digestFromBytes returns the digest held in a LayerProto.
func digestFromBytes(b []byte) (Digest, bool) {
	var d Digest
	if len(b) != len(d) {
		return Digest{}, false
	}
	copy(d[:], b)
	return d, true
}
