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

//go:build !pagesize_64k

package pgalloc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
)

// This file is the harness of pgalloc's save/restore and async page loading
// tests: MemoryFiles over memfds (as runsc creates them), pages filled with
// self-describing patterns, saving and loading through stateio, an invariant
// check, and a pages file reader that models a storage device in virtual
// time.

// testMemoryFileOpts are options to newTestMemoryFile.
type testMemoryFileOpts struct {
	// If diskBacked is true, the MemoryFile is backed by a regular file in a
	// temporary directory rather than by a memfd.
	diskBacked bool

	// expectHugepages is MemoryFileOpts.ExpectHugepages: if true,
	// AllocOpts.Huge allocations use hugepage-backed chunks.
	expectHugepages bool
}

// newTestMemoryFile returns a new MemoryFile that is destroyed when the test
// ends. The test must release every page it allocates or loads before then.
func newTestMemoryFile(t testing.TB, opts testMemoryFileOpts) *MemoryFile {
	t.Helper()
	var file *os.File
	if opts.diskBacked {
		var err error
		if file, err = os.CreateTemp(t.TempDir(), "pgalloc-test"); err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
	} else {
		fd, err := memutil.CreateMemFD("pgalloc-test", 0)
		if err != nil {
			t.Fatalf("CreateMemFD: %v", err)
		}
		file = os.NewFile(uintptr(fd), "pgalloc-test")
	}
	f, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
		DiskBackedFile:          opts.diskBacked,
		ExpectHugepages:         opts.expectHugepages,
	})
	if err != nil {
		file.Close()
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(f.Destroy)
	return f
}

// allocate allocates length bytes from f and returns their range.
func allocate(t testing.TB, f *MemoryFile, length uint64, opts AllocOpts) memmap.FileRange {
	t.Helper()
	fr, err := f.Allocate(length, opts)
	if err != nil {
		t.Fatalf("Allocate(%#x, %+v): %v", length, opts, err)
	}
	return fr
}

// Page patterns. As in QEMU's migration tests, a page written by the tests
// identifies itself: it holds its MemoryFile offset and a generation, and the
// rest of the page is derived from both, so that a page restored at the wrong
// offset, from the wrong generation or only partially is detected.
const patternMagic = 0x6776697370676974 // "tigpsivg"

// patternPage fills pg with the pattern of the page at off in generation gen.
// Generation 0 is the zero page.
func patternPage(pg []byte, off, gen uint64) {
	if gen == 0 {
		clear(pg)
		return
	}
	binary.LittleEndian.PutUint64(pg[0:], patternMagic)
	binary.LittleEndian.PutUint64(pg[8:], off)
	binary.LittleEndian.PutUint64(pg[16:], gen)
	x := off*0x9e3779b97f4a7c15 ^ gen*0xbf58476d1ce4e5b9
	for i := 24; i < len(pg); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(pg[i:], x)
	}
}

// writePattern writes the pattern of generation gen to every page in fr
// through f's internal mappings, as the Sentry writes application memory.
func writePattern(t *testing.T, f *MemoryFile, fr memmap.FileRange, gen uint64) {
	t.Helper()
	off := fr.Start
	f.forEachMappingSlice(fr, func(bs []byte) {
		for i := 0; i < len(bs); i += hostarch.PageSize {
			patternPage(bs[i:i+hostarch.PageSize], off, gen)
			off += hostarch.PageSize
		}
	})
}

// pageGens maps MemoryFile offsets of pages to their expected generations;
// pages absent from the map are expected to be zero.
type pageGens map[uint64]uint64

// set records that every page in fr has generation gen.
func (g pageGens) set(fr memmap.FileRange, gen uint64) {
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		if gen == 0 {
			delete(g, off)
		} else {
			g[off] = gen
		}
	}
}

// write writes generation gen to fr in f and records it in g.
func (g pageGens) write(t *testing.T, f *MemoryFile, fr memmap.FileRange, gen uint64) {
	t.Helper()
	writePattern(t, f, fr, gen)
	g.set(fr, gen)
}

