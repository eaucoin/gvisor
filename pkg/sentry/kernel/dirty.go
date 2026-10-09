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
	"fmt"
	"slices"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/metric"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
)

// Dirty tracking.
//
// With dirty tracking, the Kernel records which pages of its savable
// MemoryFiles are written between saves, so that a save can write only the
// pages changed since the image the Kernel was last saved to or restored from
// (pgalloc's dirty.go describes the MemoryFile side). Tracking starts at the
// end of every successful save and at every restore, and is divided into
// epochs: DirtyEpoch ends one, returning the pages dirtied during it, and
// starts the next.
//
// MemoryFiles see the writes of the Sentry. Application stores through the
// platform's mappings of MemoryFiles are reported by dirty sources.

// A DirtySource moves writes that MemoryFile marks cannot see (application
// stores through page tables) into MemoryFile dirty sets.
type DirtySource interface {
	// Name returns the source's name, for logging.
	Name() string

	// Arm makes the next write to every tracked page observable. If paused
	// is true, the kernel is paused, and quiesced, for the duration of the
	// call; otherwise tasks may run, and Arm must not lose writes racing with
	// it.
	Arm(ctx context.Context, paused bool) error

	// Harvest moves the writes observed since the last call to Arm or
	// Harvest into the MemoryFiles' dirty sets, with
	// pgalloc.MemoryFile.MarkDirty.
	Harvest(ctx context.Context) error
}

// DirtyTrackingOpts configures dirty tracking.
type DirtyTrackingOpts struct {
	// Sources report application writes. Dirty tracking is enabled if
	// Sources is not empty.
	Sources []DirtySource

	// If Verify is true, every save checks that every page whose contents
	// changed since the previous save or restore was reported dirty, and
	// fails if one was not ("escaped"). Verification hashes every page at the
	// end of every save and restore, and restores wait for every page to be
	// loaded; it is meant for tests and debugging.
	Verify bool
}

// dirtyTracking is the dirty tracking state of a Kernel.
type dirtyTracking struct {
	DirtyTrackingOpts

	// mfs are the tracked MemoryFiles, the application MemoryFile first.
	// mfs is only mutated with the Kernel paused, during saves and
	// restores.
	mfs []*pgalloc.MemoryFile

	// last is the image that the Kernel was last saved to or restored from,
	// if it has a pages file: the parent of the next incremental save. last
	// is only mutated with the Kernel paused, during saves and restores.
	last *checkpointimage.Image
}

var dirtyEscapes = metric.MustCreateNewUint64Metric("/checkpoint/dirty_tracking_escapes", metric.Uint64Metadata{
	Cumulative:  true,
	Description: "Pages found changed without being reported dirty by dirty tracking verification.",
})

// ErrDirtyTrackingEscapes fails a save whose verification found pages changed
// without being reported dirty.
var ErrDirtyTrackingEscapes = errors.New("pages changed without being reported dirty")

// SetDirtyTracking configures dirty tracking. It must be called before the
// Kernel is started or restored.
func (k *Kernel) SetDirtyTracking(opts DirtyTrackingOpts) {
	k.dirty.DirtyTrackingOpts = opts
}

// DirtyTrackingEnabled returns true if dirty tracking is enabled.
func (k *Kernel) DirtyTrackingEnabled() bool {
	return len(k.dirty.Sources) != 0
}

// LastImageDigest returns the digest of the image that k was last saved to or
// restored from, which an incremental save may have as its parent, and true;
// or false if there is none.
func (k *Kernel) LastImageDigest() (checkpointimage.Digest, bool) {
	if k.dirty.last == nil {
		return checkpointimage.Digest{}, false
	}
	return k.dirty.last.Digest, true
}

// DirtyEpochResult holds the pages dirtied during a dirty tracking epoch.
type DirtyEpochResult struct {
	// Sets holds the pages dirtied during the epoch, for each tracked
	// MemoryFile.
	Sets map[*pgalloc.MemoryFile]*pgalloc.DirtySet
}

// Abort returns the pages in e to their MemoryFiles' dirty sets, so that the
// next epoch reports them again. It is called when the save that consumed e
// failed.
func (e *DirtyEpochResult) Abort() {
	for mf, s := range e.Sets {
		mf.UnswapDirty(s)
	}
}

// DirtyEpoch ends the current dirty tracking epoch, returning the pages
// dirtied during it, and starts the next: it harvests every source, swaps the
// dirty set of every tracked MemoryFile, and re-arms every source.
//
// If paused is false, tasks may run during and after DirtyEpoch. Pages written
// between the swap and the re-arming are in the returned sets (they were
// writable, hence already dirtied during the epoch that ended), so callers that
// read the returned pages after DirtyEpoch returns read their latest
// contents.
//
// Preconditions: Dirty tracking must have started: k was saved or restored
// with dirty tracking enabled.
func (k *Kernel) DirtyEpoch(ctx context.Context, paused bool) (*DirtyEpochResult, error) {
	e, err := k.swapDirty(ctx, paused)
	if err != nil {
		return nil, err
	}
	if err := k.armDirtySources(ctx, paused); err != nil {
		e.Abort()
		return nil, err
	}
	return e, nil
}

