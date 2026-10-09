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

package container

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
)

// incrementalDirtySource is a dirty source that incremental checkpoints are
// tested with, with the marking paths of its own that negative controls
// disable.
type incrementalDirtySource struct {
	name   string
	mode   config.DirtyTrackingMode
	breaks []config.DirtyTrackingBreak
}

// incrementalDirtySources are the dirty sources that incremental checkpoints
// are tested with. The negative controls also disable, with every source, the
// marking paths that all sources share (MapInternal, and tmpfs writes to
// disk-backed filestores).
var incrementalDirtySources = []incrementalDirtySource{
	{
		name:   "wp",
		mode:   config.DirtyTrackingWriteProtect,
		breaks: []config.DirtyTrackingBreak{config.DirtyTrackingBreakFault, config.DirtyTrackingBreakArm},
	},
}

// incrementalConf returns a copy of conf that tracks dirty pages with src, at
// its default unit, verifies tracking at every checkpoint if verify is true,
// and disables the marking path brk.
func incrementalConf(conf *config.Config, src incrementalDirtySource, verify bool, brk config.DirtyTrackingBreak) *config.Config {
	c := *conf
	c.DirtyTracking = src.mode
	c.DirtyTrackingVerify = config.DirtyTrackingVerifyOff
	if verify {
		c.DirtyTrackingVerify = config.DirtyTrackingVerifyHash
	}
	c.TestOnlyDirtyTrackingBreak = brk
	return &c
}

// escapesSteps are the steps of test/cmd/dirty_tracking/escapes that a run
// takes, in order. The last re-reads memory that must still be zero.
var escapesSteps = []string{"gofer", "net", "pipe", "tmpfs", "fork", "exec", "dontneed", "mremap", "mprotect", "aio", "reuse", "huge", "churn", "churn", "check"}

// escape describes writes that escaped dirty tracking, as a run of escapes
// found them.
type escape struct {
	// step is the step after which they were found.
	step string

	// verified is true if dirty tracking verification found them, and
	// false if comparing an incremental checkpoint's chain with a full
	// checkpoint did.
	verified bool

	// what describes them.
	what string
}

func (e *escape) String() string {
	by := "comparison with a full checkpoint"
	if e.verified {
		by = "verification"
	}
	return fmt.Sprintf("after step %q, found by %s: %s", e.step, by, e.what)
}

// pageAt identifies a page of a checkpoint's memory.
type pageAt struct {
	// mf is the index of the MemoryFile in the image.
	mf int

	// off is the page's offset in the MemoryFile.
	off uint64
}

// reports reads the lines that a workload appends to a file.
type reports struct {
	path string

	// lines is the number of lines read.
	lines int
}

// next returns the next line appended to r's file, waiting for it while
// cont's sandbox runs.
func (r *reports) next(cont *Container) (string, error) {
	var line string
	if err := testutil.Poll(func() error {
		data, err := os.ReadFile(r.path)
		if err != nil {
			return err
		}
		lines := strings.SplitAfter(string(data), "\n")
		if len(lines) > r.lines && strings.HasSuffix(lines[r.lines], "\n") {
			line = strings.TrimSuffix(lines[r.lines], "\n")
			return nil
		}
		if running, err := cont.IsSandboxRunning(); err != nil || !running {
			return &backoff.PermanentError{Err: fmt.Errorf("the sandbox stopped (%v) before %s had %d lines", err, r.path, r.lines+1)}
		}
		return fmt.Errorf("%s has %d lines, waiting for more", r.path, r.lines)
	}, pollTimeout); err != nil {
		return "", err
	}
	r.lines++
	return line, nil
}

// escapesRun is a run of escapes under a chain of incremental checkpoints.
type escapesRun struct {
	t   *testing.T
	dir string

	// cont is the container that runs escapes.
	cont *Container

	// chain holds the directories of the images of the chain, oldest first.
	chain []string

	// out holds what escapes reports.
	out reports

	// vdso is the page found to change by itself between two checkpoints
	// of the same moment, which must be the VDSO parameter page that the
	// Sentry's timekeeper updates: one page, of kind usage.System, always the
	// same, and saved by every incremental checkpoint.
	vdso *pageAt
}

