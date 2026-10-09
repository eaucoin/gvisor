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

package kernel

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// TestPrecopyStopRule drives precopyNext with experiment 06's model of
// pre-copy (sim06.py): a writer dirties rate MiB/s of random pages of a
// 256 MiB image, and a round writes the pages dirtied during the previous one
// at 100 MiB/s, which leaves N(1 - exp(-rate t / N)) MiB dirty after t
// seconds; throttled, the writer dirties at most precopyThrottleLimit. The
// rounds are the model's, which predicted 06's measurements within one round.
// The 200 MiB/s writer stops by the halving rule rather than after 8 rounds
// that write 7.3 times the image, and, throttled, converges.
func TestPrecopyStopRule(t *testing.T) {
	const (
		imageMiB = 256
		bwMiB    = 100
		cost     = time.Second / bwMiB
	)
	limitMiB := float64(precopyThrottleLimit(cost)) / (1 << 20)
	for _, test := range []struct {
		rateMiB   float64
		budget    time.Duration
		maxRounds int
		throttle  bool
		rounds    int
		stop      precopyStop
	}{
		{rateMiB: 1, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 1, stop: precopyConverged},
		{rateMiB: 10, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 2, stop: precopyConverged},
		{rateMiB: 50, budget: 30 * time.Millisecond, maxRounds: 8, rounds: 6, stop: precopyConverged},
		{rateMiB: 50, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 5, stop: precopyConverged},
		{rateMiB: 50, budget: 300 * time.Millisecond, maxRounds: 8, rounds: 3, stop: precopyConverged},
		{rateMiB: 50, budget: 30 * time.Millisecond, maxRounds: 4, rounds: 4, stop: precopyRoundCap},
		{rateMiB: 50, budget: 100 * time.Millisecond, maxRounds: 8, throttle: true, rounds: 5, stop: precopyConverged},
		{rateMiB: 200, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 1, stop: precopyNotHalved},
		{rateMiB: 200, budget: 100 * time.Millisecond, maxRounds: 8, throttle: true, rounds: 4, stop: precopyConverged},
		{rateMiB: 400, budget: 100 * time.Millisecond, maxRounds: 8, throttle: true, rounds: 4, stop: precopyConverged},
	} {
		opts := PrecopyOpts{Budget: test.budget, MaxRounds: test.maxRounds, Throttle: test.throttle}
		pending := float64(imageMiB)
		rounds, stop, throttling := 0, precopyContinue, false
		for round := 0; stop == precopyContinue; round++ {
			written := pending
			rate := test.rateMiB
			if throttling {
				rate = min(rate, limitMiB)
			}
			pending = imageMiB * (1 - math.Exp(-rate*(written/bwMiB)/imageMiB))
			rounds++
			var throttle bool
			stop, throttle = precopyNext(round, uint64(written*(1<<20)), uint64(pending*(1<<20)), cost, opts, throttling)
			throttling = throttling || throttle
		}
		if rounds != test.rounds || stop != test.stop {
			t.Errorf("%v MiB/s, budget %v, at most %d rounds, throttle %t: %d rounds, stopped by %v; want %d rounds, stopped by %v", test.rateMiB, test.budget, test.maxRounds, test.throttle, rounds, stop, test.rounds, test.stop)
		}
	}
}

// imagePage returns the contents of the page at off in MemoryFile i of img,
// reading its layers from the image directories dirs, by digest.
func imagePage(t *testing.T, img *checkpointimage.Image, i int, dirs map[checkpointimage.Digest]string, off uint64) []byte {
	t.Helper()
	e, ok := img.MemoryFileImage(i).ExtentAt(off)
	if !ok {
		return make([]byte, hostarch.PageSize)
	}
	d := img.Digest
	if e.Layer != 0 {
		d = img.Layers()[e.Layer].Digest
	}
	f, err := os.Open(filepath.Join(dirs[d], checkpointfiles.PagesFileName))
	if err != nil {
		t.Fatalf("opening the pages file of %v: %v", d, err)
	}
	defer f.Close()
	pg := make([]byte, hostarch.PageSize)
	if _, err := f.ReadAt(pg, int64(e.OffsetOf(off))); err != nil {
		t.Fatalf("reading page %#x from %v: %v", off, d, err)
	}
	return pg
}

