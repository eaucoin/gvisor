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
	"time"

	"gvisor.dev/gvisor/pkg/gohacks"
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
// each failure. Background bandwidth matters most while nothing waits for
// pages, and more reads in flight may slow awaited ones on such a store, so a
// probe only starts once no awaited read completed for a bandwidth window or
// the minimum latency, whichever is longer, and the floor drops back to the
// bandwidth-delay product, and probing backs off, as soon as an awaited read
// issued while the budget was raised took aplQueueDelay longer than the
// shortest awaited read issued while it was not, or than its bytes take at
// the rate of one full-size read alone: probing must not cost page faults
// what the bandwidth-delay product spares them.
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

	// aplReadFullBackground is a background read of the maximum read size.
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

	// parallel is true if the read that ended the slow start found more reads
	// in flight to deliver more bandwidth; probing is only done then.
	parallel bool

	// floor is the number of full-size reads that the budget allows at least,
	// as probing found them worth it (0 if it found none).
	floor uint64

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
	// defers probing by.
	nextProbe int64
	backoff   time.Duration

	// raised is true while probing raises the budget above the
	// bandwidth-delay product, since raisedAt.
	raised   bool
	raisedAt int64

	// probesKept and probesFailed count the probes that were decided, and
	// drops the times that a slow awaited read dropped the floor.
	probesKept   int
	probesFailed int
	drops        int

	// lastAwaited is the time at which the last awaited read completed.
	lastAwaited int64

	// awaitedLatency is the minimum latency of awaited reads issued while
	// the budget was not raised, if awaitedLatencyKnown is true.
	awaitedLatency      time.Duration
	awaitedLatencyKnown bool
}

// init starts the budget at now, for reads of readBytes, with at most maxBytes
// in flight.
func (b *aplBackgroundBudget) init(now int64, readBytes, maxBytes uint64) {
	maxBytes = max(readBytes, maxBytes)
	*b = aplBackgroundBudget{
		minBytes:    readBytes,
		maxBytes:    maxBytes,
		bytes:       min(aplInitialReads*readBytes, maxBytes),
		startup:     true,
		windowStart: now,
		latencies:   make([]time.Duration, maxBytes/readBytes+1),
	}
}

// readCompleted records the completion at now of a read of n bytes of the
// given kind, issued at issued with inflight bytes of background reads in
// flight, itself included if it is a background read.
func (b *aplBackgroundBudget) readCompleted(now int64, n uint64, issued int64, kind aplReadKind, inflight uint64) {
	b.windowBytes += n
	latency := time.Duration(now - issued)
	switch kind {
	case aplReadAwaited:
		b.awaitedReadCompleted(now, n, latency, issued)
		return
	case aplReadBackground:
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
	b.parallel = reads > 1 && ratio*b.minLatency.Seconds()/latency.Seconds() >= aplFullPipeGain(ratio)
	if b.parallel {
		b.step = b.reads
		b.nextProbe = now
		b.backoff = aplBandwidthWindows * max(aplBandwidthWindow, b.minLatency)
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
		b.backoff *= 2
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
// bytes issued at issued, which took latency, and drops the floor if the read
// was issued while the budget was raised and took more than aplQueueDelay
// longer than an awaited read did while it was not, or than the pages file
// takes to deliver n bytes at the rate of one full-size read alone.
func (b *aplBackgroundBudget) awaitedReadCompleted(now int64, n uint64, latency time.Duration, issued int64) {
	b.lastAwaited = now
	if !b.raised || issued < b.raisedAt {
		if !b.awaitedLatencyKnown || latency < b.awaitedLatency {
			b.awaitedLatency, b.awaitedLatencyKnown = latency, true
		}
		return
	}
	usual := time.Duration(float64(b.minLatency) * float64(n) / float64(b.minBytes))
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
	b.backoff *= 2
	b.setBandwidthDelayProduct(now)
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
		if !b.startup {
			b.setBandwidthDelayProduct(now)
		}
	}
	quiet := now-b.lastAwaited >= int64(max(aplBandwidthWindow, b.minLatency))
	if maxReads := b.maxBytes / b.minBytes; b.parallel && quiet && b.probe == 0 && now >= b.nextProbe && b.reads < maxReads {
		if _, ok := b.bandwidthAtMost(b.reads); ok {
			b.probeFrom = b.reads
			b.probe = min(b.reads+max(b.step, 1), maxReads)
			b.setBandwidthDelayProduct(now)
		}
	}
}

// setBandwidthDelayProduct sets the budget at now to the bandwidth times the
// minimum latency plus aplQueueDelay, or to the floor of reads or the reads
// probed if that is more, within its bounds.
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
	return fmt.Sprintf("slow start %t, bandwidth %.3f MB/s, minimum latency %v, parallel %t, floor %d reads; probes kept %d, failed %d; dropped for awaited reads %d times", b.startup, b.bandwidth()*1e-6, b.minLatency, b.parallel, b.floor, b.probesKept, b.probesFailed, b.drops)
}
