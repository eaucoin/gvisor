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

package pgalloc

import (
	"fmt"
	"math"
	"slices"
	"time"

	"gvisor.dev/gvisor/pkg/gohacks"
	"gvisor.dev/gvisor/pkg/hostarch"
)

// The async page loader reads pages that no one waits for ("background reads")
// while the application runs, and the pages that page faults and syscalls wait
// for ("awaited reads") as they come. Reads in flight cannot be recalled, so on
// a pages file served in order, such as a disk or a FUSE connection, an awaited
// read waits for every background byte in flight before it: with 32 MiB in
// flight and 100 MiB/s, 320 ms per fault. The loader therefore bounds the
// background bytes in flight to the pages file's bandwidth-delay product, as
// BBR sizes a TCP sender's window: the bandwidth it delivers, times the latency
// of a background read without queueing, plus aplQueueDelay, the time an
// awaited read may wait behind background reads. By Little's law this keeps
// the pages file as busy as unbounded background reads would, and it bounds an
// awaited read's wait to about aplQueueDelay plus one background read, on a
// disk (sub-millisecond reads: a few MiB in flight) as on an object store
// (16 MiB reads taking tens of milliseconds, served in parallel: several reads
// in flight).
//
// The latency is the minimum over the load of the latency of full-size
// background reads, which the first read measures without queueing; later
// reads queue behind each other on a disk, and a minimum that
// expired would let that queueing raise the bound it causes. The bandwidth is
// the bytes delivered over the last aplBandwidthWindows windows divided by
// their duration, each window lasting aplBandwidthWindow or the latency,
// whichever is longer: windows shorter than a read overestimate it. Unlike
// BBR, which takes the maximum of its windows because a network's bottleneck
// cannot deliver more than its rate, the loader averages them: a pages file
// throttled by a token bucket, as cgroup io.max and cloud disks are, delivers
// bursts above its sustained rate, whose maximum overestimated the budget
// fourfold on a disk throttled to 100 MiB/s. An average limited by the
// background reads in flight does not shrink the budget, which exceeds them
// by aplQueueDelay of bandwidth; only reads that queue for longer than
// aplQueueDelay do.
//
// A source that serves reads in parallel, such as an object store, delivers
// more bandwidth with more reads in flight, which a budget derived from the
// bandwidth of fewer reads cannot discover but slowly. Loading therefore
// starts as TCP's slow start does, from aplInitialReads reads, adding the
// bytes of every full-size background read that completes without queueing
// (doubling the budget per round trip), and leaves it, as HyStart++ (RFC 9406)
// does, at the first read that queued for longer than an eighth of the minimum
// latency, within [4 ms, 16 ms] (on a disk at 100 MiB/s, the third read; the
// latency of a 16 MiB read from an object store varies by more than 4 ms
// without queueing), for the bandwidth-delay product measured so far, as BBR
// drains the queue its startup built: until then, the restore's reads of the
// state file wait behind the slow start's.
//
// On a pages file that serves reads in parallel but slows each read as more
// are in flight, as an object store sharing its CPUs does, the slow start ends
// at the second or third read, and the bandwidth-delay product, which takes
// the latency of a read alone, settles at a read or two: one read in flight
// measures one read's bandwidth, and the product of that and one read's
// latency is one read. The store delivers more with more reads in flight, but
// the budget never tries them. The slow start tells which pages files these
// are: by Little's law, the reads in flight when the read that ended it was
// issued, over its latency, are the bandwidth it saw, and the minimum latency
// that of one read; on a disk or a FUSE connection, which serve reads in
// order, the latency grows as the reads in flight do and the bandwidth does
// not, while on these it grows less. After the slow start of such a pages
// file, the loader probes for more reads in flight, as BBR's ProbeBW does:
// it raises the budget to twice the reads it allows, and keeps them as its
// floor if the reads issued with that many in flight deliver more bandwidth,
// by Little's law, than any fewer did, by at least what BBR's startup
// requires of a doubling (25%, prorated for other ratios: aplFullPipeGain),
// and probes again; otherwise it probes for half as many reads more, or,
// after a probe for one read more failed, waits for a backoff that doubles at
// each failure, up to aplMaxBackoff. Background bandwidth matters most while
// nothing waits for pages, and more reads in flight may slow awaited ones on
// such a store, so a probe only starts once no awaited read completed for a
// bandwidth window or the minimum latency, whichever is longer, and the floor
// drops back to the bandwidth-delay product, and probing backs off, as soon
// as an awaited read issued while the budget was raised took aplQueueDelay
// longer than the shortest awaited read issued while it was not, or than it
// would take alone (the latency of the quickest read, plus its bytes at the
// rate of one full-size read): probing must not cost page faults what the
// bandwidth-delay product spares them.
//
// A pages file may say how many reads it serves at once, each at the rate of
// one alone (stateio.ParallelAsyncReader), as an object store does that serves
// each read on a connection of its own. Loading from it keeps at least that
// many full-size reads in flight from its start, a floor that neither the slow
// start nor the bandwidth-delay product lowers. Without it, on an object store
// that caps the rate of each connection, as S3 does at 85-90 MB/s, and shares
// its CPUs, the slow start ends at a few reads, and the page faults of a
// restored application, which touches most of its memory, keep loading from
// ever being quiet enough to probe: restoring 3 GiB from an S3-like store, the
// budget stayed at one 16 MiB read, 40-90 MB/s, where eight connections
// delivered 194 MB/s. Awaited reads still go first, in the slot kept for them.
//
// Whether the floor's reads slow awaited reads is judged on many awaited reads,
// not on one. Each awaited read from such a store has a connection of its own,
// and its latency varies by far more than aplQueueDelay with nothing else in
// flight, as an object store's first bytes do; and the quickest read, or the
// quickest awaited read, against which one read was judged, is the least of
// them. So the first awaited read a few milliseconds slower than the quickest
// dropped the floor, whatever the floor's reads did to it, and with the floor
// gone the bandwidth-delay product, which takes the latency of the quickest
// read, fell with the reads in flight to one read, which no probe raised while
// page faults came: restoring 3.25 GB from an S3-like store, the floor dropped
// 5.5-8 s in, on an awaited read 4-244 ms slower than the quickest, after which
// one 16 MiB read stayed in flight and loading took 35-48 s rather than ~17.
//
// Instead, every aplAwaitedSamples awaited reads, the median latency of those
// issued with two full-size background reads in flight or more is compared
// with the median latency of the last awaited reads issued with fewer
// ("alone": one full-size read and a short one may be in flight beside them),
// each less its bytes at the rate of a full-size read. If it exceeds it by
// more than aplAwaitedSlowdown, plus aplQueueDelay, the floor is cut (below);
// otherwise, or if too few of them were issued with background reads in
// flight to tell, it grows by one read, up to the reads the pages file's
// bandwidth needs (below): a multiplicative decrease and an additive
// increase, as TCP's congestion avoidance, around the most reads in flight
// that keep awaited reads at most aplAwaitedSlowdown slower at the median.
// Until awaited reads alone were measured, the floor holds the reads in
// flight above one, so the first judgement holds the budget at one read until
// aplAwaitedSamples awaited reads were issued alone, as BBR's ProbeRTT drains
// its queue to measure the round trip, and then returns to the floor; a drain
// that ends, after twice the minimum latency and the time the awaited reads
// took, with fewer than half of them issued one at a time is retried after a
// backoff that doubles at each such drain, up to aplMaxBackoff, and starts
// over from aplBandwidthWindows quiet periods once a drain issued enough of
// them one at a time. No drain starts while less is left to load than the
// pages file delivers over one: loading would end within it, at one read
// rather than the floor's, and the floor's reads, and what they do to awaited
// reads, with it.
//
// Awaited reads alone are compared with as they were issued one at a time, with
// no other awaited read in flight, where aplAwaitedSamples/2 of the last were.
// A restored application faults in bursts, its threads waiting for pages at
// once as it resumes, and awaited reads in flight together slow each other:
// from an S3 Express-like store, awaited reads alone took 39 ms at the median
// one at a time and 65 ms beside others, and the first drain, during such a
// burst, measured 91 ms (39-223 ms by restore). Against that, the floor's
// reads, which slowed awaited reads to 139 ms at the median with nine or more
// in flight, were kept through the application's next requests.
// Where awaited reads alone were only measured beside others, and an awaited
// read issued with no other in flight, background reads in flight or not, was
// quicker than they were by more than the floor may slow them, a drain
// measures them again, after the backoff of a drain that measured too few. From
// an S3-like store, whose awaited reads mostly overlap but take its first
// byte's 150 ms either way (178 ms one at a time, 176 ms beside others), the
// reads measured beside others stand.
//
// Awaited reads alone measured over aplBandwidthWindows minimum latencies ago
// are judged by only where awaited reads issued with background reads in
// flight took within aplAwaitedSlowdown of them, either way; otherwise a drain
// measures awaited reads alone anew, as when there were none. With the floor
// at two reads or more none is issued alone, and a store's latency drifts as
// a restored application's faults come and go: from an S3-like store awaited
// reads took 175 ms alone as an application resumed and 210-310 ms seconds
// later with one read in flight, so that, judged against the resume's, one
// read slowed them and the floor stayed at it, 40 MB/s, for the rest of a
// 3.25 GB restore; from an S3 Express-like store the first drain, in the
// resume's burst, measured them alone at 100 ms or more, against which three
// or four reads in flight passed through the first request, slowing awaited
// reads from 41 ms alone to 75-100.
//
// A judgement that finds awaited reads slowed too much cuts the floor in
// proportion: awaited reads slow about in proportion to the background reads
// in flight beyond one, by 20-40% a read from an S3 Express-like store and
// 5-8% from an S3-like one, so the floor falls to as many reads as slow them by
// aplAwaitedSlowdown at most, or halves if that is more; and it grows back to
// the reads it was cut from, whether for want of awaited reads with
// background reads in flight to judge it by or for a judgement that kept it
// lower, only after a hold of aplBandwidthWindows quiet periods, doubled at
// each cut, up to aplMaxBackoff, until a judgement keeps it there, or at the
// most reads it may be (below) if fewer. Where awaited reads issued one at
// a time were slowed by more than twice aplAwaitedSlowdown against awaited
// reads alone issued one at a time, the hold lasts until no awaited read
// completed for aplAwaitedSamples quiet periods: from an S3 Express-like
// store, two background reads in flight slow awaited reads by 50-100% (41 to
// 60-72 ms at the median), and each end of a hold grew the floor back to two
// reads for a judgement, until a third of a restored application's first
// request's awaited reads went out beside two to four. Awaited reads issued
// beside others, as in a restored application's burst of page faults, hold
// it only for a while: from an S3-like store, held for good by such a
// judgement as an application resumed, the floor stayed at one read through
// its first request, a 3.25 GB load taking 40-52 s rather than 26-30. A floor
// of one read is not cut, nor held.
//
// The floor grows by a read for want of awaited reads to judge it by only once
// none completed for aplAwaitedSamples quiet periods, and by one each period
// from then on: between two of an application's requests, or between two of
// its threads' page faults, each period without awaited reads raised it by a
// read, back to the reads the pages file serves at once, and the next
// awaited reads waited behind them until judgements halved it again.
//
// The floor grows, and probes raise the budget, only up to the reads that the
// pages file's bandwidth needs in flight (kneeReads): the most bandwidth
// measured over aplBandwidthWindows windows so far, times the minimum latency
// of a full-size read, rounded up, and a read more, as BBR bounds its data in
// flight by its bottleneck bandwidth times its minimum round trip. More reads
// in flight deliver no more; they only queue, and awaited reads with them:
// from an S3 Express-like store (a 16 MiB read alone in 225 ms, up to 150-200
// MB/s) and a local one (16 MiB in 55-60 ms, up to 180-250 MB/s), 3-4 and 12
// reads in flight loaded the same 110-130 MiB/s as a restored application
// resumed, while a probe, once the floor was cut, raised the budget to the
// most reads the pages file serves between two page faults. Once a drain
// issued reads alone, the bound also cuts the floor: 2 reads from the local
// store, 3-4 from the S3 Express-like one and 5-6 from an S3-like one (16 MiB
// in 370 ms, up to 150-185 MB/s), whose awaited reads, 6-21% slower with two
// to eight reads in flight, keep it. Before, it only stops growth: the
// bandwidth's first windows, before the first reads are all delivered,
// measure less than the pages file delivers, and the minimum latency is that
// of reads issued together.
var (
	// aplQueueDelay is the time an awaited read may wait behind the
	// background reads in flight, beyond their own latency.
	aplQueueDelay = 2 * time.Millisecond

	// aplBandwidthWindow is the period over which bandwidth is measured.
	aplBandwidthWindow = 20 * time.Millisecond

	// aplNanotime is the clock of async page loading, replaced by tests.
	aplNanotime = gohacks.Nanotime
)

