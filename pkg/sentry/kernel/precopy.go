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
	"slices"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/metric"
	metricpb "gvisor.dev/gvisor/pkg/metric/metric_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

// Pre-copy.
//
// A pre-copy writes the memory of a save while the Kernel runs, as VM live
// migration does: in rounds, each of which writes the pages dirtied during
// the previous one (the first: every page, or the pages dirtied since the
// parent of an incremental save), until QEMU's stop rule holds; then the
// save, in its pause, writes the object graph and the pages dirtied during
// the last round, and refers to the copies for the others (pgalloc's
// precopy.go). The pause then lasts about the stop rule's budget plus the
// object graph's time, rather than the time to write all of memory.
//
// The rounds copy the application MemoryFile and the private MemoryFiles of
// the Kernel's filesystems (those of tmpfs mounts and overlay upper layers
// backed by a filestore on disk), which saves write too.

// PrecopyOpts configures a pre-copy.
type PrecopyOpts struct {
	// Budget is the stop rule's target: rounds stop when the pages dirtied
	// during the last one would take at most Budget to write, at the cost
	// per byte measured during that round.
	Budget time.Duration

	// MaxRounds is the maximum number of rounds.
	MaxRounds int

	// If Auto is true, the pre-copy is skipped when the cost of writing
	// pages that the last save or pre-copy round measured says that the
	// save would write its pages within Budget anyway. Without such a
	// measure, the first round measures it.
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

	// mfs holds the pre-copy of each MemoryFile that the rounds copied, or
	// nothing if the pre-copy was skipped.
	mfs map[*pgalloc.MemoryFile]*pgalloc.Precopy

	// rounds hold the pages dirtied during each round but the last, which
	// the rounds copied: the save's dirty tracking epoch includes them.
	rounds []*DirtyEpochResult

	// stats describes the rounds.
	stats precopyStats

	// k is the Kernel being saved.
	k *Kernel
}

// precopyStats describes the rounds of a pre-copy.
type precopyStats struct {
	// roundBytes and pendingBytes hold, for each round, the bytes it wrote
	// and the bytes dirtied since it started, which the next round or the
	// save writes.
	roundBytes   []uint64
	pendingBytes []uint64

	// stop is why the rounds stopped.
	stop precopyStop

	// longestStall is the longest that the pre-copy kept tasks from running:
	// while it started dirty tracking, re-armed the dirty sources for a
	// round (the write-protection source stops tasks while it arms), or
	// delayed a task to limit its dirtying.
	longestStall time.Duration
}

// precopyWriteCost is the time per MiB of writing pages assumed by the stop
// rule when a round writes too little to measure it: the cost of O_DIRECT
// writes to a local disk measured by experiment 05.
const precopyWriteCost = 340 * time.Microsecond

// writeTime returns the time to write n bytes at cost per MiB.
func writeTime(n uint64, cost time.Duration) time.Duration {
	return time.Duration(n) * cost / (1 << 20)
}

// precopyStop is why pre-copy rounds stop.
type precopyStop int

const (
	// precopyContinue means that the rounds go on.
	precopyContinue precopyStop = iota

	// precopyConverged: the pages dirtied during the last round would take
	// at most the budget to write.
	precopyConverged

	// precopyRoundCap: the rounds reached the maximum number of rounds.
	precopyRoundCap

	// precopyNotHalved: a round did not halve the bytes left to write.
	precopyNotHalved

	// precopySkipped: no round ran, since --precopy=auto found that the save
	// would write its pages within the budget anyway.
	precopySkipped
)

var precopyStopReasons = [...]*metric.FieldValue{
	precopyConverged: {Value: "converged"},
	precopyRoundCap:  {Value: "round_cap"},
	precopyNotHalved: {Value: "not_halved"},
	precopySkipped:   {Value: "skipped"},
}

// String implements fmt.Stringer.String.
func (s precopyStop) String() string {
	if s == precopyContinue {
		return "continue"
	}
	return precopyStopReasons[s].Value
}

// precopyBytesBucketer has buckets of bytes from 64 KiB to 32 GiB, each twice
// as wide as the previous one.
var precopyBytesBucketer = metric.NewExponentialBucketer(20, 0 /* width */, 64<<10 /* scale */, 2 /* growth */)

