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
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/metric"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

// Pre-copy.
//
// A pre-copy writes the application memory of a save while the Kernel runs,
// as VM live migration does: in rounds, each of which writes the pages
// dirtied during the previous one (the first: every page, or the pages
// dirtied since the parent of an incremental save), until QEMU's stop rule
// holds; then the save, in its pause, writes the object graph and the pages
// dirtied during the last round, and refers to the copies for the others
// (pgalloc's precopy.go). The pause then lasts about the stop rule's budget
// plus the object graph's time, rather than the time to write all of memory.

// PrecopyOpts configures a pre-copy.
type PrecopyOpts struct {
	// Budget is the stop rule's target: rounds stop when the pages dirtied
	// during the last one would take at most Budget to write, at the cost
	// per byte measured during that round.
	Budget time.Duration

	// MaxRounds is the maximum number of rounds.
	MaxRounds int

	// If Auto is true, the pre-copy is skipped when the previous save's
	// write cost says that the save would write its pages within Budget
	// anyway.
	Auto bool
}

// Precopy is a pre-copy that a save completes; see Kernel.Precopy.
type Precopy struct {
	// apfs writes the pages file.
	apfs *pgalloc.AsyncPagesFileSave

	// apfsDone is closed when apfs completes, with error apfsErr.
	apfsDone chan struct{}
	apfsErr  error

	// mf is the pre-copy of the application MemoryFile, or nil if the
	// pre-copy was skipped.
	mf *pgalloc.Precopy

	// rounds hold the pages dirtied during each round but the last, which
	// the rounds copied: the save's dirty tracking epoch includes them.
	rounds []*DirtyEpochResult
}

// precopyWriteCost is the time per MiB of writing pages assumed by the stop
// rule when a round writes too little to measure it: the cost of O_DIRECT
// writes to a local disk measured by experiment 05.
const precopyWriteCost = 340 * time.Microsecond

// writeTime returns the time to write n bytes at cost per MiB.
func writeTime(n uint64, cost time.Duration) time.Duration {
	return time.Duration(n) * cost / (1 << 20)
}

var (
	precopyRounds = metric.MustCreateNewUint64Metric("/checkpoint/precopy_rounds", metric.Uint64Metadata{
		Cumulative:  true,
		Description: "Pre-copy rounds run by saves.",
	})
	precopyBytes = metric.MustCreateNewUint64Metric("/checkpoint/precopy_bytes", metric.Uint64Metadata{
		Cumulative:  true,
		Description: "Bytes of memory written by pre-copy rounds.",
	})
)

// Precopy starts a save of k to pagesFile, in rounds that run while k runs,
// and returns it for SaveTo to complete. parent is the parent of the save if
// it is incremental (see SaveTo). Precopy takes ownership of pagesFile, even
// if it returns a non-nil error.
//
// Preconditions: Dirty tracking is enabled. k is running.
func (k *Kernel) Precopy(ctx context.Context, pagesFile stateio.AsyncWriter, parent *checkpointimage.Digest, opts PrecopyOpts) (*Precopy, error) {
	p := &Precopy{apfsDone: make(chan struct{})}
	apfs, err := pgalloc.StartAsyncPagesFileSave(pagesFile, func(err error) {
		p.apfsErr = err
		close(p.apfsDone)
	}) // transfers ownership
	if err != nil {
		return nil, fmt.Errorf("failed to start async pages file saving: %w", err)
	}
	p.apfs = apfs
	if err := k.precopyRounds(ctx, p, parent, opts); err != nil {
		p.Release()
		return nil, err
	}
	return p, nil
}

// Release releases p. If no save consumed p, the pages that its rounds copied
// are returned to the dirty sets, so that the next save writes them. SaveTo
// releases the Precopy it is given.
func (p *Precopy) Release() {
	for _, e := range p.rounds {
		e.Abort()
	}
	p.rounds = nil
	p.finishWriting()
}

// finishWriting completes the pages file and returns the error that
// terminated writing it, if any.
func (p *Precopy) finishWriting() error {
	p.apfs.MemoryFilesDone()
	<-p.apfsDone
	return p.apfsErr
}

