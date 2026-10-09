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
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
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
		stop      string
	}{
		{rateMiB: 1, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 1, stop: "converged"},
		{rateMiB: 10, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 2, stop: "converged"},
		{rateMiB: 50, budget: 30 * time.Millisecond, maxRounds: 8, rounds: 6, stop: "converged"},
		{rateMiB: 50, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 5, stop: "converged"},
		{rateMiB: 50, budget: 300 * time.Millisecond, maxRounds: 8, rounds: 3, stop: "converged"},
		{rateMiB: 50, budget: 30 * time.Millisecond, maxRounds: 4, rounds: 4, stop: "round cap reached"},
		{rateMiB: 50, budget: 100 * time.Millisecond, maxRounds: 8, throttle: true, rounds: 5, stop: "converged"},
		{rateMiB: 200, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 1, stop: "pending bytes not halved"},
		{rateMiB: 200, budget: 100 * time.Millisecond, maxRounds: 8, throttle: true, rounds: 4, stop: "converged"},
		{rateMiB: 400, budget: 100 * time.Millisecond, maxRounds: 8, throttle: true, rounds: 4, stop: "converged"},
	} {
		opts := PrecopyOpts{Budget: test.budget, MaxRounds: test.maxRounds, Throttle: test.throttle}
		pending := float64(imageMiB)
		rounds, stop, throttling := 0, "", false
		for round := 0; stop == ""; round++ {
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
			t.Errorf("%v MiB/s, budget %v, at most %d rounds, throttle %t: %d rounds, stopped by %q; want %d rounds, stopped by %q", test.rateMiB, test.budget, test.maxRounds, test.throttle, rounds, stop, test.rounds, test.stop)
		}
	}
}

// imagePage returns the contents of the page at off in img, reading its
// layers from the image directories dirs, by digest.
func imagePage(t *testing.T, img *checkpointimage.Image, dirs map[checkpointimage.Digest]string, off uint64) []byte {
	t.Helper()
	e, ok := img.MemoryFileImage(0).ExtentAt(off)
	if !ok {
		return make([]byte, hostarch.PageSize)
	}
	d := img.Digest
	if e.Layer != 0 {
		d = img.Layers()[e.Layer].Digest
	}
	f, err := os.Open(filepath.Join(dirs[d], "pages.img"))
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

// TestPrecopy checks a save that completes a pre-copy of 2 rounds, with
// writes while they run: the save writes only the pages dirtied during the
// last round, its image holds the MemoryFile's contents, verification finds
// no escape, and the image is the parent of the next incremental save.
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
			copiedBefore := precopyBytes.Value()
			img, dir, err := saveImageOpts(t, ctx, k, testSaveOpts{
				parent:  parent,
				precopy: &PrecopyOpts{Budget: 0, MaxRounds: 2},
			})
			if err != nil {
				t.Fatalf("save with pre-copy: %v", err)
			}
			src.onHarvest = nil
			dirs[img.Digest] = dir
			if harvests != 5 {
				t.Errorf("sources harvested %d times, want 5", harvests)
			}
			for off := fr.Start; off < fr.End; off += hostarch.PageSize {
				want := make([]byte, hostarch.PageSize)
				if _, err := unix.Pread(k.mf.FD(), want, int64(off)); err != nil {
					t.Fatalf("Pread: %v", err)
				}
				if got := imagePage(t, img, dirs, off); !bytes.Equal(got, want) {
					t.Errorf("page %d: image has %x..., MemoryFile has %x...", (off-fr.Start)/hostarch.PageSize, got[:8], want[:8])
				}
			}

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
		})
	}
}

// TestPrecopyFailureKeepsDirtyPages checks that a pre-copy that fails returns
// the pages its rounds took from the dirty sets, so that the next save writes
// them.
func TestPrecopyFailureKeepsDirtyPages(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, false /* verify */)
	full, err := saveImage(t, ctx, k, nil, false)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}
	src.write(t, fr, 4, 0xaa)
	if _, _, err := saveImageOpts(t, ctx, k, testSaveOpts{
		parent:    &full.Digest,
		failPages: true,
		precopy:   &PrecopyOpts{Budget: time.Second, MaxRounds: 8},
	}); err == nil {
		t.Fatalf("pre-copy to an unwritable pages file succeeded")
	}
	src.write(t, fr, 6, 0xbb)
	delta, err := saveImage(t, ctx, k, &full.Digest, false)
	if err != nil {
		t.Fatalf("incremental save after a failed pre-copy: %v", err)
	}
	want := slices.Repeat([]checkpointimage.Digest{full.Digest}, 16)
	want[4], want[6] = delta.Digest, delta.Digest
	if got := pageImages(t, delta, fr); !slices.Equal(got, want) {
		t.Errorf("images of the delta's pages: got %v, want %v", got, want)
	}
}

// TestPrecopyThrottle checks that a round that does not halve the bytes left
// to write throttles dirtying, if the pre-copy may, instead of stopping the
// rounds, and that the save lifts the limit.
func TestPrecopyThrottle(t *testing.T) {
	for _, throttle := range []bool{false, true} {
		t.Run(fmt.Sprintf("throttle=%t", throttle), func(t *testing.T) {
			ctx := contexttest.Context(t)
			k, src, fr := dirtyTestKernel(t, ctx, false /* verify */)
			full, err := saveImage(t, ctx, k, nil, false)
			if err != nil {
				t.Fatalf("full save: %v", err)
			}
			// Each harvest finds 3 pages written since the previous one, as
			// many as round 0 copies, so round 0 does not halve them.
			var (
				next   uint64
				limits []uint64
			)
			src.onHarvest = func() {
				limits = append(limits, k.dirtyLimit.Load())
				for range 3 {
					src.write(t, fr, next%16, byte(next))
					next++
				}
			}
			if _, _, err := saveImageOpts(t, ctx, k, testSaveOpts{
				parent:  &full.Digest,
				precopy: &PrecopyOpts{Budget: 0, MaxRounds: 3, Throttle: throttle},
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
		})
	}
}

// TestPrecopyThrottleUffd checks that a pre-copy that may throttle dirtying is
// refused when writes are tracked with userfaultfd, which never delays the
// tasks that write, rather than run unthrottled.
func TestPrecopyThrottleUffd(t *testing.T) {
	ctx := contexttest.Context(t)
	k, _, _ := dirtyTestKernel(t, ctx, false /* verify */)
	full, err := saveImage(t, ctx, k, nil, false)
	if err != nil {
		t.Fatalf("full save: %v", err)
	}
	k.dirty.Sources = []DirtySource{&uffdDirtySource{k: k}}
	_, _, err = saveImageOpts(t, ctx, k, testSaveOpts{
		parent:  &full.Digest,
		precopy: &PrecopyOpts{Budget: 0, MaxRounds: 3, Throttle: true},
	})
	if err == nil || !strings.Contains(err.Error(), "--dirty-tracking=wp") {
		t.Errorf("save with throttled pre-copy under uffd tracking: got error %v, want one asking for --dirty-tracking=wp", err)
	}
}