// aplSlowStartMinDelay and aplSlowStartMaxDelay bound the queueing delay that
// ends the slow start, as RFC 9406's MIN_RTT_THRESH and MAX_RTT_THRESH do.
// They also bound how much longer than usual an awaited read may take while
// probing raises the budget.
const (
	aplSlowStartMinDelay = 4 * time.Millisecond
	aplSlowStartMaxDelay = 16 * time.Millisecond
)

// aplInitialReads is the number of background reads that loading starts with,
// as TCP's initial window (RFC 6928) lets a connection start faster than one
// segment per round trip.
const aplInitialReads = 4

// aplBandwidthWindows is the number of windows over which bandwidth is
// measured.
const aplBandwidthWindows = 10

// aplMaxBackoff is the most times its first, aplBandwidthWindows quiet periods,
// that a backoff that doubles grows to: a pages file whose awaited reads are
// rarely issued alone, or whose floor's reads mostly slow them, still has a
// drain, a probe or the floor's growth tried again over a long load, and a
// backoff doubled past the range of time.Duration does not turn negative.
const aplMaxBackoff = 16

// aplAwaitedSamples is the number of awaited reads between two judgements of
// the floor of a pages file that serves reads in parallel, and the number of
// awaited reads issued alone that a judgement compares them with.
const aplAwaitedSamples = 8

