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

	// If Throttle is true, a round that does not halve the bytes left to
	// write does not stop the rounds the first time: instead, each
	// MemoryManager's tasks may then dirty memory at most at a quarter of the
	// write bandwidth measured, until the save completes, as QEMU's
	// dirty-limit does for vCPUs. Only the tasks that dirty memory faster are
	// delayed, after the faults that record their first writes, so
	// throttling requires the write-protection dirty source.
	Throttle bool
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

	// k is the Kernel being saved.
	k *Kernel
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
	precopyThrottled = metric.MustCreateNewUint64Metric("/checkpoint/precopy_throttled_ns", metric.Uint64Metadata{
		Cumulative:  true,
		Description: "Nanoseconds that tasks were delayed to limit their dirtying of memory during pre-copies.",
	})
)

// Precopy starts a save of k to pagesFile, in rounds that run while k runs,
// and returns it for SaveTo to complete. parent is the parent of the save if
// it is incremental (see SaveTo). Precopy takes ownership of pagesFile, even
// if it returns a non-nil error.
//
// Preconditions: Dirty tracking is enabled. k is running.
func (k *Kernel) Precopy(ctx context.Context, pagesFile stateio.AsyncWriter, parent *checkpointimage.Digest, opts PrecopyOpts) (*Precopy, error) {
	p := &Precopy{apfsDone: make(chan struct{}), k: k}
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
	p.k.setDirtyLimit(0)
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
	var (
		copied     uint64
		throttling bool
	)
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
		stop, throttle := precopyNext(round, n, pending, cost, opts, throttling)
		if throttle {
			throttling = true
			limit := precopyThrottleLimit(cost)
			k.setDirtyLimit(limit)
			log.Infof("Pre-copy: limiting each MemoryManager's dirtying to %d bytes/s", limit)
		}
		if stop != "" {
			log.Infof("Pre-copy done after %d rounds (%s): %d bytes in %v", round+1, stop, copied, time.Since(start))
			return nil
		}
	}
}

// precopyNext decides what follows round, which wrote written bytes and left
// pending bytes dirty, at cost per MiB, with dirtying throttled if throttling
// is true. It returns why the rounds stop, or "" if another round runs; and
// whether to throttle dirtying from now on. It is QEMU's stop rule, with its
// round cap, and an early stop when a round does not halve the bytes left to
// write, which throttling, if opts.Throttle, defers once.
func precopyNext(round int, written, pending uint64, cost time.Duration, opts PrecopyOpts, throttling bool) (stop string, throttle bool) {
	switch {
	case writeTime(pending, cost) <= opts.Budget:
		return "converged", false
	case round+1 >= opts.MaxRounds:
		return "round cap reached", false
	case 2*pending > written:
		// The dirty rate is at least half the write bandwidth: more rounds
		// would mostly write the same pages again, unless dirtying is
		// throttled.
		if opts.Throttle && !throttling {
			return "", true
		}
		return "pending bytes not halved", false
	default:
		return "", false
	}
}

// precopyThrottleLimit returns the rate in bytes per second to which
// throttling limits each MemoryManager's dirtying, given the write cost per
// MiB: a quarter of the write bandwidth, at which each round writes at most a
// quarter of the bytes of the previous one.
func precopyThrottleLimit(cost time.Duration) uint64 {
	return uint64(float64(1<<20) / cost.Seconds() / 4)
}

// setDirtyLimit sets the rate in bytes per second to which each
// MemoryManager's tasks may dirty memory, or removes the limit if limit is 0.
func (k *Kernel) setDirtyLimit(limit uint64) {
	if limit != 0 {
		k.dirtyLimitSession.Add(1)
	}
	k.dirtyLimit.Store(limit)
}

// throttleDirtying delays t, which just handled a fault, as long as its
// MemoryManager is over the dirtying limit, if any; see PrecopyOpts.Throttle.
// A signal or a stop ends the delay early: the task then delays again at its
// next fault.
func (t *Task) throttleDirtying() {
	limit := t.k.dirtyLimit.Load()
	if limit == 0 {
		return
	}
	d := t.MemoryManager().DirtyThrottleDelay(limit, t.k.dirtyLimitSession.Load(), t.k.MonotonicClock().Now().Nanoseconds())
	if d <= 0 {
		return
	}
	precopyThrottled.IncrementBy(uint64(d.Nanoseconds()))
	t.BlockWithTimeout(nil, true, d)
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
