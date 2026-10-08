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
	"fmt"
	"testing"

	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
)

// benchImage returns the pages metadata file of a full image of a MemoryFile
// with mib MiB committed, in nr extents.
func benchImage(b *testing.B, mib, nr uint64) []byte {
	pages := mib << 20 / page
	mf := &pb.MemoryFileMetadataProto{
		Version:    MemoryFileMetadataVersion,
		Chunks:     make([]*pb.ChunkInfoProto, (mib<<20+(1<<30)-1)>>30),
		MemAcct:    []*pb.MemAcctRangeProto{{Start: 0, End: pages * page, KnownCommitted: true}},
		PageHashes: make([]byte, 8*pages),
	}
	for i := range mf.Chunks {
		mf.Chunks[i] = &pb.ChunkInfoProto{}
	}
	per := pages / nr
	for i := uint64(0); i < nr; i++ {
		end := (i + 1) * per
		if i == nr-1 {
			end = pages
		}
		mf.Extents = append(mf.Extents, &pb.ExtentProto{Start: i * per * page, End: end * page, Offset: i * per * page})
	}
	data, _ := writeImage(b, testImage(), pages*page, mf)
	return data
}

// BenchmarkReadMetadata measures what a restore reads before loading pages.
func BenchmarkReadMetadata(b *testing.B) {
	for _, mib := range []uint64{64, 512, 1024} {
		data := benchImage(b, mib, 16)
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := ReadMetadata(bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReadImage measures reading a whole pages metadata file, page
// hashes included.
func BenchmarkReadImage(b *testing.B) {
	for _, mib := range []uint64{64, 512, 1024} {
		data := benchImage(b, mib, 16)
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				if _, err := ReadImage(bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
