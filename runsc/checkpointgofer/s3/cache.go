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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/cespare/xxhash/v2"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/sync"
)

// A write-through cache of the pages files that a restore reads.
//
// Given a cache directory (FileServerOptions.CacheDir), which the node stages
// with a sparse file for each pages file of the image, at the path the
// FileServer serves it at ("pages.img", "layers/<digest>/pages.img"), the
// FileServer writes the bytes of each GET of a pages file into that file, at
// their offset, as the restore reads them. It reads each response's body into
// buffers of its own, copies them to the client's memory, and writes them from
// there, so that the cache never holds bytes that the Sentry, which maps the
// client's memory too, could have changed. The writes are behind the reads,
// with O_DIRECT, so that a read never waits for the disk and the bytes do not
// fill the page cache that the sandbox's cgroup is charged for; a buffer that
// does not fit in the bounded queue is not written.
//
// Each page that the image refers to is marked once its bytes in the cache
// match the page's hash in the image's pages metadata, which the FileServer
// reads from the store itself. Once its client is done reading
// (CacheStatus), it reads from the store the pages that are not marked yet,
// syncs the files, and writes each file's marks beside it
// ("pages.img.marks"), in a new file renamed into place: complete when every
// page the image refers to in that file is marked.

const (
	// cacheChunkBytes is the size of the buffers that the bytes of a pages
	// file are read into, before they are copied to the client's memory and
	// written to the cache.
	cacheChunkBytes = 1 << 20

	// cacheWriters is the number of goroutines writing to the cache.
	cacheWriters = 4

	// cachePage is the size of a page of a pages file, which marks count.
	cachePage = checkpointimage.PageSize

	// cacheMarksSuffix is appended to the path of a cached pages file to
	// name its marks.
	cacheMarksSuffix = ".marks"

	// cacheMarksMagic begins a marks file.
	cacheMarksMagic = "gvcmarks"

	// cacheMarksVersion is the version of the marks files' format: the magic,
	// then, little-endian, the version (uint32), flags (uint32), the number
	// of pages (uint64), the pages metadata's digest (32 bytes), and one bit
	// per page, set if the page is marked, in uint64 words.
	cacheMarksVersion = 1

	// cacheMarksComplete is the flag of marks in which every page that the
	// image refers to is marked.
	cacheMarksComplete = 1
)

// cacheQueueBytes bounds the bytes waiting to be written to the cache: a
// chunk read while they would be more is not written. It is a variable for
// tests.
var cacheQueueBytes = 64 << 20

// pagesCache is the cache of the pages files of the image that a FileServer
// serves.
type pagesCache struct {
	fs  *FileServer
	dir *os.File

	// queue holds the chunks waiting to be written; free the buffers not in
	// use, of which allocated are mapped. pending counts the chunks queued
	// and not yet written.
	queue     chan cacheChunk
	free      chan []byte
	allocated atomic.Int32
	pending   sync.WaitGroup
	writers   sync.WaitGroup

	// indexDone is closed once index is built, or indexErr says why it
	// could not be.
	indexOnce sync.Once
	indexDone chan struct{}
	index     *cacheIndex
	indexErr  error

	mu sync.Mutex
	// files holds the cached files, by the path the FileServer serves them
	// at. files is protected by mu.
	files map[string]*cacheFile
	// status is the cache's status once CacheStatus completed it. status is
	// protected by mu.
	status *stateipc.CacheStatus

	dropped    atomic.Uint64
	mismatched atomic.Uint64
}

// cacheChunk is a chunk of a pages file waiting to be written to the cache.
type cacheChunk struct {
	f   *cacheFile
	off int64
	buf []byte
}

// cacheIndex is what the image's pages metadata says of each of its pages
// files: the hash of each page that the image refers to.
type cacheIndex struct {
	digest checkpointimage.Digest
	// files holds the hashes of each layer's pages file, by the path the
	// FileServer serves it at.
	files map[string]*pageHashes
}

// pageHashes holds the hashes of the pages of a pages file that an image
// refers to.
type pageHashes struct {
	// referenced has a bit set for each page the image refers to, whose
	// hash is hashes[page].
	referenced []uint64
	hashes     []uint64
}