// aplAwaitedSlowdown is the fraction by which awaited reads issued with the
// floor's background reads in flight may be slower than awaited reads issued
// alone, at the median, beyond aplQueueDelay.
const aplAwaitedSlowdown = 0.25

// aplProbeSamples is the number of full-size background reads issued at a
// probed floor whose latency decides the probe.
const aplProbeSamples = 2

// aplFullPipeGain returns the bandwidth gain that ratio times as many reads in
// flight must deliver to be worth keeping: 25% for twice as many, as BBR's
// startup requires of each doubling of its rate before it deems the pipe full,
// and ratio to the power log2(1.25) for other ratios.
func aplFullPipeGain(ratio float64) float64 {
	return math.Pow(ratio, math.Log2(1.25))
}

// aplReadKind classifies a read for the budget.
type aplReadKind int

const (
	// aplReadAwaited is a read of pages that are waited for.
	aplReadAwaited aplReadKind = iota

	// aplReadBackground is a background read shorter than the maximum read
	// size.
	aplReadBackground

	// aplReadFullBackground is a background read of the maximum read size,
	// or short of it by less than a gap that reads read through
	// (aplFullReadSlack).
	aplReadFullBackground
)

// aplBackgroundBudget is the bound on the bytes of background reads that the
// async page loader keeps in flight.
//
// aplBackgroundBudget is exclusive to the async page loader goroutine.
type aplBackgroundBudget struct {
	// minBytes and maxBytes bound the budget: at least one read, and at most
	// what the pages file can have in flight. minBytes is the size of a
	// full-size read.
	minBytes uint64
	maxBytes uint64

	// bytes is the current budget.
	bytes uint64

	// bdpBytes is the bandwidth-delay product, within the budget's bounds,
	// that the budget was last set to or above.
	bdpBytes uint64

	// startup is true during the slow start.
	startup bool

	// minLatency is the minimum latency of full-size background reads, if
	// latencyKnown is true; latencyKnown is false until one completes.
	minLatency   time.Duration
	latencyKnown bool

	// quickest is the minimum latency of reads of any size, if quickestKnown
	// is true.
	quickest      time.Duration
	quickestKnown bool

	// windowStart is the start of the current bandwidth window, and
	// windowBytes the number of bytes of reads completed in it.
	windowStart int64
	windowBytes uint64

	// pastBytes and pastDurations are the bytes delivered in and the
	// durations of the last aplBandwidthWindows windows, in rings indexed by
	// nextWindow. windows is the number of windows that have ended.
	pastBytes     [aplBandwidthWindows]uint64
	pastDurations [aplBandwidthWindows]time.Duration
	nextWindow    int
	windows       int

	// maxBandwidth is the most bandwidth measured over aplBandwidthWindows
	// windows, as each window ended, since loading started, in bytes per
	// second.
	maxBandwidth float64

	// parallel is true if the read that ended the slow start found more reads
	// in flight to deliver more bandwidth; probing is only done then.
	parallel bool

	// floor is the number of full-size reads that the budget allows at least,
	// as the pages file says it serves them in parallel (parallelReads) or as
	// probing found them worth it (0 if neither).
	floor uint64

	// parallelReads is the number of full-size reads that the pages file
	// serves at once, each at the rate of one alone, as it says
	// (stateio.ParallelAsyncReader), within the budget's bounds; 0 if it says
	// none.
	parallelReads uint64

	// floorReads is the floor that awaited reads allow on a pages file that
	// serves reads in parallel, from parallelReads down to one read (see
	// judgeFloor). The floor is at least floorReads, except while a drain
	// holds it at none or an awaited read that a probe slowed defers it
	// (until nextProbe).
	floorReads uint64

	// floorRaisedAt is the time at which floorReads last grew for want of
	// awaited reads to judge it by.
	floorRaisedAt int64

	// alone holds the latencies, each less its bytes at the rate of a
	// full-size read, of the last awaited reads issued with less than two
	// full-size reads' bytes of background reads in flight, aloneCount of
	// them in all, in a ring indexed by aloneCount.
	alone      [aplAwaitedSamples]time.Duration
	aloneCount int

	// aloneAt is the time at which the last awaited read issued alone
	// completed.
	aloneAt int64

	// solo holds those of them issued with no other awaited read in flight
	// either, soloCount of them in all, in a ring indexed by soloCount.
	// soloQuickest is the least latency, so reduced, of the awaited reads
	// issued with no other awaited read in flight, whatever background reads
	// were, if soloQuickestKnown is true.
	solo              [aplAwaitedSamples]time.Duration
	soloCount         int
	soloQuickest      time.Duration
	soloQuickestKnown bool

	// loaded holds those of the awaited reads issued with more background
	// reads in flight since the floor was last judged, loadedCount of them,
	// and awaitedSince counts every awaited read completed since then.
	loaded       [aplAwaitedSamples]time.Duration
	loadedCount  int
	awaitedSince int

	// loadedReads holds the full-size background reads in flight when each
	// of loaded was issued, and loadedSolo is true for those of loaded issued
	// with no other awaited read in flight.
	loadedReads [aplAwaitedSamples]uint64
	loadedSolo  [aplAwaitedSamples]bool

	// heldReads is the reads at which a judgement last found awaited reads
	// slowed too much, until a judgement keeps the floor at as many (0 if
	// none), and heldUntil the time until which the floor grows to them for
	// want of awaited reads with background reads in flight to judge it by,
	// or for a judgement that kept it lower. holdBackoff is the time that the
	// next hold lasts: aplBandwidthWindows quiet periods, doubled at each, up
	// to aplMaxBackoff (0 for the first).
	heldReads   uint64
	heldUntil   int64
	holdBackoff time.Duration

	// draining is true while the floor is held at none and the budget at one
	// read, until drainEnd, so that awaited reads are issued alone. A drain
	// that ends with too few of them defers the next one until nextDrain, by
	// drainBackoff, which doubles each time, up to aplMaxBackoff, until a drain
	// issues enough of them (0 for the first).
	draining     bool
	drainEnd     int64
	nextDrain    int64
	drainBackoff time.Duration

	// aloneAtDrain and soloAtDrain are aloneCount and soloCount when the drain
	// in progress started.
	aloneAtDrain int
	soloAtDrain  int

	// unloaded is the number of bytes left to load from the pages file, as
	// the loader last said.
	unloaded uint64

	// reads is the number of full-size reads that the budget allows.
	reads uint64

	// latencies[n] is the minimum latency of full-size background reads
	// issued with n reads in flight, or 0 if none completed.
	latencies []time.Duration

	// probe is the number of full-size reads that the budget allows while
	// they are being probed, or 0 if no probe is in progress; probeFrom is the
	// number of reads allowed before. probeBandwidth is the highest
	// bandwidth, by Little's law, of the probeSamples full-size background
	// reads issued with at least probe reads in flight so far.
	probe          uint64
	probeFrom      uint64
	probeBandwidth float64
	probeSamples   int

	// step is the number of reads that the next probe adds to the floor.
	step uint64

	// nextProbe is the time at or after which the next probe may start, and
	// backoff the time that the next failure of a probe for one read more
	// defers probing by, doubled at each, up to aplMaxBackoff.
	nextProbe int64
	backoff   time.Duration

	// raised is true while probing raises the budget above the
	// bandwidth-delay product, since raisedAt.
	raised   bool
	raisedAt int64

	// probesKept and probesFailed count the probes that were decided, drops
	// the times that slow awaited reads dropped or halved the floor, raises
	// the times that floorReads grew, and drains the drains.
	probesKept   int
	probesFailed int
	drops        int
	raises       int
	drains       int

	// lastAwaited is the time at which the last awaited read completed.
	lastAwaited int64

	// awaitedLatency is the minimum latency of awaited reads issued while
	// the budget was not raised, if awaitedLatencyKnown is true.
	awaitedLatency      time.Duration
	awaitedLatencyKnown bool
}