// runEscapes runs escapes in a container with conf, whose dirty tracking is
// on, and after each of its steps takes an incremental checkpoint, of the
// image the container was last checkpointed to or restored from, and a full
// checkpoint, compares the chain's memory with the full checkpoint's page by
// page, and restores the chain into a new container that runs the next step.
// The next comparison thus also checks the restore. If overlay is true, the
// root filesystem is overlaid with a tmpfs backed by a file on disk, which
// escapes writes to as well. runEscapes returns the first escape it finds, or
// nil if there is none; it fails t on any other error.
//
// This is CRIU's zdtm "pre-dump and compare" and Firecracker's
// test_cmp_full_and_first_diff_mem on gVisor's images.
func runEscapes(t *testing.T, conf *config.Config, overlay bool) *escape {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "escapes")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	app, err := testutil.FindFile("test/cmd/dirty_tracking/escapes")
	if err != nil {
		t.Fatalf("finding escapes: %v", err)
	}
	data := make([]byte, 8<<20)
	rand.Read(data)
	dataPath := filepath.Join(dir, "data")
	if err := os.WriteFile(dataPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	tmpfsDir := filepath.Join(dir, "tmpfs")
	if err := os.Mkdir(tmpfsDir, 0777); err != nil {
		t.Fatal(err)
	}
	args := []string{app, dir, dataPath, "/dev/shm/escapes", tmpfsDir}
	if overlay {
		filestoreDir := filepath.Join(dir, "filestore")
		if err := os.Mkdir(filestoreDir, 0777); err != nil {
			t.Fatal(err)
		}
		c := *conf
		if err := c.Overlay2.Set("root:dir=" + filestoreDir); err != nil {
			t.Fatal(err)
		}
		conf = &c
		args = append(args, "/escapes")
	}
	spec := testutil.NewSpecWithArgs(args...)
	spec.Mounts = append(spec.Mounts, specs.Mount{Type: "tmpfs", Destination: tmpfsDir})
	if overlay {
		// The overlay keeps the root filesystem's writes in its filestore.
		spec.Root.Readonly = false
	}
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	t.Cleanup(cleanup)
	r := &escapesRun{t: t, dir: dir, out: reports{path: filepath.Join(dir, "out")}}
	t.Cleanup(func() {
		if r.cont != nil {
			r.cont.Destroy()
		}
	})
	r.cont, err = New(conf, Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir})
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	if err := r.cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if got, err := r.out.next(r.cont); err != nil || got != "READY" {
		t.Fatalf("escapes reported %q (%v), want READY", got, err)
	}

	// The first image of the chain is full.
	first := r.imageDir("0")
	if err := r.cont.Checkpoint(conf, first, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone, Resume: true}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}
	r.checkParentID(first, "")
	r.chain = append(r.chain, first)

	for i, step := range escapesSteps {
		sum := r.step(step)
		if step == "check" && sum != "0000000000000000" {
			t.Errorf("after the restore of %d images, %d words of memory that escapes only read are not zero", len(r.chain), parseHex(t, sum))
		}
		parent := r.chain[len(r.chain)-1]
		delta := r.imageDir(fmt.Sprint(i + 1))
		if err := r.cont.Checkpoint(conf, delta, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone, Resume: true, ParentImagePath: parent}); err != nil {
			if strings.Contains(err.Error(), kernel.ErrDirtyTrackingEscapes.Error()) {
				return &escape{step: step, verified: true, what: err.Error()}
			}
			t.Fatalf("error checkpointing container incrementally after step %q: %v", step, err)
		}
		r.chain = append(r.chain, delta)
		r.checkParentID(delta, parent)
		r.logDelta(step, delta)

		// A full checkpoint of the same moment, which stops the
		// container.
		full := r.imageDir("full")
		if err := r.cont.Checkpoint(conf, full, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone}); err != nil {
			// Verification also checks full checkpoints. It sees writes
			// that escaped to pages not known to be committed (as
			// pages of disk-backed filestores written with pwritev2
			// are until a save finds them) only once a save has found
			// them: at the save after the one that missed them.
			if strings.Contains(err.Error(), kernel.ErrDirtyTrackingEscapes.Error()) {
				return &escape{step: step, verified: true, what: err.Error()}
			}
			t.Fatalf("error checkpointing container after step %q: %v", step, err)
		}
		r.cont.Destroy()
		r.cont = nil
		r.checkParentID(full, "")
		if what := r.compare(delta, full); what != "" {
			return &escape{step: step, what: what}
		}
		if err := os.RemoveAll(full); err != nil {
			t.Fatal(err)
		}
		if i == len(escapesSteps)-1 {
			break
		}

		// Restore the chain into a new container, every other one in the
		// background, which runs the next step.
		r.cont, err = New(conf, Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir})
		if err != nil {
			t.Fatalf("error creating container: %v", err)
		}
		if err := r.cont.Restore(conf, delta, r.chain[:len(r.chain)-1], false /* direct */, i%2 == 1 /* background */, nil /* networkArgs */); err != nil {
			t.Fatalf("error restoring the chain of %d images after step %q: %v", len(r.chain), step, err)
		}
	}
	return nil
}