// checkPages checks that every page in fr has the contents g expects. It
// reads through MapInternal, so it waits for async page loading as the Sentry
// does.
func (g pageGens) checkPages(t *testing.T, f *MemoryFile, fr memmap.FileRange) {
	t.Helper()
	ims, err := f.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal(%v): %v", fr, err)
	}
	data := make([]byte, fr.Length())
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data)), ims); err != nil {
		t.Fatalf("reading %v: %v", fr, err)
	}
	want := make([]byte, hostarch.PageSize)
	bad := 0
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		got := data[off-fr.Start:][:hostarch.PageSize]
		gen := g[off]
		patternPage(want, off, gen)
		if !bytes.Equal(got, want) {
			if bad < 8 {
				t.Errorf("page %#x: got %s, want generation %d", off, describePage(got), gen)
			}
			bad++
		}
	}
	if bad != 0 {
		t.Errorf("%d of %d pages in %v differ", bad, fr.Length()/hostarch.PageSize, fr)
	}
}

// describePage describes a page that differs from its expected pattern.
func describePage(pg []byte) string {
	if bytes.Equal(pg, make([]byte, len(pg))) {
		return "the zero page"
	}
	if binary.LittleEndian.Uint64(pg[0:]) != patternMagic {
		return "a page without a pattern"
	}
	off := binary.LittleEndian.Uint64(pg[8:])
	gen := binary.LittleEndian.Uint64(pg[16:])
	want := make([]byte, len(pg))
	patternPage(want, off, gen)
	if !bytes.Equal(pg, want) {
		return fmt.Sprintf("a corrupt page claiming offset %#x generation %d", off, gen)
	}
	return fmt.Sprintf("the page of offset %#x generation %d", off, gen)
}

// committedRanges returns the known-committed ranges of f's memAcct, merged.
func committedRanges(f *MemoryFile) []memmap.FileRange {
	f.mu.Lock()
	defer f.mu.Unlock()
	var frs []memmap.FileRange
	for seg := f.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		if !seg.ValuePtr().knownCommitted {
			continue
		}
		if n := len(frs); n != 0 && frs[n-1].End == seg.Start() {
			frs[n-1].End = seg.End()
		} else {
			frs = append(frs, seg.Range())
		}
	}
	return frs
}

// checkInvariants checks the consistency of f's page state:
//   - knownCommittedBytes is the span of known-committed memAcct segments;
//   - memAcct covers exactly the used, waste and releasing pages: pages with
//     references are in memAcct and not marked waste, and pages in memAcct
//     that are not waste or releasing have references;
//   - memAcct lies within the file.
func checkInvariants(t *testing.T, f *MemoryFile) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	fileSize := uint64(len(f.chunksLoad())) * chunkSize
	var committed uint64
	for seg := f.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		ma := seg.ValuePtr()
		if seg.End() > fileSize {
			t.Errorf("memAcct segment %v extends beyond the file (%#x bytes)", seg.Range(), fileSize)
		}
		if ma.knownCommitted {
			committed += seg.Range().Length()
		}
		if ma.wasteOrReleasing {
			continue
		}
		f.forEachChunk(seg.Range(), func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
			unfree := &f.unfreeSmall
			if chunk.huge {
				unfree = &f.unfreeHuge
			}
			unfree.VisitFullRange(chunkFR, func(ufseg unfreeIterator) bool {
				if ufseg.ValuePtr().refs == 0 {
					t.Errorf("memAcct segment %v (%+v) covers pages %v without references", seg.Range(), *ma, ufseg.Range().Intersect(chunkFR))
				}
				return true
			})
			return true
		})
	}
	if committed != f.knownCommittedBytes {
		t.Errorf("knownCommittedBytes is %d, known-committed memAcct segments span %d bytes", f.knownCommittedBytes, committed)
	}
	f.forEachChunk(memmap.FileRange{0, fileSize}, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		unfree := &f.unfreeSmall
		if chunk.huge {
			unfree = &f.unfreeHuge
		}
		for ufseg := unfree.LowerBoundSegment(chunkFR.Start); ufseg.Ok() && ufseg.Start() < chunkFR.End; ufseg = ufseg.NextSegment() {
			if ufseg.ValuePtr().refs == 0 {
				continue
			}
			fr := ufseg.Range().Intersect(chunkFR)
			for off := fr.Start; off < fr.End; {
				maseg := f.memAcct.FindSegment(off)
				if !maseg.Ok() || maseg.ValuePtr().wasteOrReleasing {
					t.Errorf("used page %#x is not accounted as used in memAcct", off)
					break
				}
				off = maseg.End()
			}
		}
		return true
	})
}

// testImage is a saved image: its pages metadata file and pages file, as
// written to pages_meta.img and pages.img, and the pages files of its layers.
type testImage struct {
	meta  []byte
	pages []byte
	img   *checkpointimage.Image
	// layers holds the pages file of each layer, layers[0] being pages.
	layers [][]byte
}