// checkImagePages checks that the pages of fr in MemoryFile i of img hold the
// contents of the same pages of mf.
func checkImagePages(t *testing.T, img *checkpointimage.Image, i int, dirs map[checkpointimage.Digest]string, mf *pgalloc.MemoryFile, fr memmap.FileRange) {
	t.Helper()
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		want := make([]byte, hostarch.PageSize)
		if _, err := unix.Pread(mf.FD(), want, int64(off)); err != nil {
			t.Fatalf("Pread: %v", err)
		}
		if got := imagePage(t, img, i, dirs, off); !bytes.Equal(got, want) {
			t.Errorf("MemoryFile %d, page %d: image has %x..., MemoryFile has %x...", i, (off-fr.Start)/hostarch.PageSize, got[:8], want[:8])
		}
	}
}

// slowArmSource is a DirtySource whose arming takes delay.
type slowArmSource struct {
	DirtySource
	delay time.Duration
}

// Arm implements DirtySource.Arm.
func (s *slowArmSource) Arm(ctx context.Context, paused bool) error {
	time.Sleep(s.delay)
	return s.DirtySource.Arm(ctx, paused)
}

// TestPrecopy checks a save that completes a pre-copy of 2 rounds, with
// writes while they run: the save writes only the pages dirtied during the
// last round, its image holds the MemoryFile's contents, verification finds
// no escape, and the image is the parent of the next incremental save. The
// pre-copy's statistics and metrics describe its rounds.
func TestPrecopy(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		name := "full"
		if incremental {
			name = "incremental"
		}
		t.Run(name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			k, src, fr := dirtyTestKernel(t, ctx, true /* verify */)
			dirs := make(map[checkpointimage.Digest]string)
			full, dir, err := saveImageOpts(t, ctx, k, testSaveOpts{})
			if err != nil {
				t.Fatalf("full save: %v", err)
			}
			dirs[full.Digest] = dir
			var parent *checkpointimage.Digest
			if incremental {
				parent = &full.Digest
			}
			src.write(t, fr, 0, 0xa0)

			// Each harvest finds a page written since the previous one: by
			// round 0's epoch, after round 0, by round 1's epoch, after
			// round 1 and by the save's epoch.
			harvests := 0
			src.onHarvest = func() {
				harvests++
				if harvests <= 5 {
					src.write(t, fr, uint64(harvests), byte(0xa0+harvests))
				}
			}
			// Arming for each round stops tasks for 10 ms.
			const armDelay = 10 * time.Millisecond
			k.dirty.Sources = []DirtySource{&slowArmSource{DirtySource: src, delay: armDelay}}
			var (
				stats        precopyStats
				roundsBefore = precopyRounds.Value()
				copiedBefore = precopyBytes.Value()
				capBefore    = precopyStops.Value(precopyStopReasons[precopyRoundCap])
			)
			img, dir, err := saveImageOpts(t, ctx, k, testSaveOpts{
				parent:  parent,
				precopy: &PrecopyOpts{Budget: 0, MaxRounds: 2},
				stats:   &stats,
			})
			if err != nil {
				t.Fatalf("save with pre-copy: %v", err)
			}
			src.onHarvest = nil
			dirs[img.Digest] = dir
			if harvests != 5 {
				t.Errorf("sources harvested %d times, want 5", harvests)
			}
			checkImagePages(t, img, 0, dirs, k.mf, fr)

			// The rounds wrote what they copied; the save wrote the pages
			// dirtied during round 1: those written after it and by the
			// save's epoch.
			copied := precopyBytes.Value() - copiedBefore
			if got, want := img.Layers()[0].PagesSize-copied, uint64(2*hostarch.PageSize); got != want {
				t.Errorf("the save wrote %d bytes, want %d", got, want)
			}
			if got, ok := k.LastImageDigest(); !ok || got != img.Digest {
				t.Errorf("LastImageDigest: got %v, %t; want %v", got, ok, img.Digest)
			}

			// Round 0 copied every page with data (a full save) or the 2
			// pages written since the parent; round 1 the 2 pages written
			// during round 0. 1 page was written during each.
			want := precopyStats{
				roundBytes:   []uint64{2 * hostarch.PageSize, 2 * hostarch.PageSize},
				pendingBytes: []uint64{hostarch.PageSize, hostarch.PageSize},
				stop:         precopyRoundCap,
			}
			if !incremental {
				// Every page with data.
				want.roundBytes[0] = stats.roundBytes[0]
				if stats.roundBytes[0] < 16*hostarch.PageSize {
					t.Errorf("round 0 of a full save wrote %d bytes, want at least the %d of the test's pages", stats.roundBytes[0], 16*hostarch.PageSize)
				}
			}
			if !slices.Equal(stats.roundBytes, want.roundBytes) || !slices.Equal(stats.pendingBytes, want.pendingBytes) || stats.stop != want.stop {
				t.Errorf("rounds wrote %v bytes, leaving %v, and stopped by %v; want %v, leaving %v, stopped by %v", stats.roundBytes, stats.pendingBytes, stats.stop, want.roundBytes, want.pendingBytes, want.stop)
			}
			if stats.longestStall < armDelay {
				t.Errorf("longest stall %v, want at least the %v that arming took", stats.longestStall, armDelay)
			}
			if got := precopyRounds.Value() - roundsBefore; got != 2 {
				t.Errorf("%s counted %d rounds, want 2", "/checkpoint/precopy_rounds", got)
			}
			if got := copied; got != stats.roundBytes[0]+stats.roundBytes[1] {
				t.Errorf("%s counted %d bytes, want %d", "/checkpoint/precopy_bytes", got, stats.roundBytes[0]+stats.roundBytes[1])
			}
			if got := precopyStops.Value(precopyStopReasons[precopyRoundCap]) - capBefore; got != 1 {
				t.Errorf("%s{reason=round_cap} counted %d pre-copies, want 1", "/checkpoint/precopy_stops", got)
			}
		})
	}
}