// init starts the budget at now, for reads of readBytes, with at most maxBytes
// in flight, from a pages file that serves parallelReads reads at once (0 if
// it does not say).
func (b *aplBackgroundBudget) init(now int64, readBytes, maxBytes, parallelReads uint64) {
	maxBytes = max(readBytes, maxBytes)
	parallelReads = min(parallelReads, maxBytes/readBytes)
	*b = aplBackgroundBudget{
		minBytes:      readBytes,
		maxBytes:      maxBytes,
		bytes:         min(max(aplInitialReads, parallelReads)*readBytes, maxBytes),
		startup:       true,
		windowStart:   now,
		latencies:     make([]time.Duration, maxBytes/readBytes+1),
		floor:         parallelReads,
		parallelReads: parallelReads,
		floorReads:    parallelReads,
	}
}

// readCompleted records the completion at now of a read of n bytes of the
// given kind, issued at issued with inflight bytes of background reads in
// flight, itself included if it is a background read, and, if it is an
// awaited read, others other awaited reads.
func (b *aplBackgroundBudget) readCompleted(now int64, n uint64, issued int64, kind aplReadKind, inflight uint64, others int) {
	b.windowBytes += n
	latency := time.Duration(now - issued)
	// An awaited read is judged against the quickest read before it.
	if kind == aplReadAwaited {
		b.awaitedReadCompleted(now, n, latency, issued, inflight, others)
	}
	if !b.quickestKnown || latency < b.quickest {
		b.quickest, b.quickestKnown = latency, true
	}
	if kind != aplReadFullBackground {
		return
	}
	reads := min((inflight+b.minBytes-1)/b.minBytes, uint64(len(b.latencies)-1))
	if !b.latencyKnown || latency < b.minLatency {
		b.minLatency, b.latencyKnown = latency, true
	}
	if b.latencies[reads] == 0 || latency < b.latencies[reads] {
		b.latencies[reads] = latency
	}
	if b.startup {
		if latency > b.minLatency+min(max(b.minLatency/8, aplSlowStartMinDelay), aplSlowStartMaxDelay) {
			b.endStartup(now, latency, reads)
		} else {
			b.bytes = min(b.maxBytes, b.bytes+n)
		}
		return
	}
	if b.probe != 0 && reads >= b.probe {
		b.probeBandwidth = max(b.probeBandwidth, float64(reads)/latency.Seconds())
		b.probeSamples++
		if b.probeSamples == aplProbeSamples {
			b.endProbe(now)
		}
	}
}