// precopyTimeBucketer has buckets of nanoseconds from start to start * 2^19,
// each twice as wide as the previous one.
func precopyTimeBucketer(start time.Duration) metric.Bucketer {
	return metric.NewExponentialBucketer(20, 0 /* width */, float64(start) /* scale */, 2 /* growth */)
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
	precopyRoundBytes = metric.MustCreateNewDistributionMetric("/checkpoint/precopy_round_bytes", false /* sync */, precopyBytesBucketer, metricpb.MetricMetadata_UNITS_NONE,
		"Bytes of memory written by each pre-copy round.")
	precopyPendingBytes = metric.MustCreateNewDistributionMetric("/checkpoint/precopy_pending_bytes", false /* sync */, precopyBytesBucketer, metricpb.MetricMetadata_UNITS_NONE,
		"Bytes of memory dirtied during each pre-copy round, left for the next round or the save to write.")
	precopyStops = metric.MustCreateNewUint64Metric("/checkpoint/precopy_stops", metric.Uint64Metadata{
		Cumulative:  true,
		Description: "Pre-copies, by why their rounds stopped: converged (the pages dirtied during the last round would take at most the budget to write), round_cap (the maximum number of rounds), not_halved (a round did not halve the bytes left to write), or skipped (--precopy=auto found that the save would write its pages within the budget anyway).",
		Fields:      []metric.Field{metric.NewField("reason", precopyStopReasons[precopyConverged:]...)},
	})
	precopyThrottled = metric.MustCreateNewUint64Metric("/checkpoint/precopy_throttled_ns", metric.Uint64Metadata{
		Cumulative:  true,
		Unit:        metricpb.MetricMetadata_UNITS_NANOSECONDS,
		Description: "Nanoseconds that tasks were delayed to limit their dirtying of memory during pre-copies.",
	})
	precopyLongestStall = metric.MustCreateNewTimerMetric("/checkpoint/precopy_longest_stall", precopyTimeBucketer(100*time.Microsecond),
		"Longest time that each pre-copy kept tasks from running: while it started dirty tracking, re-armed the dirty sources for a round, or delayed a task to limit its dirtying.")
	precopyPause = metric.MustCreateNewTimerMetric("/checkpoint/precopy_pause", precopyTimeBucketer(time.Millisecond),
		"Time that each save completing a pre-copy paused the sandbox.")
	pagesWriteCostMetric = metric.MustCreateNewUint64Metric("/checkpoint/pages_write_cost", metric.Uint64Metadata{
		Unit:        metricpb.MetricMetadata_UNITS_NANOSECONDS,
		Description: "Time per MiB that the last save, or pre-copy round, that wrote at least 1 MiB of pages took to write them, which pre-copy's stop rule and --precopy=auto estimate write times with.",
	})
)

// setPagesWriteCost records cost, the time per MiB that the last save or
// pre-copy round took to write pages.
func (k *Kernel) setPagesWriteCost(cost time.Duration) {
	k.pagesWriteCost = cost
	pagesWriteCostMetric.Set(uint64(cost))
}

// Precopy starts a save of k to pagesFile, in rounds that run while k runs,
// and returns it for SaveTo to complete. parent is the parent of the save if
// it is incremental (see SaveTo). Precopy takes ownership of pagesFile, even
// if it returns a non-nil error.
//
// Preconditions: Dirty tracking is enabled. k is running.
func (k *Kernel) Precopy(ctx context.Context, pagesFile stateio.AsyncWriter, parent *checkpointimage.Digest, opts PrecopyOpts) (*Precopy, error) {
	return k.precopy(ctx, pagesFile, parent, k.privateMemoryFiles(ctx), opts)
}

// precopy is Precopy, with privates the private MemoryFiles that the save
// will save.
func (k *Kernel) precopy(ctx context.Context, pagesFile stateio.AsyncWriter, parent *checkpointimage.Digest, privates []*pgalloc.MemoryFile, opts PrecopyOpts) (*Precopy, error) {
	p := &Precopy{apfsDone: make(chan struct{}), k: k}
	apfs, err := pgalloc.StartAsyncPagesFileSave(pagesFile, func(err error) {
		p.apfsErr = err
		close(p.apfsDone)
	}) // transfers ownership
	if err != nil {
		return nil, fmt.Errorf("failed to start async pages file saving: %w", err)
	}
	p.apfs = apfs
	if err := k.precopyRounds(ctx, p, parent, append([]*pgalloc.MemoryFile{k.mf}, privates...), opts); err != nil {
		p.Release()
		return nil, err
	}
	return p, nil
}

