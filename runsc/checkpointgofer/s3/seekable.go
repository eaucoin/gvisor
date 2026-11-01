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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
)

// The zstd seekable format (zstd's contrib/seekable_format): independent zstd
// frames, then a skippable frame holding the seek table, whose footer ends the
// file.
const (
	seekableSkippableMagic = 0x184D2A5E
	seekableMagic          = 0x8F92EAB1
	seekableFooterBytes    = 9
	seekableMaxFrames      = 0x8000000
	// seekableSuffixBytes is the suffix of the object read first for its seek
	// table: 8 bytes per frame, 1 GiB in frames of 256 KiB.
	seekableSuffixBytes = 64 << 10
)

// seekTable holds where each frame of a seekable file begins, logically and
// physically, with a last entry for the end of the last.
type seekTable struct {
	logical  []int64
	physical []int64
}

// parseSeekTable parses the seek table at the end of tail, the last bytes of
// a seekable file of size physical. It returns the table, or the number of
// bytes of the file's end it needs if tail holds fewer.
func parseSeekTable(tail []byte, physical int64) (*seekTable, int64, error) {
	if len(tail) < seekableFooterBytes {
		return nil, seekableFooterBytes, nil
	}
	footer := tail[len(tail)-seekableFooterBytes:]
	if binary.LittleEndian.Uint32(footer[5:]) != seekableMagic {
		return nil, 0, errors.New("no seekable zstd footer")
	}
	descriptor := footer[4]
	if descriptor&0x7f != 0 {
		return nil, 0, fmt.Errorf("seek table descriptor %#x has reserved bits set", descriptor)
	}
	entry := int64(8)
	if descriptor&0x80 != 0 {
		entry = 12
	}
	frames := int64(binary.LittleEndian.Uint32(footer))
	if frames > seekableMaxFrames || frames*entry+seekableFooterBytes+8 > physical {
		return nil, 0, fmt.Errorf("seek table of %d frames in a file of %d bytes", frames, physical)
	}
	tableBytes := 8 + frames*entry + seekableFooterBytes
	if int64(len(tail)) < tableBytes {
		return nil, tableBytes, nil
	}
	table := tail[int64(len(tail))-tableBytes:]
	if binary.LittleEndian.Uint32(table) != seekableSkippableMagic || int64(binary.LittleEndian.Uint32(table[4:])) != frames*entry+seekableFooterBytes {
		return nil, 0, errors.New("seek table is not a skippable frame of its size")
	}
	t := &seekTable{logical: make([]int64, frames+1), physical: make([]int64, frames+1)}
	for i := int64(0); i < frames; i++ {
		e := table[8+i*entry:]
		t.physical[i+1] = t.physical[i] + int64(binary.LittleEndian.Uint32(e))
		t.logical[i+1] = t.logical[i] + int64(binary.LittleEndian.Uint32(e[4:]))
	}
	if t.physical[frames]+tableBytes != physical {
		return nil, 0, fmt.Errorf("seek table's frames hold %d bytes of a file of %d", t.physical[frames], physical-tableBytes)
	}
	return t, 0, nil
}

// size returns the file's logical size.
func (t *seekTable) size() int64 {
	return t.logical[len(t.logical)-1]
}

// frameAt returns the index of the frame holding logical offset off < size().
func (t *seekTable) frameAt(off int64) int {
	return sort.Search(len(t.logical)-1, func(i int) bool { return t.logical[i+1] > off })
}

// zstdPages is the state of a Reader of a pages file stored as a seekable zstd
// file: its seek table, read once, and a decoder per worker.
type zstdPages struct {
	once  sync.Once
	table *seekTable
	err   error
	decs  []*zstd.Decoder
}

func newZstdPages(maxParallel int) *zstdPages {
	return &zstdPages{decs: make([]*zstd.Decoder, maxParallel)}
}

// loadTable reads r's seek table, once.
func (r *Reader) loadTable(ctx context.Context) (*seekTable, error) {
	z := r.zst
	z.once.Do(func() {
		want := int64(seekableSuffixBytes)
		for {
			out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(r.obj.bucket),
				Key:    aws.String(r.obj.key),
				Range:  aws.String(fmt.Sprintf("bytes=-%d", want)),
			})
			if err != nil {
				z.err = mapError(err, "GetObject", r.obj)
				return
			}
			tail, err := io.ReadAll(out.Body)
			out.Body.Close()
			if err != nil {
				z.err = err
				return
			}
			_, total, _ := strings.Cut(aws.ToString(out.ContentRange), "/")
			physical, err := strconv.ParseInt(total, 10, 64)
			if err != nil {
				z.err = fmt.Errorf("%s: no size in Content-Range %q", r.obj, aws.ToString(out.ContentRange))
				return
			}
			t, need, err := parseSeekTable(tail, physical)
			if err != nil {
				z.err = fmt.Errorf("%s: %w", r.obj, err)
				return
			}
			if t != nil {
				z.table = t
				r.size.Store(t.size())
				log.Infof("S3 read trace: %s is seekable zstd, %d frames, %d bytes of %d", r.obj, len(t.logical)-1, physical, t.size())
				return
			}
			if need <= want || need > physical {
				z.err = fmt.Errorf("%s: a seek table of %d bytes", r.obj, need)
				return
			}
			want = need
		}
	})
	return z.table, z.err
}

// readOnceZstd reads up to total logical bytes at off into dst with one GET
// of the frames that hold them, decoded as they stream, by the given worker.
// It returns io.EOF if the file ends before them.
func (r *Reader) readOnceZstd(ctx context.Context, off int64, total uint64, dst *stateio.LocalClientRanges, worker int) (n uint64, err error) {
	t, err := r.loadTable(ctx)
	if err != nil {
		return 0, err
	}
	size := t.size()
	if off >= size {
		return 0, io.EOF
	}
	want := min(total, uint64(size-off))
	first, last := t.frameAt(off), t.frameAt(off+int64(want)-1)
	from, to := t.physical[first], t.physical[last+1]
	ctx, cancel := context.WithTimeout(ctx, r.retry.deadline(uint64(to-from)))
	defer cancel()
	begun := time.Now()
	var answered time.Time
	if r.trace {
		defer func() {
			r.gets.Add(1)
			r.bytes.Add(uint64(to - from))
			log.Infof("S3 read trace: %s off %d bytes %d got %d worker %d begun %d answered %d ended %d physical %d frames %d err %v", r.obj, off, total, n, worker, begun.UnixNano(), answered.UnixNano(), time.Now().UnixNano(), to-from, last-first+1, err)
		}()
	}
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.obj.bucket),
		Key:    aws.String(r.obj.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", from, to-1)),
	})
	answered = time.Now()
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()
	dec := r.zst.decs[worker]
	if dec == nil {
		if dec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20)); err != nil {
			return 0, err
		}
		r.zst.decs[worker] = dec
	}
	if err := dec.Reset(out.Body); err != nil {
		return 0, err
	}
	if _, err := io.CopyN(io.Discard, dec, off-t.logical[first]); err != nil {
		return 0, fmt.Errorf("%w: %v", errShortBody, err)
	}
	var done uint64
	for _, m := range dst.Mappings {
		if done == want {
			break
		}
		m = m[:min(uint64(len(m)), want-done)]
		k, err := io.ReadFull(dec, m)
		done += uint64(k)
		if err != nil {
			return done, fmt.Errorf("%w after %d of %d bytes: %v", errShortBody, done, want, err)
		}
	}
	if done == total {
		return done, nil
	}
	return done, io.EOF
}