// endStartup ends the slow start at now, at a full-size background read that
// queued, issued with reads full-size reads in flight and taking latency.
func (b *aplBackgroundBudget) endStartup(now int64, latency time.Duration, reads uint64) {
	b.startup = false
	b.setBandwidthDelayProduct(now)
	// By Little's law, the bandwidth that the read saw, over that of one
	// read alone.
	ratio := float64(reads)
	b.parallel = b.parallelReads > 1 || (reads > 1 && ratio*b.minLatency.Seconds()/latency.Seconds() >= aplFullPipeGain(ratio))
	if b.parallel {
		b.step = b.reads
		b.nextProbe = now
		b.backoff = b.firstBackoff()
	}
}

// endProbe decides at now the probe in progress, whose samples are complete.
func (b *aplBackgroundBudget) endProbe(now int64) {
	from, _ := b.bandwidthAtMost(b.probeFrom)
	kept := b.probeBandwidth >= from*aplFullPipeGain(float64(b.probe)/float64(b.probeFrom))
	if kept {
		// Keep the probed reads, and probe for as many more.
		b.probesKept++
		b.floor = b.probe
		b.step *= 2
		b.nextProbe = now
	} else if b.probesFailed++; b.step > 1 {
		// Probe for fewer reads more.
		b.step /= 2
		b.nextProbe = now
	} else {
		b.nextProbe = now + int64(b.backoff)
		b.backoff = b.doubled(b.backoff)
	}
	b.probe, b.probeFrom, b.probeSamples, b.probeBandwidth = 0, 0, 0, 0
	b.setBandwidthDelayProduct(now)
}

// bandwidthAtMost returns the highest bandwidth, in reads per second by
// Little's law, of full-size background reads issued with at most reads in
// flight, and false if none completed.
func (b *aplBackgroundBudget) bandwidthAtMost(reads uint64) (float64, bool) {
	best, ok := 0.0, false
	for n := uint64(1); n <= reads && n < uint64(len(b.latencies)); n++ {
		if l := b.latencies[n]; l != 0 {
			best, ok = max(best, float64(n)/l.Seconds()), true
		}
	}
	return best, ok
}

// awaitedReadCompleted records the completion at now of an awaited read of n
// bytes issued at issued, with inflight bytes of background reads and others
// other awaited reads in flight, which took latency. On a pages file that serves reads in parallel, outside a
// probe, it judges the floor every aplAwaitedSamples awaited reads (see
// judgeFloor). Otherwise, it drops the floor if the read was issued while the
// budget was raised and took more than aplQueueDelay longer than an awaited
// read did while it was not, or than it would take alone: the latency of the
// quickest read, which is mostly the pages file's latency to a first byte,
// plus n bytes at the rate of one full-size read.
func (b *aplBackgroundBudget) awaitedReadCompleted(now int64, n uint64, latency time.Duration, issued int64, inflight uint64, others int) {
	b.lastAwaited = now
	if b.parallelReads != 0 && b.probe == 0 {
		b.parallelAwaitedReadCompleted(now, n, latency, inflight, others)
		return
	}
	if !b.raised || issued < b.raisedAt {
		if !b.awaitedLatencyKnown || latency < b.awaitedLatency {
			b.awaitedLatency, b.awaitedLatencyKnown = latency, true
		}
		return
	}
	usual := b.quickest + time.Duration(float64(b.minLatency-b.quickest)*float64(n)/float64(b.minBytes))
	if b.awaitedLatencyKnown {
		usual = min(usual, b.awaitedLatency)
	}
	if latency <= usual+aplQueueDelay {
		return
	}
	b.drops++
	b.floor = 0
	b.probe, b.probeFrom, b.probeSamples, b.probeBandwidth = 0, 0, 0, 0
	b.step = 1
	b.nextProbe = now + int64(b.backoff)
	b.backoff = b.doubled(b.backoff)
	b.setBandwidthDelayProduct(now)
}