// imageDir returns a new directory for an image named name.
func (r *escapesRun) imageDir(name string) string {
	dir := filepath.Join(r.dir, "image-"+name)
	if err := os.Mkdir(dir, 0777); err != nil {
		r.t.Fatal(err)
	}
	return dir
}

// step runs step, and returns the checksum that escapes reports for it.
func (r *escapesRun) step(step string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "step"), []byte(step), 0644); err != nil {
		r.t.Fatal(err)
	}
	if err := r.cont.SignalContainer(unix.SIGUSR1, false /* all */); err != nil {
		r.t.Fatalf("error signaling container: %v", err)
	}
	got, err := r.out.next(r.cont)
	if err != nil {
		r.t.Fatalf("waiting for the end of step %q: %v", step, err)
	}
	sum, ok := strings.CutPrefix(got, "DONE "+step+" ")
	if !ok {
		r.t.Fatalf("escapes reported %q, want the end of step %q", got, step)
	}
	return sum
}

// logDelta logs what the incremental image in dir, saved after step, holds.
func (r *escapesRun) logDelta(step, dir string) {
	r.t.Helper()
	img, err := checkpointimage.ReadMetadataFile(filepath.Join(dir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		r.t.Fatalf("error reading image %s: %v", dir, err)
	}
	var own, committed []uint64
	for i, mf := range img.MemoryFiles {
		var n uint64
		for _, e := range mf.GetExtents() {
			if e.GetLayer() == 0 {
				n += e.GetEnd() - e.GetStart()
			}
		}
		own = append(own, n)
		committed = append(committed, img.CommittedBytes(i))
	}
	r.t.Logf("after step %q: %d layers; bytes saved per MemoryFile %v, of %v committed", step, len(img.Layers()), own, committed)
}

// checkParentID checks that the state file of the image in dir names as its
// parent the image in parent, or none if parent is empty.
func (r *escapesRun) checkParentID(dir, parent string) {
	r.t.Helper()
	md, err := readStateFileMetadata(filepath.Join(dir, checkpointfiles.StateFileName))
	if err != nil {
		r.t.Fatalf("error reading state file metadata: %v", err)
	}
	var want string
	if parent != "" {
		d, err := checkpointimage.FileDigest(filepath.Join(parent, checkpointfiles.PagesMetadataFileName))
		if err != nil {
			r.t.Fatal(err)
		}
		want = d.String()
	}
	if got := md[checkpointimage.ParentMetadataKey]; got != want {
		r.t.Errorf("image %s has parent_id %q in its state file, want %q", dir, got, want)
	}
}

// imageMemory reads the memory of an image.
type imageMemory struct {
	img *checkpointimage.Image

	// pages holds the pages file of each layer of img.
	pages []*os.File
}

// openImage opens the image in dir, whose layers are images of r's chain.
func (r *escapesRun) openImage(dir string) *imageMemory {
	r.t.Helper()
	img, err := checkpointimage.ReadMetadataFile(filepath.Join(dir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		r.t.Fatalf("error reading image %s: %v", dir, err)
	}
	layers, err := checkpointimage.FindLayers(img, dir, r.chain)
	if err != nil {
		r.t.Fatalf("error finding the layers of image %s: %v", dir, err)
	}
	m := &imageMemory{img: img}
	for _, d := range append([]string{dir}, layers...) {
		f, err := os.Open(filepath.Join(d, checkpointfiles.PagesFileName))
		if err != nil {
			r.t.Fatal(err)
		}
		m.pages = append(m.pages, f)
	}
	return m
}

func (m *imageMemory) close() {
	for _, f := range m.pages {
		f.Close()
	}
}

// committedPages returns the offsets of the pages that the images of ms hold
// data for in their i-th MemoryFiles, in increasing order.
func committedPages(i int, ms ...*imageMemory) []uint64 {
	var offs []uint64
	for _, m := range ms {
		for _, e := range m.img.MemoryFileImage(i).Extents {
			for off := e.Start; off < e.End; off += checkpointimage.PageSize {
				offs = append(offs, off)
			}
		}
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	var out []uint64
	for _, off := range offs {
		if n := len(out); n == 0 || out[n-1] != off {
			out = append(out, off)
		}
	}
	return out
}

// kindAt returns the memory accounting kind of the page at off in the i-th
// MemoryFile of m, or -1 if m does not know it.
func (m *imageMemory) kindAt(i int, off uint64) int {
	for _, ma := range m.img.MemoryFiles[i].GetMemAcct() {
		if ma.GetStart() <= off && off < ma.GetEnd() {
			return int(ma.GetKind())
		}
	}
	return -1
}

// compare compares the memory of the incremental image in deltaDir, read
// through its chain, with that of the full image in fullDir, page by page.
// It returns a description of the pages that differ, but for the Sentry's
// VDSO parameter page, or "" if none does.
func (r *escapesRun) compare(deltaDir, fullDir string) string {
	r.t.Helper()
	delta, full := r.openImage(deltaDir), r.openImage(fullDir)
	defer delta.close()
	defer full.close()
	if n := len(full.img.Layers()); n != 1 {
		r.t.Fatalf("full image %s has %d layers", fullDir, n)
	}
	if got, want := fmt.Sprint(delta.img.Proto.GetPrivateMemoryFiles()), fmt.Sprint(full.img.Proto.GetPrivateMemoryFiles()); got != want {
		r.t.Fatalf("incremental image has private MemoryFiles %s, the full image %s", got, want)
	}
	differ := make(map[string][]pageAt)
	var nr int
	a, b := make([]byte, checkpointimage.PageSize), make([]byte, checkpointimage.PageSize)
	for i := range full.img.MemoryFiles {
		dm := delta.img.MemoryFileImage(i)
		dc, fc := dm.Cursor(), full.img.MemoryFileImage(i).Cursor()
		for _, off := range committedPages(i, delta, full) {
			delta.readPage(r.t, &dc, off, a)
			full.readPage(r.t, &fc, off, b)
			if bytes.Equal(a, b) {
				continue
			}
			kind := full.kindAt(i, off)
			if kind < 0 {
				kind = delta.kindAt(i, off)
			}
			p := pageAt{mf: i, off: off}
			if kind == int(usage.System) && (r.vdso == nil || *r.vdso == p) {
				// The VDSO parameter page, which the Sentry
				// rewrites by itself (at every resume and restore,
				// among others): the first System page found to
				// differ, which must be the only one, always the
				// same, and in the delta itself, since it was
				// rewritten after the parent was saved.
				if e, ok := dm.ExtentAt(off); ok && e.Layer == 0 {
					r.vdso = &p
					continue
				}
			}
			name := kindName(kind)
			differ[name] = append(differ[name], p)
			nr++
		}
	}
	if nr == 0 {
		return ""
	}
	var desc []string
	for name, ps := range differ {
		desc = append(desc, fmt.Sprintf("%d %s pages, first %+v", len(ps), name, ps[0]))
	}
	sort.Strings(desc)
	return fmt.Sprintf("%d pages differ: %s", nr, strings.Join(desc, "; "))
}

// readPage reads the page at off, which c looks up, into pg: zeroes if m
// holds no data for it.
func (m *imageMemory) readPage(t *testing.T, c *checkpointimage.Cursor, off uint64, pg []byte) {
	t.Helper()
	e, ok, _, _ := c.Lookup(off)
	if !ok {
		clear(pg)
		return
	}
	if _, err := m.pages[e.Layer].ReadAt(pg, int64(e.OffsetOf(off))); err != nil {
		t.Fatalf("error reading page %#x from layer %d of %v: %v", off, e.Layer, m.img.Digest, err)
	}
}

// TestCheckpointIncrementalNoEscapes checks that a chain of incremental
// checkpoints, of a workload that writes memory through every path a write can
// take, holds the same memory as full checkpoints of the same moments, and
// restores it; with dirty tracking verified at every checkpoint, on its own and
// with the root filesystem overlaid on a disk-backed filestore.
func TestCheckpointIncrementalNoEscapes(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, src := range incrementalDirtySources {
			for _, overlay := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/overlay=%t", name, src.name, overlay), func(t *testing.T) {
					start := time.Now()
					if e := runEscapes(t, incrementalConf(conf, src, true /* verify */, config.DirtyTrackingBreakNone), overlay); e != nil {
						t.Errorf("writes escaped dirty tracking %s", e)
					} else {
						t.Logf("%d steps in %v", len(escapesSteps), time.Since(start))
					}
				})
			}
		}
	}
}