// cacheFile is a cached pages file.
type cacheFile struct {
	path  string
	data  *os.File
	pages uint64

	mu sync.Mutex
	// marked has a bit set for each page whose cached bytes matched its
	// hash; written for each page written whose hash was not known then.
	// These fields are protected by mu.
	marked  []uint64
	written []uint64
	// err is the first error writing the file. err is protected by mu.
	err error
}

func setBit(bits []uint64, i uint64)      { bits[i/64] |= 1 << (i % 64) }
func clearBit(bits []uint64, i uint64)    { bits[i/64] &^= 1 << (i % 64) }
func bitSet(bits []uint64, i uint64) bool { return bits[i/64]&(1<<(i%64)) != 0 }

// newPagesCache returns the cache of the pages files that fs serves in dir.
func newPagesCache(fs *FileServer, dir *os.File) *pagesCache {
	queued := cacheQueueBytes / cacheChunkBytes
	c := &pagesCache{
		fs:    fs,
		dir:   dir,
		queue: make(chan cacheChunk, queued),
		// Room for the buffers queued, being written, and being read into
		// by each of the reads in flight.
		free:      make(chan []byte, queued+cacheWriters+fs.opts.PagesFileReadParallel),
		indexDone: make(chan struct{}),
		files:     make(map[string]*cacheFile),
	}
	c.writers.Add(cacheWriters)
	for range cacheWriters {
		go c.writerMain()
	}
	return c
}