// parallelAwaitedReadCompleted records the completion at now of an awaited
// read of n bytes from a pages file that serves reads in parallel, issued with
// inflight bytes of background reads and others other awaited reads in flight,
// which took latency, and judges the floor once aplAwaitedSamples awaited
// reads completed since it was last judged, or ends a drain once enough
// awaited reads were issued alone.
func (b *aplBackgroundBudget) parallelAwaitedReadCompleted(now int64, n uint64, latency time.Duration, inflight uint64, others int) {
	if b.startup {
		// The slow start grows the budget from the floor; the floor is
		// judged after it.
		return
	}
	// The read's latency less its bytes at the rate of a full-size read, so
	// that awaited reads of different lengths compare.
	if b.latencyKnown && b.minLatency > b.quickest {
		latency = max(latency-time.Duration(float64(b.minLatency-b.quickest)*float64(n)/float64(b.minBytes)), 0)
	}
	if others == 0 && (!b.soloQuickestKnown || latency < b.soloQuickest) {
		b.soloQuickest, b.soloQuickestKnown = latency, true
	}
	if inflight < 2*b.minBytes {
		// Alone, beside one full-size read at most: with the floor at one
		// read, a short read beside it is in flight more often than not.
		b.alone[b.aloneCount%len(b.alone)] = latency
		b.aloneCount++
		b.aloneAt = now
		if others == 0 {
			b.solo[b.soloCount%len(b.solo)] = latency
			b.soloCount++
		}
	} else if b.loadedCount < len(b.loaded) {
		b.loaded[b.loadedCount] = latency
		b.loadedReads[b.loadedCount] = (inflight + b.minBytes - 1) / b.minBytes
		b.loadedSolo[b.loadedCount] = others == 0
		b.loadedCount++
	}
	if b.draining {
		if b.aloneCount-b.aloneAtDrain >= len(b.alone) {
			b.endDrain(now)
		}
		return
	}
	if b.awaitedSince++; b.awaitedSince >= aplAwaitedSamples {
		b.judgeFloor(now)
	}
}

// judgeFloor judges at now the floor of a pages file that serves reads in
// parallel by the awaited reads completed since it was last judged: it cuts
// floorReads, and holds it below the reads it was cut from, if those issued
// with two full-size background reads in flight or more took more than
// aplAwaitedSlowdown longer, plus aplQueueDelay, than awaited reads issued
// alone did at the median, and grows it by one read if they did not or if too
// few of them were issued with background reads in flight to tell. Without
// enough awaited reads issued alone to compare with, or with only those issued
// beside other awaited reads, and slower than one issued with no other in
// flight by more than aplAwaitedSlowdown, or with only those measured too long
// ago to cut the floor by, it starts a drain to measure them instead, unless a
// drain that measured too few defers it or less is left to load than the
// pages file delivers over the drain.
func (b *aplBackgroundBudget) judgeFloor(now int64) {
	loaded, loadedReads := b.loaded[:b.loadedCount], b.loadedReads[:b.loadedCount]
	// Those of loaded issued one at a time, before loaded is reordered.
	var soloLoaded []time.Duration
	for i, l := range loaded {
		if b.loadedSolo[i] {
			soloLoaded = append(soloLoaded, l)
		}
	}
	b.awaitedSince, b.loadedCount = 0, 0
	if len(loaded) < aplAwaitedSamples/2 {
		b.raiseFloorReads(now)
		return
	}
	alone, beside, ok := b.aloneMedian()
	if !ok || (beside && b.soloQuickestKnown && alone > aplSlowed(b.soloQuickest)) {
		// Twice the minimum latency, which the background reads in flight
		// take to complete, plus what the awaited reads of a judgement take
		// one after another.
		drain := 2*b.minLatency + aplAwaitedSamples*aplMedian(loaded)
		if now >= b.nextDrain && float64(b.unloaded) > b.bandwidth()*drain.Seconds() {
			b.startDrain(now, drain)
			return
		}
		if !ok {
			return
		}
	}
	// Awaited reads alone measured long ago are judged by only where
	// awaited reads with background reads in flight took within
	// aplAwaitedSlowdown of them, either way (see the comment atop this
	// file); otherwise they are measured anew.
	stale := now-b.aloneAt > int64(aplBandwidthWindows*max(aplBandwidthWindow, b.minLatency))
	median := aplMedian(loaded)
	if stale && (aplSlowed(median) < alone || median > aplSlowed(alone)) {
		// Measure awaited reads alone anew, as when there were none, to
		// judge by.
		drain := 2*b.minLatency + aplAwaitedSamples*median
		if now >= b.nextDrain && float64(b.unloaded) > b.bandwidth()*drain.Seconds() {
			b.startDrain(now, drain)
		}
		return
	}
	if median <= aplSlowed(alone) {
		if b.floorReads >= b.heldReads {
			// Kept where it was found too slow: no hold any more.
			b.heldReads, b.holdBackoff = 0, 0
		} else if b.floorReads >= b.maxFloorReads() {
			// Kept at the most reads it may be, short of them: the next
			// cut holds it as the first did.
			b.holdBackoff = 0
		}
		b.raiseFloorReads(now)
		return
	}
	if b.floorReads <= 1 {
		// Nothing to cut.
		return
	}
	// Awaited reads slow about in proportion to the background reads in
	// flight beyond one: the floor halves, or falls to as many reads as
	// slow them by aplAwaitedSlowdown at most if that is fewer, and grows
	// back to the reads it was at only after a hold.
	slices.Sort(loadedReads)
	reads := max(loadedReads[len(loadedReads)/2], 2)
	target := uint64(1)
	slowdown := float64(median-aplQueueDelay)/float64(max(alone, 1)) - 1
	if slowdown > 0 {
		target = 1 + uint64(aplAwaitedSlowdown*float64(reads-1)/slowdown)
	}
	b.drops++
	if b.holdBackoff == 0 {
		b.holdBackoff = b.firstBackoff()
	}
	b.heldReads, b.heldUntil = b.floorReads, now+int64(b.holdBackoff)
	if !beside && len(soloLoaded) >= aplAwaitedSamples/2 && aplMedian(soloLoaded) > time.Duration(float64(alone)*(1+2*aplAwaitedSlowdown))+aplQueueDelay {
		// Awaited reads issued one at a time slowed twice as much as
		// allowed: judgements do not grow the floor back to as many reads
		// again; only a pause in awaited reads does.
		b.heldUntil = math.MaxInt64
	}
	b.holdBackoff = b.doubled(b.holdBackoff)
	b.setFloorReads(now, max(min(b.floorReads/2, target), 1))
}