// TestCheckpointIncrementalNoEscapesNegativeControls checks that
// TestCheckpointIncrementalNoEscapes would catch a write that escaped dirty
// tracking: with each marking path disabled, verification fails an
// incremental checkpoint, and, without verification, the comparison of the
// chain with a full checkpoint finds pages that differ.
func TestCheckpointIncrementalNoEscapesNegativeControls(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, src := range incrementalDirtySources {
			breaks := append([]config.DirtyTrackingBreak{config.DirtyTrackingBreakMapInternal, config.DirtyTrackingBreakTmpfs}, src.breaks...)
			for _, brk := range breaks {
				// Only tmpfs on a disk-backed filestore, which the
				// root filesystem's overlay is, writes without
				// MapInternal.
				overlay := brk == config.DirtyTrackingBreakTmpfs
				for _, verify := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/%s/%v/verify=%t", name, src.name, brk, verify), func(t *testing.T) {
						e := runEscapes(t, incrementalConf(conf, src, verify, brk), overlay)
						switch {
						case e == nil:
							t.Errorf("no write escaped dirty tracking with %v disabled", brk)
						case e.verified != verify:
							t.Errorf("writes escaped dirty tracking %s; want them found by verification: %t", e, verify)
						default:
							t.Logf("writes escaped dirty tracking %s", e)
						}
					})
				}
			}
		}
	}
}

