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
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// testDirtySource is a DirtySource that reports the writes it is told about,
// and records the calls made to it.
type testDirtySource struct {
	mf *pgalloc.MemoryFile

	// writes are reported by the next Harvest.
	writes []memmap.FileRange

	// calls records "harvest" and "arm" in call order.
	calls []string

	// armErr is returned by Arm.
	armErr error
}

func (s *testDirtySource) Name() string { return "test" }

func (s *testDirtySource) Arm(context.Context, bool) error {
	s.calls = append(s.calls, "arm")
	return s.armErr
}

func (s *testDirtySource) Harvest(context.Context) error {
	s.calls = append(s.calls, "harvest")
	for _, fr := range s.writes {
		s.mf.MarkDirty(fr)
	}
	s.writes = nil
	return nil
}

// dirtyTestKernel returns a Kernel with dirty tracking enabled with one
// source and started on the MemoryFile of ctx, which holds one allocation of
// 16 pages written with data and saved.
func dirtyTestKernel(t *testing.T, ctx context.Context, verify bool) (*Kernel, *testDirtySource, memmap.FileRange) {
	t.Helper()
	mf := pgalloc.MemoryFileFromContext(ctx)
	fr, err := mf.Allocate(16*hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	t.Cleanup(func() { mf.DecRef(fr) })
	for i := uint64(0); i < 16; i++ {
		pwrite(t, mf, fr.Start+i*hostarch.PageSize, byte(i+1))
	}
	k := &Kernel{mf: mf}
	src := &testDirtySource{mf: mf}
	k.SetDirtyTracking(DirtyTrackingOpts{Sources: []DirtySource{src}, Verify: verify})
	save(t, ctx, k, nil)
	return k, src, fr
}

func pwrite(t *testing.T, mf *pgalloc.MemoryFile, off uint64, b byte) {
	t.Helper()
	if _, err := unix.Pwrite(mf.FD(), []byte{b}, int64(off)); err != nil {
		t.Fatalf("Pwrite: %v", err)
	}
}

// save simulates a save of k's MemoryFile, which fails with saveErr if it is
// not nil, and returns the dirty sets it consumed and its error.
func save(t *testing.T, ctx context.Context, k *Kernel, saveErr error) (*DirtyEpochResult, error) {
	t.Helper()
	e, err := k.beginDirtySave(ctx)
	if err != nil {
		t.Fatalf("beginDirtySave: %v", err)
	}
	if saveErr == nil {
		if err := k.mf.SaveTo(ctx, io.Discard, &pgalloc.SaveOpts{}); err != nil {
			t.Fatalf("SaveTo: %v", err)
		}
	}
	return e, k.endDirtySave(ctx, e, []*pgalloc.MemoryFile{k.mf}, nil /* image */, saveErr)
}

// pages returns the pages of fr in s, as indices into fr.
func pages(s *pgalloc.DirtySet, fr memmap.FileRange) []uint64 {
	var ps []uint64
	for off := fr.Start; off < fr.End; off += hostarch.PageSize {
		if s.Contains(off) {
			ps = append(ps, (off-fr.Start)/hostarch.PageSize)
		}
	}
	return ps
}

func TestDirtyEpoch(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, false)
	src.calls = nil

	// Sources are harvested before the dirty sets are swapped, so their
	// writes are in the epoch that ends, and armed after.
	src.writes = []memmap.FileRange{{fr.Start + 2*hostarch.PageSize, fr.Start + 4*hostarch.PageSize}}
	k.mf.MarkDirty(memmap.FileRange{fr.Start + 9*hostarch.PageSize, fr.Start + 10*hostarch.PageSize})
	e, err := k.DirtyEpoch(ctx, false /* paused */)
	if err != nil {
		t.Fatalf("DirtyEpoch: %v", err)
	}
	if got, want := pages(e.Sets[k.mf], fr), []uint64{2, 3, 9}; !slices.Equal(got, want) {
		t.Errorf("dirty pages: got %v, want %v", got, want)
	}
	if want := []string{"harvest", "arm"}; !slices.Equal(src.calls, want) {
		t.Errorf("source calls: got %v, want %v", src.calls, want)
	}

	// A failed arming returns the pages to the dirty sets.
	src.armErr = errors.New("arming failed")
	k.mf.MarkDirty(memmap.FileRange{fr.Start, fr.Start + hostarch.PageSize})
	if _, err := k.DirtyEpoch(ctx, false /* paused */); err == nil {
		t.Fatalf("DirtyEpoch succeeded with a failing source")
	}
	src.armErr = nil
	e, err = k.DirtyEpoch(ctx, false /* paused */)
	if err != nil {
		t.Fatalf("DirtyEpoch: %v", err)
	}
	if got, want := pages(e.Sets[k.mf], fr), []uint64{0}; !slices.Equal(got, want) {
		t.Errorf("dirty pages after a failed epoch: got %v, want %v", got, want)
	}
}

// TestDirtySaveFailureKeepsDirtyPages checks Firecracker's rule: a failed
// save loses no dirty page.
func TestDirtySaveFailureKeepsDirtyPages(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, false)
	src.writes = []memmap.FileRange{{fr.Start + 5*hostarch.PageSize, fr.Start + 6*hostarch.PageSize}}
	saveErr := errors.New("disk full")
	if _, err := save(t, ctx, k, saveErr); err != saveErr {
		t.Fatalf("failed save returned %v, want %v", err, saveErr)
	}
	src.writes = []memmap.FileRange{{fr.Start + 7*hostarch.PageSize, fr.Start + 8*hostarch.PageSize}}
	e, err := save(t, ctx, k, nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, want := pages(e.Sets[k.mf], fr), []uint64{5, 7}; !slices.Equal(got, want) {
		t.Errorf("dirty pages of the save after a failed save: got %v, want %v", got, want)
	}
}

func TestDirtyVerification(t *testing.T) {
	ctx := contexttest.Context(t)
	k, src, fr := dirtyTestKernel(t, ctx, true)

	// Writes reported by a source are not escapes.
	pwrite(t, k.mf, fr.Start+3*hostarch.PageSize, 0xff)
	src.writes = []memmap.FileRange{{fr.Start + 3*hostarch.PageSize, fr.Start + 4*hostarch.PageSize}}
	if _, err := save(t, ctx, k, nil); err != nil {
		t.Fatalf("save with every write reported: %v", err)
	}

	// A write no source reports fails the save, and is counted.
	escapes := dirtyEscapes.Value()
	pwrite(t, k.mf, fr.Start+6*hostarch.PageSize, 0xff)
	_, err := save(t, ctx, k, nil)
	if err == nil || !strings.Contains(err.Error(), "1 pages changed without being reported dirty") {
		t.Fatalf("save with an unreported write: got %v, want an escape", err)
	}
	if got := dirtyEscapes.Value() - escapes; got != 1 {
		t.Errorf("escapes metric increased by %d, want 1", got)
	}

	// The failed save left the epoch open: the escape is still one.
	if _, err := save(t, ctx, k, nil); err == nil {
		t.Fatalf("save after a failed verification succeeded")
	}
}