// raiseFloorReads grows floorReads by one read at now, up to the reads the
// pages file serves at once or those its bandwidth needs in flight if fewer
// (maxFloorReads), unless a hold keeps it below heldReads.
func (b *aplBackgroundBudget) raiseFloorReads(now int64) {
	if reads := b.floorReads + 1; reads <= b.maxFloorReads() && (reads < b.heldReads || now >= b.heldUntil) {
		b.setFloorReads(now, reads)
	}
}

// maxFloorReads returns the most reads that floorReads may be: the reads that
// the pages file serves at once, or those its bandwidth needs in flight if
// fewer.
func (b *aplBackgroundBudget) maxFloorReads() uint64 {
	return min(b.parallelReads, b.kneeReads())
}

// kneeReads returns the full-size reads that the pages file's bandwidth needs
// in flight: the most bandwidth measured so far times the minimum latency of a
// full-size read, rounded up, and one read more, within one read and the most
// reads the budget allows; the most, before either was measured.
func (b *aplBackgroundBudget) kneeReads() uint64 {
	maxReads := b.maxBytes / b.minBytes
	if !b.latencyKnown || b.maxBandwidth == 0 {
		return maxReads
	}
	// Less a millionth of a read, so that a product of whole reads, in
	// floating point, is not rounded up past them.
	reads := uint64(math.Ceil(b.maxBandwidth*b.minLatency.Seconds()/float64(b.minBytes)-1e-6)) + 1
	return min(max(reads, 1), maxReads)
}

// aloneMedian returns the median latency of the awaited reads issued alone that
// judgements compare with: of the last issued with no other awaited read in
// flight either, if aplAwaitedSamples/2 were, else of the last issued alone,
// beside other awaited reads or not, with beside true; and false if fewer than
// aplAwaitedSamples/2 were.
func (b *aplBackgroundBudget) aloneMedian() (median time.Duration, beside bool, ok bool) {
	if n := min(b.soloCount, len(b.solo)); n >= aplAwaitedSamples/2 {
		solo := b.solo
		return aplMedian(solo[:n]), false, true
	}
	if n := min(b.aloneCount, len(b.alone)); n >= aplAwaitedSamples/2 {
		alone := b.alone
		return aplMedian(alone[:n]), true, true
	}
	return 0, false, false
}

// aplSlowed returns the most that an awaited read issued with the floor's reads
// in flight may take at the median, where awaited reads alone take latency.
func aplSlowed(latency time.Duration) time.Duration {
	return time.Duration(float64(latency)*(1+aplAwaitedSlowdown)) + aplQueueDelay
}

// setFloorReads sets floorReads to reads at now, and the floor with it unless
// a drain or a backoff holds the floor lower. A floor that a probe kept above
// floorReads is kept if floorReads grows, and cut if it shrinks.
func (b *aplBackgroundBudget) setFloorReads(now int64, reads uint64) {
	if !b.draining && b.floor >= b.floorReads {
		if reads < b.floorReads {
			b.floor = reads
		} else {
			b.floor = max(b.floor, reads)
		}
	}
	if reads > b.floorReads {
		b.raises++
	}
	b.floorReads = reads
	b.setBandwidthDelayProduct(now)
}

// startDrain holds at now the floor at none and the budget at one read, so
// that awaited reads are issued alone, for at most drain.
func (b *aplBackgroundBudget) startDrain(now int64, drain time.Duration) {
	b.draining = true
	b.drains++
	b.aloneAtDrain, b.soloAtDrain = b.aloneCount, b.soloCount
	b.floor = 0
	b.drainEnd = now + int64(drain)
	b.setBandwidthDelayProduct(now)
}

// endDrain ends the drain in progress at now, restoring the floor, and defers
// the next one if too few awaited reads were issued alone, with no other
// awaited read in flight, to judge the floor by.
func (b *aplBackgroundBudget) endDrain(now int64) {
	b.draining = false
	// Awaited reads issued while the floor's reads were still in flight
	// are not judged.
	b.loadedCount, b.awaitedSince = 0, 0
	if b.soloCount-b.soloAtDrain < aplAwaitedSamples/2 {
		if b.drainBackoff == 0 {
			b.drainBackoff = b.firstBackoff()
		}
		b.nextDrain = now + int64(b.drainBackoff)
		b.drainBackoff = b.doubled(b.drainBackoff)
	} else {
		// Enough: the next drain that measures too few defers the one
		// after it as the first did.
		b.drainBackoff = 0
	}
	b.floor = b.floorReads
	b.setBandwidthDelayProduct(now)
}

// firstBackoff returns the first backoff of a probe that failed, a hold or a
// drain that measured too few awaited reads alone: aplBandwidthWindows quiet
// periods.
func (b *aplBackgroundBudget) firstBackoff() time.Duration {
	return aplBandwidthWindows * max(aplBandwidthWindow, b.minLatency)
}

// doubled returns backoff doubled, up to aplMaxBackoff times the first.
func (b *aplBackgroundBudget) doubled(backoff time.Duration) time.Duration {
	return min(2*backoff, aplMaxBackoff*b.firstBackoff())
}

// aplMedian returns the median of latencies, which it reorders; latencies
// must not be empty.
func aplMedian(latencies []time.Duration) time.Duration {
	slices.Sort(latencies)
	if n := len(latencies); n%2 == 0 {
		return (latencies[n/2-1] + latencies[n/2]) / 2
	}
	return latencies[len(latencies)/2]
}