// runMemTouch runs test/cmd/dirty_tracking/mem_touch, CRIU's mem-touch, in a
// container with conf, whose dirty tracking is on, while it takes a chain of
// incremental checkpoints, each of the previous one, with a restore of the
// chain into a new container after every fourth, and asks mem_touch to check
// its memory after each restore. It returns mem_touch's results, and an error
// if mem_touch stopped after a restore.
func runMemTouch(t *testing.T, conf *config.Config) ([]string, error) {
	const (
		checkpoints  = 12
		restoreEvery = 4
		interval     = 100 * time.Millisecond
	)
	dir, err := os.MkdirTemp(testutil.TmpDir(), "mem-touch")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	app, err := testutil.FindFile("test/cmd/dirty_tracking/mem_touch")
	if err != nil {
		t.Fatalf("finding mem_touch: %v", err)
	}
	spec := testutil.NewSpecWithArgs(app, dir, "/dev/shm/mem-touch-shadow")
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	t.Cleanup(cleanup)
	cont, err := New(conf, Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir})
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	t.Cleanup(func() { cont.Destroy() })
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	out := reports{path: filepath.Join(dir, "out")}
	if got, err := out.next(cont); err != nil || got != "READY" {
		t.Fatalf("mem_touch reported %q (%v), want READY", got, err)
	}

	var (
		chain   []string
		results []string
	)
	for i := 0; i <= checkpoints; i++ {
		time.Sleep(interval)
		image := filepath.Join(dir, fmt.Sprint("image-", i))
		if err := os.Mkdir(image, 0777); err != nil {
			t.Fatal(err)
		}
		opts := sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone, Resume: i%restoreEvery != 0 || i == 0}
		if i != 0 {
			opts.ParentImagePath = chain[len(chain)-1]
		}
		if err := cont.Checkpoint(conf, image, opts); err != nil {
			t.Fatalf("error checkpointing container (checkpoint %d): %v", i, err)
		}
		chain = append(chain, image)
		if opts.Resume {
			continue
		}
		cont.Destroy()
		cont, err = New(conf, Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir})
		if err != nil {
			t.Fatalf("error creating container: %v", err)
		}
		if err := cont.Restore(conf, image, chain[:len(chain)-1], false /* direct */, false /* background */, nil /* networkArgs */); err != nil {
			t.Fatalf("error restoring the chain of %d images: %v", len(chain), err)
		}
		// mem_touch may stop, if its memory lost writes.
		var result string
		err = cont.SignalContainer(unix.SIGUSR1, false /* all */)
		if err == nil {
			result, err = out.next(cont)
		}
		if err != nil {
			return results, fmt.Errorf("after restoring %d images: %w", len(chain), err)
		}
		results = append(results, fmt.Sprintf("after restoring %d images: %s", len(chain), result))
	}
	return results, nil
}