// saveImage saves the MemoryFiles fs, in order, to a new image, as
// kernel.Kernel.saveMemoryFiles saves the application MemoryFile (fs[0]) and
// private MemoryFiles to one pages file. opts.PagesFile is set by saveImage.
func saveImage(t *testing.T, opts SaveOpts, fs ...*MemoryFile) *testImage {
	t.Helper()
	var meta, pages bytes.Buffer
	img, err := saveImageTo(&meta, stateio.NewIOWriter(&pages, 256<<10, 64, 4), opts, fs...)
	if err != nil {
		t.Fatalf("saving image: %v", err)
	}
	return &testImage{meta: meta.Bytes(), pages: pages.Bytes(), img: img, layers: [][]byte{pages.Bytes()}}
}

// saveImageTo saves fs to meta and pages as an image without layers,
// returning the first error.
func saveImageTo(meta io.Writer, pages stateio.AsyncWriter, opts SaveOpts, fs ...*MemoryFile) (*checkpointimage.Image, error) {
	var (
		wg      sync.WaitGroup
		pageErr error
	)
	wg.Add(1)
	apfs, err := StartAsyncPagesFileSave(pages, func(err error) {
		pageErr = err
		wg.Done()
	})
	if err != nil {
		return nil, err
	}
	opts.PagesFile = apfs
	opts.PageHashes = true
	ip := &pgallocpb.ImageProto{
		Layers:   []*pgallocpb.LayerProto{{}},
		PageHash: checkpointimage.PageHashXXH64,
	}
	// fs[1:] are private MemoryFiles; LoadFrom ignores their owners.
	for i := range fs[1:] {
		ip.PrivateMemoryFiles = append(ip.PrivateMemoryFiles, &pgallocpb.ResourceIDProto{Path: fmt.Sprintf("/private/%d", i)})
	}
	w := checkpointimage.NewWriter(meta, ip)
	var saveErr error
	for _, f := range fs {
		if saveErr = f.SaveTo(context.Background(), w, &opts); saveErr != nil {
			break
		}
	}
	apfs.MemoryFilesDone()
	wg.Wait()
	if saveErr != nil {
		return nil, saveErr
	}
	if pageErr != nil {
		return nil, pageErr
	}
	return w.Finish(apfs.PagesFileOffset())
}

// testLoad is an image being loaded into MemoryFiles with async page
// loading.
type testLoad struct {
	apfl *AsyncPagesFileLoad

	// done is closed when async page loading for every MemoryFile completes;
	// err is then the error that terminated it.
	done chan struct{}
	err  error

	// mfErrs receives the result of each MemoryFile's LoadOpts.DoneCallback.
	mfErrs chan error
}

// startLoad loads img, an image without layers, into fs, in the order in
// which they were saved, reading pages through ar. It returns once every MemoryFile's metadata is loaded;
// pages load asynchronously. The test must release fs's pages before it ends.
func startLoad(t *testing.T, img *testImage, ar stateio.AsyncReader, fs ...*MemoryFile) *testLoad {
	t.Helper()
	l := &testLoad{
		done:   make(chan struct{}),
		mfErrs: make(chan error, len(fs)),
	}
	apfl, err := StartAsyncPagesFileLoad(ar, func(err error) {
		l.err = err
		close(l.done)
	}, nil /* timeline */)
	if err != nil {
		t.Fatalf("StartAsyncPagesFileLoad: %v", err)
	}
	l.apfl = apfl
	t.Cleanup(func() {
		if r, ok := ar.(*testPagesFile); ok {
			r.finish()
		}
		apfl.MemoryFilesDone()
		select {
		case <-l.done:
		case <-time.After(testWaitTimeout):
			t.Errorf("timed out after %v waiting for async page loading to stop", testWaitTimeout)
		}
	})
	r := img.img.MemoryFileRecords()
	opts := LoadOpts{Image: img.img.Proto, PagesFiles: []*AsyncPagesFileLoad{apfl}}
	for _, f := range fs {
		opts.DoneCallback = func(err error) { l.mfErrs <- err }
		if err := f.LoadFrom(context.Background(), r, &opts); err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
	}
	apfl.MemoryFilesDone()
	if r.Len() != 0 {
		t.Fatalf("LoadFrom left %d bytes of pages metadata unread", r.Len())
	}
	return l
}