// file returns the cached file of the pages file served at p, opening it,
// or nil if the cache holds no file for it. It starts reading the image's
// pages metadata, which the cache checks pages against.
func (c *pagesCache) file(p string) *cacheFile {
	c.indexOnce.Do(func() { go c.buildIndex() })
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.files[p]; ok {
		return f
	}
	how := unix.OpenHow{
		Flags:   unix.O_RDWR | unix.O_DIRECT | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(int(c.dir.Fd()), p, &how)
	if errors.Is(err, unix.EINVAL) {
		// A filesystem without O_DIRECT: the bytes go through the page cache.
		how.Flags &^= unix.O_DIRECT
		fd, err = unix.Openat2(int(c.dir.Fd()), p, &how)
	}
	if err != nil {
		if !errors.Is(err, unix.ENOENT) {
			log.Warningf("s3 cache: opening %s: %v", p, err)
		}
		c.files[p] = nil
		return nil
	}
	data := os.NewFile(uintptr(fd), p)
	fi, err := data.Stat()
	if err != nil {
		log.Warningf("s3 cache: %s: %v", p, err)
		data.Close()
		c.files[p] = nil
		return nil
	}
	pages := (uint64(fi.Size()) + cachePage - 1) / cachePage
	f := &cacheFile{
		path:    p,
		data:    data,
		pages:   pages,
		marked:  make([]uint64, (pages+63)/64),
		written: make([]uint64, (pages+63)/64),
	}
	c.files[p] = f
	return f
}

// buildIndex reads the image's pages metadata from the store and builds the
// index of its pages files' hashes.
func (c *pagesCache) buildIndex() {
	defer close(c.indexDone)
	c.index, c.indexErr = c.readIndex()
	if c.indexErr != nil {
		log.Warningf("s3 cache: cannot check pages, so marks none: %v", c.indexErr)
	}
}

func (c *pagesCache) readIndex() (*cacheIndex, error) {
	obj := c.fs.object(checkpointfiles.PagesMetadataFileName)
	in := &s3.GetObjectInput{Bucket: aws.String(obj.bucket), Key: aws.String(obj.key)}
	if etag := c.fs.opts.ETags[checkpointfiles.PagesMetadataFileName]; etag != "" {
		in.IfMatch = aws.String(etag)
	}
	out, err := c.fs.client.GetObject(c.fs.ctx, in)
	if err != nil {
		return nil, mapError(err, "GetObject", obj)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", obj, err)
	}
	img, err := checkpointimage.ReadImage(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", obj, err)
	}
	if img.Proto.GetPageHash() != checkpointimage.PageHashXXH64 {
		return nil, fmt.Errorf("%s has no page hashes", obj)
	}
	layers := img.Layers()
	index := &cacheIndex{digest: img.Digest, files: make(map[string]*pageHashes, len(layers))}
	byLayer := make([]*pageHashes, len(layers))
	for i, l := range layers {
		p := checkpointfiles.PagesFileName
		if i != 0 {
			p = checkpointimage.LayerPath(l.Digest, checkpointfiles.PagesFileName)
		}
		pages := (l.PagesSize + cachePage - 1) / cachePage
		byLayer[i] = &pageHashes{referenced: make([]uint64, (pages+63)/64), hashes: make([]uint64, pages)}
		index.files[p] = byLayer[i]
	}
	for i := range img.MemoryFiles {
		m := img.MemoryFileImage(i)
		cur := m.Cursor()
		for _, e := range m.Extents {
			for off := e.Start; off < e.End; off += cachePage {
				_, _, h, ok := cur.Lookup(off)
				if !ok {
					continue
				}
				ph := byLayer[e.Layer]
				page := e.OffsetOf(off) / cachePage
				if page >= uint64(len(ph.hashes)) {
					return nil, fmt.Errorf("%s: an extent of layer %d is beyond its pages file", obj, e.Layer)
				}
				setBit(ph.referenced, page)
				ph.hashes[page] = h
			}
		}
	}
	return index, nil
}

// indexReady returns the index if it is built, or nil.
func (c *pagesCache) indexReady() *cacheIndex {
	select {
	case <-c.indexDone:
		return c.index
	default:
		return nil
	}
}

// buffer returns a buffer of cacheChunkBytes, or nil if every buffer is in
// use: the bytes are then read to the client's memory alone, and not cached.
func (c *pagesCache) buffer() []byte {
	select {
	case buf := <-c.free:
		return buf
	default:
	}
	if int(c.allocated.Load()) >= cap(c.free) {
		return nil
	}
	// Mapped, so that it is aligned as O_DIRECT requires.
	buf, err := unix.Mmap(-1, 0, cacheChunkBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil
	}
	c.allocated.Add(1)
	return buf
}

// write queues buf, the bytes of f at off, to be written; buf is the cache's
// again once they are. Bytes that do not fit in the queue, or that O_DIRECT
// cannot write (not page-aligned), are not written.
func (c *pagesCache) write(f *cacheFile, off int64, buf []byte) {
	if off%cachePage != 0 || len(buf)%cachePage != 0 {
		c.drop(buf)
		return
	}
	c.pending.Add(1)
	select {
	case c.queue <- cacheChunk{f: f, off: off, buf: buf}:
	default:
		c.pending.Done()
		c.drop(buf)
	}
}

// drop counts buf's bytes as not written and returns it to the cache.
func (c *pagesCache) drop(buf []byte) {
	c.dropped.Add(uint64(len(buf)))
	c.free <- buf[:cap(buf)]
}

// writerMain writes queued chunks and marks their pages.
func (c *pagesCache) writerMain() {
	defer c.writers.Done()
	for ch := range c.queue {
		c.writeChunk(ch.f, ch.off, ch.buf)
		c.free <- ch.buf[:cap(ch.buf)]
		c.pending.Done()
	}
}

// writeChunk writes buf, the bytes of f at off, and marks its pages that
// match their hashes, or records them as written if their hashes are not
// known yet.
func (c *pagesCache) writeChunk(f *cacheFile, off int64, buf []byte) {
	if _, err := unix.Pwrite(int(f.data.Fd()), buf, off); err != nil {
		f.mu.Lock()
		if f.err == nil {
			f.err = fmt.Errorf("writing %s at %d: %w", f.path, off, err)
		}
		f.mu.Unlock()
		c.dropped.Add(uint64(len(buf)))
		return
	}
	c.check(f, uint64(off)/cachePage, buf)
}

// check marks the pages of f from first that match their hashes, buf holding
// their bytes; if the index is not built yet, it records them as written, to
// be checked once it is.
func (c *pagesCache) check(f *cacheFile, first uint64, buf []byte) {
	index := c.indexReady()
	var ph *pageHashes
	if index != nil {
		ph = index.files[f.path]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := uint64(0); i < uint64(len(buf))/cachePage; i++ {
		page := first + i
		if page >= f.pages {
			break
		}
		if index == nil {
			setBit(f.written, page)
			continue
		}
		clearBit(f.written, page)
		if ph == nil || page >= uint64(len(ph.hashes)) || !bitSet(ph.referenced, page) {
			// Not referenced (a gap read through, a copy of a page that a
			// later copy supersedes): written, never marked.
			continue
		}
		if xxhash.Sum64(buf[i*cachePage:(i+1)*cachePage]) == ph.hashes[page] {
			setBit(f.marked, page)
		} else {
			clearBit(f.marked, page)
			c.mismatched.Add(1)
		}
	}
}

// CacheStatus implements stateipc.CacheReporter.CacheStatus: once the
// restore's reads are done, it writes what is queued, checks the pages written
// before the index was built, reads from the store the referenced pages not
// marked yet, syncs the files and writes their marks. It does so once; later
// calls return the same status.
func (c *pagesCache) CacheStatus() stateipc.CacheStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != nil {
		return *c.status
	}
	st := c.complete()
	c.status = &st
	log.Infof("s3 cache: %d files, %d of %d referenced bytes held (%d of them filled), %d bytes not written, %d pages mismatched; complete %t %s", st.Files, st.Held, st.Referenced, st.Filled, st.Dropped, st.Mismatched, st.Complete, st.Error)
	return st
}

// complete completes the cache.
//
// Preconditions: c.mu must be locked.
func (c *pagesCache) complete() stateipc.CacheStatus {
	st := stateipc.CacheStatus{Enabled: true}
	c.pending.Wait()
	select {
	case <-c.indexDone:
	case <-c.fs.ctx.Done():
		st.Error = "the checkpoint gofer stopped before the cache's index was built"
		return st
	}
	if c.indexErr != nil {
		st.Error = c.indexErr.Error()
		return st
	}
	st.Complete = true
	for p, ph := range c.index.files {
		f := c.files[p]
		if f == nil {
			st.Complete = false
			continue
		}
		st.Files++
		filled, err := c.completeFile(f, ph)
		st.Filled += filled
		var referenced, held uint64
		f.mu.Lock()
		for page := uint64(0); page < uint64(len(ph.hashes)); page++ {
			if !bitSet(ph.referenced, page) {
				continue
			}
			referenced += cachePage
			if page < f.pages && bitSet(f.marked, page) {
				held += cachePage
			}
		}
		f.mu.Unlock()
		st.Referenced += referenced
		st.Held += held
		complete := err == nil && held == referenced
		if err == nil {
			err = c.writeMarks(f, complete)
		}
		if err != nil {
			complete = false
			if st.Error == "" {
				st.Error = err.Error()
			}
		}
		st.Complete = st.Complete && complete
	}
	st.Dropped = c.dropped.Load()
	st.Mismatched = c.mismatched.Load()
	return st
}

// completeFile checks the pages of f written before the index was built,
// reads from the store the pages that ph refers to and that are not marked,
// writes them and syncs f. It returns the number of bytes it read.
//
// Preconditions: c's writers are done with f.
func (c *pagesCache) completeFile(f *cacheFile, ph *pageHashes) (uint64, error) {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	buf, err := unix.Mmap(-1, 0, cacheChunkBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return 0, err
	}
	defer unix.Munmap(buf)
	// The pages written before their hashes were known, read back.
	for page := uint64(0); page < f.pages; page++ {
		if !bitSet(f.written, page) {
			continue
		}
		pg := buf[:cachePage]
		if _, err := unix.Pread(int(f.data.Fd()), pg, int64(page*cachePage)); err != nil {
			return 0, fmt.Errorf("reading %s back: %w", f.path, err)
		}
		c.check(f, page, pg)
	}
	// The holes: runs of referenced pages not marked, read from the store.
	var filled uint64
	obj := c.fs.object(f.path)
	for page := uint64(0); page < uint64(len(ph.hashes)) && page < f.pages; {
		if !bitSet(ph.referenced, page) || bitSet(f.marked, page) {
			page++
			continue
		}
		end := page + 1
		for end < uint64(len(ph.hashes)) && end < f.pages && end-page < cacheChunkBytes/cachePage && bitSet(ph.referenced, end) && !bitSet(f.marked, end) {
			end++
		}
		n := (end - page) * cachePage
		if err := c.fill(obj, f.path, int64(page*cachePage), buf[:n]); err != nil {
			return filled, err
		}
		if _, err := unix.Pwrite(int(f.data.Fd()), buf[:n], int64(page*cachePage)); err != nil {
			return filled, fmt.Errorf("writing %s at %d: %w", f.path, page*cachePage, err)
		}
		filled += n
		c.check(f, page, buf[:n])
		page = end
	}
	if err := unix.Fdatasync(int(f.data.Fd())); err != nil {
		return filled, fmt.Errorf("syncing %s: %w", f.path, err)
	}
	return filled, nil
}

// fill reads len(buf) bytes of obj, served at p, at off from the store.
func (c *pagesCache) fill(obj object, p string, off int64, buf []byte) error {
	in := &s3.GetObjectInput{
		Bucket: aws.String(obj.bucket),
		Key:    aws.String(obj.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, off+int64(len(buf))-1)),
	}
	if etag := c.fs.opts.ETags[p]; etag != "" {
		in.IfMatch = aws.String(etag)
	}
	ctx, cancel := context.WithTimeout(c.fs.ctx, c.fs.retry.deadline(uint64(len(buf))))
	defer cancel()
	out, err := c.fs.client.GetObject(ctx, in)
	if err != nil {
		return mapError(err, "GetObject", obj)
	}
	defer out.Body.Close()
	if _, err := io.ReadFull(out.Body, buf); err != nil {
		return fmt.Errorf("reading %s at %d: %w", obj, off, err)
	}
	return nil
}