// TestCheckpointIncrementalMemTouch runs CRIU's mem-touch, which writes memory
// without pause and checks it against a shadow table, under a chain of
// incremental checkpoints with dirty tracking verified at every checkpoint,
// and restores of the chain: after every restore, the check must pass. As a
// negative control, with each of the dirty source's own marking paths
// disabled, and no verification, a check must fail, or mem_touch stop: its
// stack loses writes too, so that it may return into the wrong caller. (With
// MapInternal's disabled, restores fail before any check, on the VDSO
// parameter page, which TestCheckpointIncrementalNoEscapesNegativeControls
// covers.)
func TestCheckpointIncrementalMemTouch(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, src := range incrementalDirtySources {
			t.Run(fmt.Sprintf("%s/%s", name, src.name), func(t *testing.T) {
				results, err := runMemTouch(t, incrementalConf(conf, src, true /* verify */, config.DirtyTrackingBreakNone))
				if err != nil {
					t.Errorf("mem_touch: %v", err)
				}
				for _, result := range results {
					if !strings.Contains(result, ": PASS ") {
						t.Errorf("mem_touch's check %s", result)
					}
				}
			})
			for _, brk := range src.breaks {
				t.Run(fmt.Sprintf("%s/%s/%v", name, src.name, brk), func(t *testing.T) {
					results, err := runMemTouch(t, incrementalConf(conf, src, false /* verify */, brk))
					for _, result := range results {
						if strings.Contains(result, ": FAIL ") {
							t.Logf("mem_touch's check %s", result)
							return
						}
					}
					if err != nil {
						t.Logf("mem_touch: %v", err)
						return
					}
					t.Errorf("mem_touch's checks passed with %v disabled: %q", brk, results)
				})
			}
		}
	}
}

// kindName returns the name of the memory accounting kind k, or a description
// of it if it is not known.
func kindName(k int) string {
	switch usage.MemoryKind(k) {
	case usage.System:
		return "System"
	case usage.Anonymous:
		return "Anonymous"
	case usage.PageCache:
		return "PageCache"
	case usage.Tmpfs:
		return "Tmpfs"
	case usage.Ramdiskfs:
		return "Ramdiskfs"
	case usage.Mapped:
		return "Mapped"
	}
	return fmt.Sprintf("unknown kind %d", k)
}

// parseHex parses s as a hexadecimal number.
func parseHex(t *testing.T, s string) uint64 {
	t.Helper()
	var n uint64
	if _, err := fmt.Sscanf(s, "%x", &n); err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return n
}