// wait waits for async page loading to complete and returns its error.
func (l *testLoad) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-l.done:
		return l.err
	case <-time.After(testWaitTimeout):
		t.Fatalf("timed out after %v waiting for async page loading to complete", testWaitTimeout)
		return nil
	}
}

// loadImage loads img into fs and waits for every page to be loaded.
func loadImage(t *testing.T, img *testImage, fs ...*MemoryFile) {
	t.Helper()
	l := startLoad(t, img, newTestPagesFile(t, img.pages, testPagesFileOpts{}), fs...)
	if err := l.wait(t); err != nil {
		t.Fatalf("async page loading: %v", err)
	}
	for range fs {
		if err := <-l.mfErrs; err != nil {
			t.Fatalf("async page loading of a MemoryFile: %v", err)
		}
	}
}

// releaseAll drops one reference on every used page of f, which releases the
// pages a loaded image holds.
func releaseAll(t *testing.T, f *MemoryFile) {
	t.Helper()
	var used []memmap.FileRange
	f.mu.Lock()
	for seg := f.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		if !seg.ValuePtr().wasteOrReleasing {
			used = append(used, seg.Range())
		}
	}
	f.mu.Unlock()
	for _, fr := range used {
		f.DecRef(fr)
	}
}

// testPagesFileOpts configures a testPagesFile.
type testPagesFileOpts struct {
	// maxReadBytes, maxRanges and maxParallel are the stateio.AsyncReader
	// limits; zero values select 256 KiB, 64 and 16.
	maxReadBytes uint64
	maxRanges    int
	maxParallel  int

	// latency is the time from the end of a read's transfer to its
	// completion; it does not occupy the device, so the latencies of queued
	// reads overlap.
	latency time.Duration

	// bandwidth is the transfer rate of each of the device's channels in
	// bytes per second; 0 is unlimited.
	bandwidth uint64

	// channels is the number of reads the device transfers at once, each at
	// bandwidth: 1 (the default) for a disk or a FUSE connection, several for
	// an object store serving parallel requests.
	channels int

	// If manual is true, virtual time advances only when the test calls
	// advance; otherwise Wait advances it to the completion it waits for.
	manual bool
}

// testPagesFile is a stateio.WaitOrAsyncReader over an in-memory pages file
// that models a storage device in virtual time: the device transfers reads in
// submission order, on the first of its channels to be free (with one channel,
// a FIFO queue, as a disk or a FUSE connection serves reads), at its
// bandwidth, and each read completes its latency after its transfer ends. A
// read's data is copied to its destination when it completes.
//
// The virtual clock makes queueing deterministic: a test can tell, to the
// nanosecond of virtual time, how long a read waited behind the reads
// submitted before it.
type testPagesFile struct {
	stateio.NoRegisterClientFD
	t    *testing.T
	opts testPagesFileOpts
	data []byte

	mu   sync.Mutex
	cond sync.Cond

	// now is the virtual time.
	now time.Duration

	// channelFree are the virtual times at which the device's channels finish
	// transferring the reads submitted to them.
	channelFree []time.Duration

	// inflight are submitted reads not yet returned by Wait, in completion
	// order.
	inflight []*testRead

	// reads records every read ever submitted, in submission order.
	reads []*testRead

	// blocked is true while r's user is blocked in Wait in manual mode.
	blocked bool

	// timedOut is set when a wait by the test times out.
	timedOut bool

	// failOff, if non-negative, is a pages file offset; reads covering it fail
	// with EIO after transferring the bytes before it.
	failOff int64

	closed bool
}

// testRead is a read submitted to a testPagesFile.
type testRead struct {
	id  int
	off int64
	len uint64
	dst stateio.LocalClientRanges

	// submitted and completed are the virtual times at which the read was
	// submitted and completes.
	submitted time.Duration
	completed time.Duration
}

// newTestPagesFile returns a testPagesFile that reads data. Its methods that
// wait for the loader fail t if it does not react within testWaitTimeout.
func newTestPagesFile(t *testing.T, data []byte, opts testPagesFileOpts) *testPagesFile {
	if opts.maxReadBytes == 0 {
		opts.maxReadBytes = 256 << 10
	}
	if opts.maxRanges == 0 {
		opts.maxRanges = 64
	}
	if opts.maxParallel == 0 {
		opts.maxParallel = 16
	}
	if opts.channels == 0 {
		opts.channels = 1
	}
	r := &testPagesFile{
		t:           t,
		opts:        opts,
		data:        data,
		channelFree: make([]time.Duration, opts.channels),
		failOff:     -1,
	}
	r.cond.L = &r.mu
	return r
}