// TestPrecopyFailureKeepsDirtyPages checks that a pre-copy that fails returns
// the pages its rounds took from the dirty sets, so that the next save writes
// them: whether it fails in round 0, or in round 1, after round 0 copied
// pages.
func TestPrecopyFailureKeepsDirtyPages(t *testing.T) {
	for _, failRound := range []int{0, 1} {
		t.Run(map[int]string{0: "round0", 1: "round1"}[failRound], func(t *testing.T) {
			ctx := contexttest.Context(t)
			k, src, fr := dirtyTestKernel(t, ctx, false /* verify */)
			full, _, err := saveImage(t, ctx, k, nil, noLimit)
			if err != nil {
				t.Fatalf("full save: %v", err)
			}
			// Round 0 copies these 4 pages; 1 page is written during it,
			// which round 1 copies. The pages file takes failRound * 4
			// pages.
			for _, i := range []uint64{4, 6, 8, 10} {
				src.write(t, fr, i, 0xaa)
			}
			harvests := 0
			src.onHarvest = func() {
				harvests++
				if harvests == 2 {
					src.write(t, fr, 5, 0xbb)
				}
			}
			_, _, err = saveImageOpts(t, ctx, k, testSaveOpts{
				parent:   &full.Digest,
				fullDisk: true,
				limit:    failRound * 4 * hostarch.PageSize,
				precopy:  &PrecopyOpts{Budget: 0, MaxRounds: 8},
			})
			src.onHarvest = nil
			if err == nil {
				t.Fatalf("pre-copy to a full disk succeeded")
			}
			if harvests < 2*failRound+1 {
				t.Fatalf("pre-copy failed after %d harvests, before round %d", harvests, failRound)
			}
			src.write(t, fr, 12, 0xcc)
			delta, _, err := saveImage(t, ctx, k, &full.Digest, noLimit)
			if err != nil {
				t.Fatalf("incremental save after a failed pre-copy: %v", err)
			}
			want := slices.Repeat([]checkpointimage.Digest{full.Digest}, 16)
			for _, i := range []uint64{4, 6, 8, 10, 12} {
				want[i] = delta.Digest
			}
			if failRound == 1 {
				want[5] = delta.Digest
			}
			if got := pageImages(t, delta, fr); !slices.Equal(got, want) {
				t.Errorf("images of the delta's pages: got %v, want %v", got, want)
			}
		})
	}
}