func (k *Kernel) precopyRounds(ctx context.Context, p *Precopy, parent *checkpointimage.Digest, opts PrecopyOpts) error {
	if !k.DirtyTrackingEnabled() {
		return fmt.Errorf("pre-copy requires dirty tracking")
	}
	if parent != nil {
		if last, ok := k.LastImageDigest(); !ok || last != *parent {
			return fmt.Errorf("incremental save: image %v is not the image the sandbox was last saved to or restored from", *parent)
		}
	}
	if len(k.dirty.mfs) == 0 {
		// Tracking starts at the first save or restore; this is the first
		// save, and its rounds need tracking now.
		if err := k.startDirtyTrackingRunning(ctx); err != nil {
			return err
		}
	}
	if opts.Auto && k.precopyNeedless(ctx, parent != nil, opts.Budget) {
		return nil
	}

	// Track first writes in single pages during rounds: with larger units,
	// random writes dirty every unit of a large image within a round, and
	// rounds never converge (experiment 06).
	for _, s := range k.dirty.Sources {
		if us, ok := s.(*WriteProtectDirtySource); ok {
			defer us.SetUnit(us.unit)
			us.SetUnit(hostarch.PageSize)
		}
	}

	mf, err := k.mf.StartPrecopy(p.apfs)
	if err != nil {
		return err
	}
	p.mf = mf
	start := time.Now()
	var copied uint64
	for round := 0; ; round++ {
		e, err := k.DirtyEpoch(ctx, false /* paused */)
		if err != nil {
			return err
		}
		p.rounds = append(p.rounds, e)
		roundStart := time.Now()
		var n uint64
		if round == 0 && parent == nil {
			if n, err = mf.CopyAll(); err != nil {
				return err
			}
		} else {
			n = mf.Copy(e.Sets[k.mf])
		}
		if err := mf.Wait(); err != nil {
			return fmt.Errorf("pre-copy round %d: %w", round, err)
		}
		took := time.Since(roundStart)
		copied += n
		precopyRounds.Increment()
		precopyBytes.IncrementBy(n)

		cost := k.pagesWriteCost
		if cost == 0 {
			cost = precopyWriteCost
		}
		if n >= 1<<20 {
			cost = took * (1 << 20) / time.Duration(n)
			k.pagesWriteCost = cost
		}
		pending, err := k.pendingDirtyBytes(ctx)
		if err != nil {
			return err
		}
		log.Infof("Pre-copy round %d: wrote %d bytes in %v; %d bytes dirty since, %v to write (budget %v)", round, n, took, pending, writeTime(pending, cost), opts.Budget)
		if stop := precopyStop(round, n, pending, cost, opts); stop != "" {
			log.Infof("Pre-copy done after %d rounds (%s): %d bytes in %v", round+1, stop, copied, time.Since(start))
			return nil
		}
	}
}

// precopyStop returns why pre-copy rounds stop after round, which wrote
// written bytes and left pending bytes dirty, at cost per MiB; or "" if
// another round runs. It is QEMU's stop rule, with its round cap, and an
// early stop when a round does not halve the bytes left to write.
func precopyStop(round int, written, pending uint64, cost time.Duration, opts PrecopyOpts) string {
	switch {
	case writeTime(pending, cost) <= opts.Budget:
		return "converged"
	case round+1 >= opts.MaxRounds:
		return "round cap reached"
	case 2*pending > written:
		// The dirty rate is at least half the write bandwidth: more rounds
		// would mostly write the same pages again.
		return "pending bytes not halved"
	default:
		return ""
	}
}

// startDirtyTrackingRunning starts dirty tracking of the application
// MemoryFile while k runs.
func (k *Kernel) startDirtyTrackingRunning(ctx context.Context) error {
	k.Pause()
	defer k.Unpause()
	k.mf.EnableDirtyTracking()
	if k.dirty.Verify {
		if err := k.mf.RecordPageHashes(); err != nil {
			return fmt.Errorf("recording page hashes: %w", err)
		}
	}
	k.dirty.mfs = []*pgalloc.MemoryFile{k.mf}
	return k.armDirtySources(ctx, true /* paused */)
}

// precopyNeedless returns true if the previous save's write cost says that a
// save would write the application MemoryFile's pages (those dirtied since
// the parent if incremental) within budget.
func (k *Kernel) precopyNeedless(ctx context.Context, incremental bool, budget time.Duration) bool {
	if k.pagesWriteCost == 0 {
		return false
	}
	var bytes uint64
	if incremental {
		var err error
		if bytes, err = k.pendingDirtyBytes(ctx); err != nil {
			return false
		}
	} else {
		usage, err := k.mf.TotalUsage()
		if err != nil {
			return false
		}
		bytes = usage
	}
	estimate := writeTime(bytes, k.pagesWriteCost)
	if estimate > budget {
		return false
	}
	log.Infof("Pre-copy skipped: %d bytes to write, %v at the previous save's cost (budget %v)", bytes, estimate, budget)
	return true
}

// pendingDirtyBytes returns the number of bytes of the application
// MemoryFile dirtied since the last dirty tracking epoch.
func (k *Kernel) pendingDirtyBytes(ctx context.Context) (uint64, error) {
	for _, s := range k.dirty.Sources {
		if err := s.Harvest(ctx); err != nil {
			return 0, fmt.Errorf("dirty source %s: harvesting: %w", s.Name(), err)
		}
	}
	return k.mf.DirtyBytes(), nil
}

// epochSince returns the pages dirtied since p started: those of e, the dirty
// tracking epoch of the save that completes p, dirtied during p's last round,
// and those that p's rounds copied. The result owns p's rounds.
func (p *Precopy) epochSince(e *DirtyEpochResult) *DirtyEpochResult {
	all := &DirtyEpochResult{Sets: make(map[*pgalloc.MemoryFile]*pgalloc.DirtySet, len(e.Sets))}
	for mf, s := range e.Sets {
		u := &pgalloc.DirtySet{}
		u.Union(s)
		for _, r := range p.rounds {
			if rs, ok := r.Sets[mf]; ok {
				u.Union(rs)
			}
		}
		all.Sets[mf] = u
	}
	p.rounds = nil
	return all
}