// testWaitTimeout bounds how long tests wait for the loader to react.
const testWaitTimeout = 10 * time.Second

// waitLocked waits on r.cond until cond returns true, failing the test after
// testWaitTimeout.
//
// Preconditions: r.mu must be locked; the caller is the test's goroutine.
func (r *testPagesFile) waitLocked(what string, cond func() bool) {
	r.t.Helper()
	timer := time.AfterFunc(testWaitTimeout, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.timedOut = true
		r.cond.Broadcast()
	})
	defer timer.Stop()
	for !cond() {
		if r.timedOut {
			r.timedOut = false
			// The callers' deferred unlocks release r.mu as Fatalf exits
			// the goroutine.
			r.t.Fatalf("timed out after %v waiting for %s", testWaitTimeout, what)
		}
		r.cond.Wait()
	}
}

// Close implements stateio.AsyncReader.Close.
func (r *testPagesFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.inflight = nil
	r.cond.Broadcast()
	return nil
}

// MaxReadBytes implements stateio.AsyncReader.MaxReadBytes.
func (r *testPagesFile) MaxReadBytes() uint64 {
	return r.opts.maxReadBytes
}

// MaxRanges implements stateio.AsyncReader.MaxRanges.
func (r *testPagesFile) MaxRanges() int {
	return r.opts.maxRanges
}

// MaxParallel implements stateio.AsyncReader.MaxParallel.
func (r *testPagesFile) MaxParallel() int {
	return r.opts.maxParallel
}

// AddRead implements stateio.AsyncReader.AddRead.
func (r *testPagesFile) AddRead(id int, off int64, _ stateio.DestinationFile, dstFR memmap.FileRange, dstMap []byte) {
	r.submit(id, off, dstFR.Length(), stateio.LocalClientMapping(dstMap))
}

// AddReadv implements stateio.AsyncReader.AddReadv.
func (r *testPagesFile) AddReadv(id int, off int64, total uint64, _ stateio.DestinationFile, _ []memmap.FileRange, dstMaps []unix.Iovec) {
	r.submit(id, off, total, stateio.LocalClientMappings(dstMaps))
}

func (r *testPagesFile) submit(id int, off int64, length uint64, dst stateio.LocalClientRanges) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inflight) >= r.opts.maxParallel {
		panic(fmt.Sprintf("read %d submitted with %d reads in flight", id, len(r.inflight)))
	}
	ch := 0
	for i, free := range r.channelFree {
		if free < r.channelFree[ch] {
			ch = i
		}
	}
	start := max(r.now, r.channelFree[ch])
	var transfer time.Duration
	if r.opts.bandwidth != 0 {
		transfer = time.Duration(length * uint64(time.Second) / r.opts.bandwidth)
	}
	r.channelFree[ch] = start + transfer
	rd := &testRead{
		id:        id,
		off:       off,
		len:       length,
		dst:       dst,
		submitted: r.now,
		completed: r.channelFree[ch] + r.opts.latency,
	}
	i := len(r.inflight)
	for i > 0 && r.inflight[i-1].completed > rd.completed {
		i--
	}
	r.inflight = slices.Insert(r.inflight, i, rd)
	r.reads = append(r.reads, rd)
	r.cond.Broadcast()
}

// Wait implements stateio.AsyncReader.Wait.
func (r *testPagesFile) Wait(cs []stateio.Completion, minCompletions int) ([]stateio.Completion, error) {
	return r.wait(cs, minCompletions, nil)
}

// WaitOr implements stateio.WaitOrAsyncReader.WaitOr.
func (r *testPagesFile) WaitOr(cs []stateio.Completion, wake <-chan struct{}) ([]stateio.Completion, error) {
	return r.wait(cs, 1, wake)
}

