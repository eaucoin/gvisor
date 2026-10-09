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
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
)

// TestPrecopyStopRule drives precopyStop with experiment 06's model of
// pre-copy (sim06.py): a writer dirties rate MiB/s of random pages of a
// 256 MiB image, and a round writes the pages dirtied during the previous one
// at 100 MiB/s, which leaves N(1 - exp(-rate t / N)) MiB dirty after t
// seconds. The rounds are the model's, which predicted 06's measurements
// within one round, and the 200 MiB/s writer stops by the halving rule
// rather than after 8 rounds that write 7.3 times the image.
func TestPrecopyStopRule(t *testing.T) {
	const (
		imageMiB = 256
		bwMiB    = 100
		cost     = time.Second / bwMiB
	)
	for _, test := range []struct {
		rateMiB   float64
		budget    time.Duration
		maxRounds int
		rounds    int
		stop      string
	}{
		{rateMiB: 1, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 1, stop: "converged"},
		{rateMiB: 10, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 2, stop: "converged"},
		{rateMiB: 50, budget: 30 * time.Millisecond, maxRounds: 8, rounds: 6, stop: "converged"},
		{rateMiB: 50, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 5, stop: "converged"},
		{rateMiB: 50, budget: 300 * time.Millisecond, maxRounds: 8, rounds: 3, stop: "converged"},
		{rateMiB: 50, budget: 30 * time.Millisecond, maxRounds: 4, rounds: 4, stop: "round cap reached"},
		{rateMiB: 200, budget: 100 * time.Millisecond, maxRounds: 8, rounds: 1, stop: "pending bytes not halved"},
	} {
		opts := PrecopyOpts{Budget: test.budget, MaxRounds: test.maxRounds}
		pending := float64(imageMiB)
		rounds, stop := 0, ""
		for round := 0; stop == ""; round++ {
			written := pending
			pending = imageMiB * (1 - math.Exp(-test.rateMiB*(written/bwMiB)/imageMiB))
			rounds++
			stop = precopyStop(round, uint64(written*(1<<20)), uint64(pending*(1<<20)), cost, opts)
		}
		if rounds != test.rounds || stop != test.stop {
			t.Errorf("%v MiB/s, budget %v, at most %d rounds: %d rounds, stopped by %q; want %d rounds, stopped by %q", test.rateMiB, test.budget, test.maxRounds, rounds, stop, test.rounds, test.stop)
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