// privateMemoryFiles returns the private MemoryFiles of k's filesystems that
// a save saves: those of tmpfs filesystems that have a resource ID, as
// tmpfs's PrepareSave records them.
func (k *Kernel) privateMemoryFiles(ctx context.Context) []*pgalloc.MemoryFile {
	var mfs []*pgalloc.MemoryFile
	for _, fs := range k.vfs.GetFilesystems() {
		if mf := tmpfs.MemoryFileOf(fs); mf != nil && mf.ResourceID().Ok() && !slices.Contains(mfs, mf) {
			mfs = append(mfs, mf)
		}
		fs.DecRef(ctx)
	}
	return mfs
}

// Release releases p. If no save consumed p, the pages that its rounds copied
// are returned to the dirty sets, so that the next save writes them. SaveTo
// releases the Precopy it is given.
func (p *Precopy) Release() {
	p.k.setDirtyLimit(0)
	p.stalled(time.Duration(p.k.dirtyLimitLongestDelay.Swap(0)))
	for _, e := range p.rounds {
		e.Abort()
	}
	p.rounds = nil
	p.finishWriting()
	if len(p.mfs) != 0 {
		precopyLongestStall.AddSample(int64(p.stats.longestStall))
		log.Infof("Pre-copy: tasks were kept from running for at most %v", p.stats.longestStall)
	}
}

// RecordPause records that the save that completed p paused the sandbox for
// d.
func (p *Precopy) RecordPause(d time.Duration) {
	if len(p.mfs) != 0 {
		precopyPause.AddSample(int64(d))
	}
}

// finishWriting completes the pages file and returns the error that
// terminated writing it, if any.
func (p *Precopy) finishWriting() error {
	p.apfs.MemoryFilesDone()
	<-p.apfsDone
	return p.apfsErr
}

// stalled records that the pre-copy kept tasks from running for d.
func (p *Precopy) stalled(d time.Duration) {
	p.stats.longestStall = max(p.stats.longestStall, d)
}