// privateTestMemoryFile returns a private MemoryFile owned by id, which holds
// one allocation of 8 pages written with data.
func privateTestMemoryFile(t *testing.T, id checkpoint.ResourceID) (*pgalloc.MemoryFile, memmap.FileRange) {
	t.Helper()
	memfd, err := memutil.CreateMemFD("private", 0)
	if err != nil {
		t.Fatalf("CreateMemFD: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(os.NewFile(uintptr(memfd), "private"), pgalloc.MemoryFileOpts{ResourceID: id, DisableMemoryAccounting: true})
	if err != nil {
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(mf.Destroy)
	fr, err := mf.Allocate(8*hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Tmpfs})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	t.Cleanup(func() { mf.DecRef(fr) })
	for i := uint64(0); i < 8; i++ {
		pwrite(t, mf, fr.Start+i*hostarch.PageSize, byte(0x10+i))
	}
	return mf, fr
}

// TestPrecopyPrivateMemoryFile checks that a pre-copy copies the private
// MemoryFiles that the save saves, whose pages the save then writes only if
// they were dirtied during the last round; and that an incremental save
// writes the pages of a private MemoryFile that the pre-copy did not copy
// that were dirtied during its rounds.
func TestPrecopyPrivateMemoryFile(t *testing.T) {
	for _, test := range []struct {
		name        string
		incremental bool
		copied      bool
	}{
		{name: "full", copied: true},
		{name: "incremental", incremental: true, copied: true},
		{name: "incremental/not-copied", incremental: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			k, src, fr := dirtyTestKernel(t, ctx, true /* verify */)
			id := checkpoint.ResourceID{ContainerName: "test", Path: "/tmp"}
			pmf, pfr := privateTestMemoryFile(t, id)
			privates := map[checkpoint.ResourceID]*pgalloc.MemoryFile{id: pmf}
			dirs := make(map[checkpointimage.Digest]string)
			full, dir, err := saveImageOpts(t, ctx, k, testSaveOpts{privates: privates})
			if err != nil {
				t.Fatalf("full save: %v", err)
			}
			dirs[full.Digest] = dir
			var parent *checkpointimage.Digest
			if test.incremental {
				parent = &full.Digest
			}
			opts := testSaveOpts{
				parent:   parent,
				privates: privates,
				precopy:  &PrecopyOpts{Budget: 0, MaxRounds: 1},
			}
			if test.copied {
				opts.precopyPrivates = []*pgalloc.MemoryFile{pmf}
			}

			// Page 1 of the private MemoryFile is written before the
			// pre-copy's only round, page 2 during it, with a page of the
			// application MemoryFile.
			writePrivate := func(i uint64, b byte) {
				off := pfr.Start + i*hostarch.PageSize
				pwrite(t, pmf, off, b)
				pmf.MarkDirty(memmap.FileRange{off, off + hostarch.PageSize})
			}
			writePrivate(1, 0xe1)
			harvests := 0
			src.onHarvest = func() {
				harvests++
				if harvests == 2 {
					writePrivate(2, 0xe2)
					src.write(t, fr, 3, 0xa3)
				}
			}
			copiedBefore := precopyBytes.Value()
			img, dir, err := saveImageOpts(t, ctx, k, opts)
			src.onHarvest = nil
			if err != nil {
				t.Fatalf("save with pre-copy: %v", err)
			}
			dirs[img.Digest] = dir
			checkImagePages(t, img, 0, dirs, k.mf, fr)
			checkImagePages(t, img, 1, dirs, pmf, pfr)

			// The save wrote the pages written during the round, and those
			// of the private MemoryFile written before it if the pre-copy
			// did not copy it.
			want := uint64(2 * hostarch.PageSize)
			if !test.copied {
				want += hostarch.PageSize
			}
			if got := img.Layers()[0].PagesSize - (precopyBytes.Value() - copiedBefore); got != want {
				t.Errorf("the save wrote %d bytes, want %d", got, want)
			}
		})
	}
}

// TestPrecopyAuto checks that --precopy=auto pre-copies when nothing measured
// the cost of writing pages yet, and measures it in round 0; then skips the
// rounds when the save would write its pages within the budget at the
// measured cost, and runs them otherwise.
func TestPrecopyAuto(t *testing.T) {
	const writeRate = 16 << 20 // 62.5 ms per MiB
	ctx := contexttest.Context(t)
	k, _, _ := dirtyTestKernel(t, ctx, false /* verify */)
	// 2 MiB more of data, so that round 0 writes enough to measure the cost.
	fr, err := k.mf.Allocate(2<<20, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	t.Cleanup(func() { k.mf.DecRef(fr) })
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		pwrite(t, k.mf, off, 0xaa)
	}
	if k.pagesWriteCost != 0 {
		t.Fatalf("a cost of writing pages is known before any save measured it: %v", k.pagesWriteCost)
	}

	save := func(budget time.Duration, parent *checkpointimage.Digest) (*checkpointimage.Image, precopyStats) {
		t.Helper()
		var stats precopyStats
		img, _, err := saveImageOpts(t, ctx, k, testSaveOpts{
			parent:    parent,
			writeRate: writeRate,
			precopy:   &PrecopyOpts{Budget: budget, MaxRounds: 8, Auto: true},
			stats:     &stats,
		})
		if err != nil {
			t.Fatalf("save with --precopy=auto, budget %v: %v", budget, err)
		}
		return img, stats
	}
	skippedBefore := precopyStops.Value(precopyStopReasons[precopySkipped])
	_, stats := save(time.Hour, nil)
	if len(stats.roundBytes) == 0 {
		t.Errorf("the first save with --precopy=auto ran no round, want round 0 to measure the cost of writing pages")
	}
	if minCost := time.Second / (writeRate >> 20); k.pagesWriteCost < minCost {
		t.Errorf("round 0 measured %v per MiB, want at least %v at %d bytes/s", k.pagesWriteCost, minCost, writeRate)
	}
	if got := time.Duration(pagesWriteCostMetric.Value()); got != k.pagesWriteCost {
		t.Errorf("%s is %v, want %v", "/checkpoint/pages_write_cost", got, k.pagesWriteCost)
	}

	// Writing all of memory takes about 130 ms at the measured cost.
	if _, stats = save(time.Second, nil); stats.stop != precopySkipped || len(stats.roundBytes) != 0 {
		t.Errorf("save within the budget: %d rounds, stopped by %v; want none, skipped", len(stats.roundBytes), stats.stop)
	}
	if got := precopyStops.Value(precopyStopReasons[precopySkipped]) - skippedBefore; got != 1 {
		t.Errorf("%s{reason=skipped} counted %d pre-copies, want 1", "/checkpoint/precopy_stops", got)
	}
	img, stats := save(10*time.Millisecond, nil)
	if len(stats.roundBytes) == 0 {
		t.Errorf("save beyond the budget ran no round")
	}
	// An incremental save writes only the pages dirtied since its parent:
	// none.
	if _, stats = save(10*time.Millisecond, &img.Digest); stats.stop != precopySkipped {
		t.Errorf("incremental save of nothing: %d rounds, stopped by %v; want none, skipped", len(stats.roundBytes), stats.stop)
	}
}

// TestPrecopyThrottle checks that a round that does not halve the bytes left
// to write throttles dirtying, if the pre-copy may, instead of stopping the
// rounds; that the save lifts the limit; and that the longest delay of a task
// is the pre-copy's longest stall.
func TestPrecopyThrottle(t *testing.T) {
	for _, throttle := range []bool{false, true} {
		t.Run(fmt.Sprintf("throttle=%t", throttle), func(t *testing.T) {
			ctx := contexttest.Context(t)
			k, src, fr := dirtyTestKernel(t, ctx, false /* verify */)
			full, _, err := saveImage(t, ctx, k, nil, noLimit)
			if err != nil {
				t.Fatalf("full save: %v", err)
			}
			// Each harvest finds 3 pages written since the previous one, as
			// many as round 0 copies, so round 0 does not halve them. While
			// dirtying is limited, the limit delays a task by 50 ms.
			const delay = 50 * time.Millisecond
			var (
				next   uint64
				limits []uint64
			)
			src.onHarvest = func() {
				limit := k.dirtyLimit.Load()
				limits = append(limits, limit)
				if limit != 0 {
					k.recordThrottleDelay(delay)
				}
				for range 3 {
					src.write(t, fr, next%16, byte(next))
					next++
				}
			}
			var stats precopyStats
			if _, _, err := saveImageOpts(t, ctx, k, testSaveOpts{
				parent:  &full.Digest,
				precopy: &PrecopyOpts{Budget: 0, MaxRounds: 3, Throttle: throttle},
				stats:   &stats,
			}); err != nil {
				t.Fatalf("save with pre-copy: %v", err)
			}
			src.onHarvest = nil
			// Harvests: round 0's epoch, after round 0, then, if the rounds
			// go on, round 1's epoch, after round 1, round 2's epoch and
			// after round 2; and the save's epoch.
			wantHarvests := 3
			if throttle {
				wantHarvests = 7
			}
			if len(limits) != wantHarvests {
				t.Fatalf("sources harvested %d times, want %d", len(limits), wantHarvests)
			}
			if throttle && limits[2] == 0 {
				t.Errorf("dirtying was not limited in round 1")
			}
			if got := k.dirtyLimit.Load(); got != 0 {
				t.Errorf("dirtying limited to %d bytes/s after the save, want no limit", got)
			}
			if got := stats.longestStall >= delay; got != throttle {
				t.Errorf("longest stall %v with throttling %t, want the %v delay counted: %t", stats.longestStall, throttle, delay, throttle)
			}
		})
	}
}