// writeMarks writes f's marks beside it, in a new file renamed into place.
func (c *pagesCache) writeMarks(f *cacheFile, complete bool) error {
	var flags uint32
	if complete {
		flags |= cacheMarksComplete
	}
	b := make([]byte, 0, len(cacheMarksMagic)+4+4+8+len(c.index.digest)+8*len(f.marked))
	b = append(b, cacheMarksMagic...)
	b = binary.LittleEndian.AppendUint32(b, cacheMarksVersion)
	b = binary.LittleEndian.AppendUint32(b, flags)
	b = binary.LittleEndian.AppendUint64(b, f.pages)
	b = append(b, c.index.digest[:]...)
	f.mu.Lock()
	for _, w := range f.marked {
		b = binary.LittleEndian.AppendUint64(b, w)
	}
	f.mu.Unlock()

	marks := f.path + cacheMarksSuffix
	tmp := path.Join(path.Dir(marks), "."+path.Base(marks)+".tmp")
	fd, err := unix.Openat2(int(c.dir.Fd()), tmp, &unix.OpenHow{
		Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_TRUNC | unix.O_CLOEXEC,
		Mode:    0o600,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	tf := os.NewFile(uintptr(fd), tmp)
	if _, err := tf.Write(b); err != nil {
		tf.Close()
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := unix.Fsync(fd); err != nil {
		tf.Close()
		return fmt.Errorf("syncing %s: %w", tmp, err)
	}
	if err := tf.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp, err)
	}
	if err := unix.Renameat(int(c.dir.Fd()), tmp, int(c.dir.Fd()), marks); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmp, marks, err)
	}
	return nil
}

// close stops the cache's writers and closes its files.
func (c *pagesCache) close() {
	c.pending.Wait()
	close(c.queue)
	c.writers.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.files {
		if f != nil {
			f.data.Close()
		}
	}
	for range c.allocated.Load() {
		unix.Munmap(<-c.free)
	}
	c.dir.Close()
}