// update ends the current bandwidth window if it has lasted
// aplBandwidthWindow, and at least the minimum latency of a background read,
// by now, updates the budget, and starts a probe if one is due. The first
// window ends when the first full-size background read completes, so that the
// budget grows from one read as soon as the latency is known.
func (b *aplBackgroundBudget) update(now int64) {
	if !b.latencyKnown {
		return
	}
	elapsed := time.Duration(now - b.windowStart)
	if b.windows == 0 || elapsed >= max(aplBandwidthWindow, b.minLatency) {
		b.pastBytes[b.nextWindow] = b.windowBytes
		b.pastDurations[b.nextWindow] = elapsed
		b.nextWindow = (b.nextWindow + 1) % aplBandwidthWindows
		b.windows++
		b.windowStart, b.windowBytes = now, 0
		b.maxBandwidth = max(b.maxBandwidth, b.bandwidth())
		if !b.startup {
			b.setBandwidthDelayProduct(now)
		}
	}
	if b.draining && now >= b.drainEnd {
		b.endDrain(now)
	}
	if b.floor < b.floorReads && !b.draining && b.probe == 0 && now >= b.nextProbe {
		// The backoff after an awaited read that a probe slowed is over.
		b.floor = b.floorReads
		b.setBandwidthDelayProduct(now)
	}
	quietFor := int64(max(aplBandwidthWindow, b.minLatency))
	quiet := now-b.lastAwaited >= quietFor
	if b.drains > 0 && !b.draining && b.floorReads > b.maxFloorReads() {
		// Once a drain had reads issued alone measure the minimum latency,
		// the floor keeps no more reads than the bandwidth needs.
		b.setFloorReads(now, b.maxFloorReads())
	}
	if !b.draining && b.floorReads < b.maxFloorReads() && now-b.lastAwaited >= aplAwaitedSamples*quietFor && now-b.floorRaisedAt >= quietFor {
		// No awaited read to slow for aplAwaitedSamples quiet periods: the
		// floor grows by a read each period from then on.
		b.floorRaisedAt = now
		b.setFloorReads(now, b.floorReads+1)
	}
	maxReads := b.maxBytes / b.minBytes
	if b.parallelReads != 0 {
		// Reads past those the bandwidth needs deliver no more.
		maxReads = min(maxReads, b.kneeReads())
	}
	if b.parallel && quiet && !b.draining && b.probe == 0 && now >= b.nextProbe && b.reads < maxReads {
		if _, ok := b.bandwidthAtMost(b.reads); ok {
			b.probeFrom = b.reads
			b.probe = min(b.reads+max(b.step, 1), maxReads)
			b.setBandwidthDelayProduct(now)
		}
	}
}

// setBandwidthDelayProduct sets the budget at now to the bandwidth times the
// minimum latency plus aplQueueDelay, or to the floor of reads or the reads
// probed if that is more, within its bounds; or to one read while a drain is
// in progress.
func (b *aplBackgroundBudget) setBandwidthDelayProduct(now int64) {
	bdp := b.bandwidth() * (b.minLatency + aplQueueDelay).Seconds()
	switch {
	case bdp >= float64(b.maxBytes):
		b.bdpBytes = b.maxBytes
	case bdp <= float64(b.minBytes):
		b.bdpBytes = b.minBytes
	default:
		b.bdpBytes = uint64(bdp)
	}
	if b.draining {
		// One read, so that awaited reads are issued alone.
		b.bytes, b.raised, b.reads = b.minBytes, false, 1
		return
	}
	b.bytes = min(max(b.bdpBytes, max(b.floor, b.probe)*b.minBytes), b.maxBytes)
	if b.bytes > b.bdpBytes {
		if !b.raised {
			b.raised, b.raisedAt = true, now
		}
	} else {
		b.raised = false
	}
	if b.probe == 0 {
		b.reads = b.bytes / b.minBytes
	}
}

// gapBytes returns the length of the longest gap in the pages file between
// pages that a background read reads through, dropping its bytes, rather than
// ending before it and leaving the pages past it to another read: what a
// full-size read delivers in the latency of the quickest read, which a read of
// the pages past the gap would take before delivering any. The quickest read
// is mostly latency if it is short, as awaited reads are. Until one completes
// in less time than a full-size read, a gap may be as long as a full-size
// read, which then costs a read at the pages file's bandwidth.
func (b *aplBackgroundBudget) gapBytes() uint64 {
	if !b.latencyKnown || b.minLatency == 0 {
		return b.minBytes
	}
	return hostarch.PageRoundDown(uint64(float64(b.minBytes) * b.quickest.Seconds() / b.minLatency.Seconds()))
}

// bandwidth returns the bandwidth in bytes per second over the last
// aplBandwidthWindows windows.
func (b *aplBackgroundBudget) bandwidth() float64 {
	var bytes uint64
	var duration time.Duration
	for i := range b.pastBytes {
		bytes += b.pastBytes[i]
		duration += b.pastDurations[i]
	}
	if duration == 0 {
		return 0
	}
	return float64(bytes) / duration.Seconds()
}

// String implements fmt.Stringer.String.
func (b *aplBackgroundBudget) String() string {
	return fmt.Sprintf("slow start %t, bandwidth %.3f MB/s, minimum latency %v, quickest read %v, parallel %t (%d reads said), floor %d reads, %d at most (most bandwidth %.3f MB/s); probes kept %d, failed %d; dropped for awaited reads %d times; raised %d times, drained %d times, %d awaited reads alone (%d one at a time); gaps read through up to %d bytes", b.startup, b.bandwidth()*1e-6, b.minLatency, b.quickest, b.parallel, b.parallelReads, b.floor, b.maxFloorReads(), b.maxBandwidth*1e-6, b.probesKept, b.probesFailed, b.drops, b.raises, b.drains, b.aloneCount, b.soloCount, b.gapBytes())
}