// precopyRounds runs the rounds of p, which copy mfs, the application
// MemoryFile first.
func (k *Kernel) precopyRounds(ctx context.Context, p *Precopy, parent *checkpointimage.Digest, mfs []*pgalloc.MemoryFile, opts PrecopyOpts) error {
	if !k.DirtyTrackingEnabled() {
		return fmt.Errorf("pre-copy requires dirty tracking")
	}
	if parent != nil {
		if last, ok := k.LastImageDigest(); !ok || last != *parent {
			return fmt.Errorf("incremental save: image %v is not the image the sandbox was last saved to or restored from", *parent)
		}
	}
	// Round 0 copies all of a MemoryFile that the save writes whole, and
	// otherwise the pages dirtied since the parent.
	var (
		whole     = make(map[*pgalloc.MemoryFile]bool, len(mfs))
		untracked []*pgalloc.MemoryFile
	)
	for _, mf := range mfs {
		whole[mf] = parent == nil || !k.trackedSinceLastImage(mf)
		if !slices.Contains(k.dirty.mfs, mf) {
			untracked = append(untracked, mf)
		}
	}
	if opts.Auto && k.precopyNeedless(ctx, mfs, whole, opts.Budget) {
		p.stats.stop = precopySkipped
		precopyStops.Increment(precopyStopReasons[precopySkipped])
		return nil
	}
	if len(untracked) != 0 {
		// Tracking starts at saves and restores, for the MemoryFiles they
		// save or load: these were not, and the rounds need their writes.
		stall, err := k.trackDirtyRunning(ctx, untracked)
		p.stalled(stall)
		if err != nil {
			return err
		}
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

	p.mfs = make(map[*pgalloc.MemoryFile]*pgalloc.Precopy, len(mfs))
	for _, mf := range mfs {
		pc, err := mf.StartPrecopy(p.apfs)
		if err != nil {
			return err
		}
		p.mfs[mf] = pc
	}
	start := time.Now()
	var (
		copied     uint64
		throttling bool
	)
	for round := 0; ; round++ {
		e, stall, err := k.precopyEpoch(ctx)
		p.stalled(stall)
		if err != nil {
			return err
		}
		p.rounds = append(p.rounds, e)
		roundStart := time.Now()
		var n uint64
		for _, mf := range mfs {
			pc := p.mfs[mf]
			if round == 0 && whole[mf] {
				c, err := pc.CopyAll()
				if err != nil {
					return err
				}
				n += c
			} else {
				n += pc.Copy(e.Sets[mf])
			}
		}
		for _, mf := range mfs {
			if err := p.mfs[mf].Wait(); err != nil {
				return fmt.Errorf("pre-copy round %d: %w", round, err)
			}
		}
		took := time.Since(roundStart)
		copied += n
		precopyRounds.Increment()
		precopyBytes.IncrementBy(n)
		precopyRoundBytes.AddSample(int64(n))

		cost := k.pagesWriteCost
		if cost == 0 {
			cost = precopyWriteCost
		}
		if n >= 1<<20 {
			cost = took * (1 << 20) / time.Duration(n)
			k.setPagesWriteCost(cost)
		}
		pending, err := k.pendingDirtyBytes(ctx, mfs)
		if err != nil {
			return err
		}
		precopyPendingBytes.AddSample(int64(pending))
		p.stats.roundBytes = append(p.stats.roundBytes, n)
		p.stats.pendingBytes = append(p.stats.pendingBytes, pending)
		log.Infof("Pre-copy round %d: wrote %d bytes in %v; %d bytes dirty since, %v to write (budget %v)", round, n, took, pending, writeTime(pending, cost), opts.Budget)
		stop, throttle := precopyNext(round, n, pending, cost, opts, throttling)
		if throttle {
			throttling = true
			limit := precopyThrottleLimit(cost)
			k.setDirtyLimit(limit)
			log.Infof("Pre-copy: limiting each MemoryManager's dirtying to %d bytes/s", limit)
		}
		if stop != precopyContinue {
			p.stats.stop = stop
			precopyStops.Increment(precopyStopReasons[stop])
			log.Infof("Pre-copy done after %d rounds (%v): %d bytes in %v", round+1, stop, copied, time.Since(start))
			return nil
		}
	}
}

// precopyNext decides what follows round, which wrote written bytes and left
// pending bytes dirty, at cost per MiB, with dirtying throttled if throttling
// is true. It returns why the rounds stop, or precopyContinue; and whether to
// throttle dirtying from now on. It is QEMU's stop rule, with its round cap,
// and an early stop when a round does not halve the bytes left to write,
// which throttling, if opts.Throttle, defers once.
func precopyNext(round int, written, pending uint64, cost time.Duration, opts PrecopyOpts, throttling bool) (stop precopyStop, throttle bool) {
	switch {
	case writeTime(pending, cost) <= opts.Budget:
		return precopyConverged, false
	case round+1 >= opts.MaxRounds:
		return precopyRoundCap, false
	case 2*pending > written:
		// The dirty rate is at least half the write bandwidth: more rounds
		// would mostly write the same pages again, unless dirtying is
		// throttled.
		if opts.Throttle && !throttling {
			return precopyContinue, true
		}
		return precopyNotHalved, false
	default:
		return precopyContinue, false
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
		k.dirtyLimitLongestDelay.Store(0)
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
	left, _ := t.BlockWithTimeout(nil, true, d)
	t.k.recordThrottleDelay(d - left)
}

// recordThrottleDelay records that the dirtying limit delayed a task for d.
func (k *Kernel) recordThrottleDelay(d time.Duration) {
	precopyThrottled.IncrementBy(uint64(d))
	for {
		longest := k.dirtyLimitLongestDelay.Load()
		if int64(d) <= longest || k.dirtyLimitLongestDelay.CompareAndSwap(longest, int64(d)) {
			return
		}
	}
}

// precopyEpoch ends the dirty tracking epoch of a round, as DirtyEpoch does
// while tasks run, and returns its dirty sets and the time that re-arming the
// dirty sources took, for which the write-protection source stops tasks.
func (k *Kernel) precopyEpoch(ctx context.Context) (*DirtyEpochResult, time.Duration, error) {
	e, err := k.swapDirty(ctx, false /* paused */)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	if err := k.armDirtySources(ctx, false /* paused */); err != nil {
		e.Abort()
		return nil, 0, err
	}
	return e, time.Since(start), nil
}

// trackDirtyRunning starts dirty tracking of mfs, which are not tracked, while
// k runs, and returns how long it paused k. Tracking of the application
// MemoryFile starts so when the first save of a Kernel pre-copies it.
func (k *Kernel) trackDirtyRunning(ctx context.Context, mfs []*pgalloc.MemoryFile) (stall time.Duration, err error) {
	start := time.Now()
	k.Pause()
	defer func() {
		k.Unpause()
		stall = time.Since(start)
	}()
	for _, mf := range mfs {
		mf.EnableDirtyTracking()
		if k.dirty.Verify {
			if err := mf.RecordPageHashes(); err != nil {
				return 0, fmt.Errorf("recording page hashes of MemoryFile %p: %w", mf, err)
			}
		}
	}
	k.dirty.mfs = append(k.dirty.mfs, mfs...)
	return 0, k.armDirtySources(ctx, true /* paused */)
}

// trackedSinceLastImage returns true if the pages of mf dirtied since the
// image k was last saved to or restored from are tracked, and that image holds
// the others: mf was tracked since, and is k's application MemoryFile, or a
// private MemoryFile of that image.
func (k *Kernel) trackedSinceLastImage(mf *pgalloc.MemoryFile) bool {
	if k.dirty.last == nil || !slices.Contains(k.dirty.mfs, mf) {
		return false
	}
	if mf == k.mf {
		return true
	}
	id := mf.ResourceID()
	for _, pid := range k.dirty.last.Proto.GetPrivateMemoryFiles() {
		if pid.GetContainerName() == id.ContainerName && pid.GetPath() == id.Path {
			return true
		}
	}
	return false
}

// precopyNeedless returns true if the cost of writing pages that the last
// save or pre-copy round measured says that a save would write the pages of
// mfs within budget: all of those that whole holds, the pages dirtied since
// the parent of the others.
func (k *Kernel) precopyNeedless(ctx context.Context, mfs []*pgalloc.MemoryFile, whole map[*pgalloc.MemoryFile]bool, budget time.Duration) bool {
	if k.pagesWriteCost == 0 {
		// Nothing measured it yet: the first round will.
		return false
	}
	var bytes uint64
	for _, mf := range mfs {
		if !whole[mf] {
			continue
		}
		usage, err := mf.TotalUsage()
		if err != nil {
			return false
		}
		bytes += usage
	}
	var delta []*pgalloc.MemoryFile
	for _, mf := range mfs {
		if !whole[mf] {
			delta = append(delta, mf)
		}
	}
	if len(delta) != 0 {
		dirty, err := k.pendingDirtyBytes(ctx, delta)
		if err != nil {
			return false
		}
		bytes += dirty
	}
	estimate := writeTime(bytes, k.pagesWriteCost)
	if estimate > budget {
		return false
	}
	log.Infof("Pre-copy skipped: %d bytes to write, %v at the last measured cost (budget %v)", bytes, estimate, budget)
	return true
}

// pendingDirtyBytes returns the number of bytes of mfs, which are tracked,
// dirtied since the last dirty tracking epoch.
func (k *Kernel) pendingDirtyBytes(ctx context.Context, mfs []*pgalloc.MemoryFile) (uint64, error) {
	for _, s := range k.dirty.Sources {
		if err := s.Harvest(ctx); err != nil {
			return 0, fmt.Errorf("dirty source %s: harvesting: %w", s.Name(), err)
		}
	}
	var n uint64
	for _, mf := range mfs {
		n += mf.DirtyBytes()
	}
	return n, nil
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

// beginSave begins a save as to dirty tracking, if tracking has started: it
// ends the dirty tracking epoch at the start of the save, and returns the
// pages dirtied since the last save or restore, or since precopy started if
// precopy is not nil, which endDirtySave must take even if beginSave fails.
// It also returns the pages dirtied during precopy's last round, which the
// save writes, and, if parent is not nil, the incremental save relative to it.
//
// Preconditions: Dirty tracking is enabled. The Kernel is paused.
func (k *Kernel) beginSave(ctx context.Context, parent *checkpointimage.Digest, precopy *Precopy) (saveEpoch, lastRound *DirtyEpochResult, delta *incrementalSave, err error) {
	if saveEpoch, err = k.beginDirtySave(ctx); err != nil {
		return nil, nil, nil, err
	}
	if precopy != nil {
		lastRound = saveEpoch
		saveEpoch = precopy.epochSince(saveEpoch)
	}
	if parent != nil {
		// The pages dirtied since the parent include those that the
		// pre-copy's rounds took from the dirty sets: those of MemoryFiles
		// that it did not copy are in no copy.
		delta, err = k.beginIncrementalSave(*parent, saveEpoch)
	}
	return saveEpoch, lastRound, delta, err
}

// setSaveOpts makes opts complete p's pre-copy of mf, if p copied mf: the
// pages that last, the dirty tracking epoch of the save, does not hold, which
// are unchanged since p's last round, refer to their copies.
func (p *Precopy) setSaveOpts(opts *pgalloc.SaveOpts, mf *pgalloc.MemoryFile, last *DirtyEpochResult) {
	pc, ok := p.mfs[mf]
	if !ok {
		return
	}
	set, ok := last.Sets[mf]
	if !ok {
		return
	}
	opts.Precopy = pc
	opts.Clean = func(off uint64) bool { return !set.Contains(off) }
}