// wait waits for minCompletions reads to complete, or, if wake is not nil, for
// wake to be readable.
func (r *testPagesFile) wait(cs []stateio.Completion, minCompletions int, wake <-chan struct{}) ([]stateio.Completion, error) {
	woken := false // protected by r.mu
	if wake != nil {
		select {
		case <-wake:
			return cs, nil
		default:
		}
		if r.opts.manual {
			// Turn a wake into a broadcast of r.cond while blocked below.
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				select {
				case <-wake:
					r.mu.Lock()
					woken = true
					r.cond.Broadcast()
					r.mu.Unlock()
				case <-stop:
				}
			}()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if woken {
			return cs, nil
		}
		if r.closed {
			return cs, fmt.Errorf("testPagesFile closed")
		}
		if minCompletions <= len(r.inflight) && (minCompletions == 0 || r.inflight[minCompletions-1].completed <= r.now) {
			break
		}
		if !r.opts.manual {
			r.now = r.inflight[minCompletions-1].completed
			break
		}
		r.blocked = true
		r.cond.Broadcast()
		r.cond.Wait()
		r.blocked = false
	}
	for len(r.inflight) != 0 && r.inflight[0].completed <= r.now {
		rd := r.inflight[0]
		r.inflight = r.inflight[1:]
		cs = append(cs, r.complete(rd))
	}
	r.cond.Broadcast()
	return cs, nil
}

// complete copies rd's data to its destination.
//
// Preconditions: r.mu must be locked.
func (r *testPagesFile) complete(rd *testRead) stateio.Completion {
	c := stateio.Completion{ID: rd.id}
	off := rd.off
	for _, m := range rd.dst.Mappings {
		n := uint64(len(m))
		if r.failOff >= off && r.failOff < off+int64(n) {
			c.N += uint64(copy(m[:r.failOff-off], r.data[off:]))
			c.Err = linuxerr.EIO
			return c
		}
		if off+int64(n) > int64(len(r.data)) {
			c.N += uint64(copy(m, r.data[off:]))
			c.Err = io.EOF
			return c
		}
		c.N += uint64(copy(m, r.data[off:]))
		off += int64(n)
	}
	return c
}

// nanotime returns r's virtual time in nanoseconds.
func (r *testPagesFile) nanotime() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int64(r.now)
}

// useClock makes r's virtual time the clock of async page loading until the
// test ends. It must be called before startLoad, so that loading stops before
// the clock is restored.
func (r *testPagesFile) useClock() {
	nanotime := aplNanotime
	aplNanotime = r.nanotime
	r.t.Cleanup(func() { aplNanotime = nanotime })
}

// failAt makes reads covering the pages file offset off fail with EIO.
func (r *testPagesFile) failAt(off int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOff = off
}

// finish makes r complete every read without waiting for advance.
func (r *testPagesFile) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts.manual = false
	r.cond.Broadcast()
}

// waitPending waits until r's user is blocked in Wait with every completion
// due by the current virtual time delivered, and returns the first read in
// flight, which completes later. r's user processes completions before
// waiting again, so after waitPending it has reacted to every completion so
// far.
func (r *testPagesFile) waitPending() *testRead {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waitPendingLocked()
}

// Preconditions: r.mu must be locked.
func (r *testPagesFile) waitPendingLocked() *testRead {
	r.t.Helper()
	r.waitLocked("a pending read", func() bool {
		return r.blocked && len(r.inflight) != 0 && r.inflight[0].completed > r.now
	})
	return r.inflight[0]
}

// advanceToNext waits for a pending read (see waitPending), advances r's
// virtual time to its completion, and returns it.
func (r *testPagesFile) advanceToNext() *testRead {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	rd := r.waitPendingLocked()
	r.now = rd.completed
	r.cond.Broadcast()
	return rd
}

// waitInflight waits until n reads are in flight.
func (r *testPagesFile) waitInflight(n int) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waitLocked(fmt.Sprintf("%d reads in flight", n), func() bool { return len(r.inflight) >= n })
}

// readsSnapshot returns a copy of every read submitted so far.
func (r *testPagesFile) readsSnapshot() []testRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	reads := make([]testRead, len(r.reads))
	for i, rd := range r.reads {
		reads[i] = *rd
	}
	return reads
}

// readAt returns the first read submitted that covers pages file offset off.
func (r *testPagesFile) readAt(off int64) (testRead, bool) {
	for _, rd := range r.readsSnapshot() {
		if rd.off <= off && off < rd.off+int64(rd.len) {
			return rd, true
		}
	}
	return testRead{}, false
}

// waitSubmitted waits until a read that covers pages file offset off has been
// submitted, and returns the first such read.
func (r *testPagesFile) waitSubmitted(off int64) testRead {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var found *testRead
	r.waitLocked(fmt.Sprintf("a read of pages file offset %d", off), func() bool {
		for _, rd := range r.reads {
			if rd.off <= off && off < rd.off+int64(rd.len) {
				found = rd
				return true
			}
		}
		return false
	})
	return *found
}