// swapDirty harvests every source and swaps the dirty set of every tracked
// MemoryFile.
func (k *Kernel) swapDirty(ctx context.Context, paused bool) (*DirtyEpochResult, error) {
	if len(k.dirty.mfs) == 0 {
		panic("Kernel.DirtyEpoch called before dirty tracking started")
	}
	for _, s := range k.dirty.Sources {
		if err := s.Harvest(ctx); err != nil {
			return nil, fmt.Errorf("dirty source %s: harvesting: %w", s.Name(), err)
		}
	}
	e := &DirtyEpochResult{Sets: make(map[*pgalloc.MemoryFile]*pgalloc.DirtySet, len(k.dirty.mfs))}
	for _, mf := range k.dirty.mfs {
		e.Sets[mf] = mf.SwapDirty(paused)
	}
	return e, nil
}

func (k *Kernel) armDirtySources(ctx context.Context, paused bool) error {
	for _, s := range k.dirty.Sources {
		if err := s.Arm(ctx, paused); err != nil {
			return fmt.Errorf("dirty source %s: arming: %w", s.Name(), err)
		}
	}
	return nil
}

// trackDirty makes mfs, the application MemoryFile and the private
// MemoryFiles that the Kernel was just saved with or restored from, the
// tracked MemoryFiles, and arms every source. MemoryFiles that are already
// tracked stay tracked; new ones are tracked from now on; MemoryFiles no
// longer saved are no longer swapped.
//
// Preconditions:
//   - Dirty tracking is enabled.
//   - The Kernel is paused.
//   - Every allocated page of mfs is either known-committed or zero, as
//     after saving or loading them.
func (k *Kernel) trackDirty(ctx context.Context, mfs []*pgalloc.MemoryFile) error {
	for _, mf := range mfs {
		if slices.Contains(k.dirty.mfs, mf) {
			continue
		}
		mf.EnableDirtyTracking()
		if k.dirty.Verify {
			if err := mf.RecordPageHashes(); err != nil {
				return fmt.Errorf("recording page hashes of MemoryFile %p: %w", mf, err)
			}
		}
	}
	k.dirty.mfs = mfs
	return k.armDirtySources(ctx, true /* paused */)
}

// beginDirtySave ends the dirty tracking epoch at the start of a save, if
// tracking has started, and returns its dirty sets, which the save consumes.
// endDirtySave must follow.
//
// Preconditions: The Kernel is paused.
func (k *Kernel) beginDirtySave(ctx context.Context) (*DirtyEpochResult, error) {
	if len(k.dirty.mfs) == 0 {
		return nil, nil
	}
	return k.swapDirty(ctx, true /* paused */)
}

// endDirtySave ends a save that began with beginDirtySave, which returned e.
// If the save succeeded (err is nil), saved holds the MemoryFiles it saved and
// image is the image it wrote, if it has a pages file: with verification,
// every page of saved that changed during e's epoch must be in e, else
// endDirtySave fails the save; then tracking continues with saved, and image
// becomes the parent of the next incremental save. If the save failed, e's
// pages are returned to the dirty sets, so that the next save writes them,
// and the parent of the next incremental save is unchanged. endDirtySave
// returns err, or the error that fails the save.
//
// Preconditions: The Kernel is paused.
func (k *Kernel) endDirtySave(ctx context.Context, e *DirtyEpochResult, saved []*pgalloc.MemoryFile, image *checkpointimage.Image, err error) error {
	if err == nil && e != nil && k.dirty.Verify {
		err = verifyDirty(e, saved)
	}
	if err != nil {
		if e != nil {
			e.Abort()
		}
		if armErr := k.armDirtySources(ctx, true /* paused */); armErr != nil {
			log.Warningf("Failed to re-arm dirty tracking after a failed save: %v", armErr)
		}
		return err
	}
	k.dirty.last = image
	return k.trackDirty(ctx, saved)
}

// verifyDirty checks that every page of the MemoryFiles in saved that are in
// e, and that changed during the epoch e ended, is in e; then it starts the
// next verification epoch. It returns an error if a page escaped.
//
// Preconditions: Every allocated page of saved is either known-committed or
// zero, as after saving them.
func verifyDirty(e *DirtyEpochResult, saved []*pgalloc.MemoryFile) error {
	var (
		escapes uint64
		vs      []*pgalloc.DirtyVerification
	)
	for _, mf := range saved {
		s, ok := e.Sets[mf]
		if !ok {
			// mf was not tracked during the epoch.
			continue
		}
		v, err := mf.VerifyDirty(s)
		if err != nil {
			return fmt.Errorf("dirty tracking verification: MemoryFile %p: %w", mf, err)
		}
		if v.Escapes != 0 {
			log.Warningf("Dirty tracking verification: MemoryFile %p: %d pages changed without being dirty, starting with %v", mf, v.Escapes, v.First)
			escapes += v.Escapes
		}
		vs = append(vs, v)
	}
	if escapes != 0 {
		dirtyEscapes.IncrementBy(escapes)
		return fmt.Errorf("dirty tracking verification: %d %w", escapes, ErrDirtyTrackingEscapes)
	}
	for _, v := range vs {
		v.Commit()
	}
	return nil
}
