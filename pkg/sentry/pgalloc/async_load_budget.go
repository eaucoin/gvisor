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

// aplBackgroundBudget is the bound on the bytes of background reads that the
// async page loader keeps in flight.
//
// aplBackgroundBudget is exclusive to the async page loader goroutine.
type aplBackgroundBudget struct {
	// minBytes and maxBytes bound the budget: at least one read, and at most
	// what the pages file can have in flight.
	minBytes uint64
	maxBytes uint64

	// bytes is the current budget.
	bytes uint64

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
	}
}

// readCompleted records the completion at now of a read of n bytes issued at
// issued. fullBackground is true if the read was a background read of the
// maximum read size.
func (b *aplBackgroundBudget) readCompleted(now int64, n uint64, issued int64, fullBackground bool) {
	b.windowBytes += n
	if !fullBackground {
		return
	}
	latency := time.Duration(now - issued)
	if !b.latencyKnown || latency < b.minLatency {
		b.minLatency, b.latencyKnown = latency, true
	}
	if b.startup {
		if latency > b.minLatency+min(max(b.minLatency/8, aplSlowStartMinDelay), aplSlowStartMaxDelay) {
			b.startup = false
			b.setBandwidthDelayProduct()
		} else {
			b.bytes = min(b.maxBytes, b.bytes+n)
		}
	}
}

// update ends the current bandwidth window if it has lasted
// aplBandwidthWindow, and at least the minimum latency of a background read,
// by now, and updates the budget. The first window ends when the first
// full-size background read completes, so that the budget grows from one read
// as soon as the latency is known.
func (b *aplBackgroundBudget) update(now int64) {
	if !b.latencyKnown {
		return
	}
	elapsed := time.Duration(now - b.windowStart)
	if b.windows > 0 && elapsed < max(aplBandwidthWindow, b.minLatency) {
		return
	}
	b.pastBytes[b.nextWindow] = b.windowBytes
	b.pastDurations[b.nextWindow] = elapsed
	b.nextWindow = (b.nextWindow + 1) % aplBandwidthWindows
	b.windows++
	b.windowStart, b.windowBytes = now, 0
	if !b.startup {
		b.setBandwidthDelayProduct()
	}
}

// setBandwidthDelayProduct sets the budget to the bandwidth times the minimum
// latency plus aplQueueDelay, within its bounds.
func (b *aplBackgroundBudget) setBandwidthDelayProduct() {
	bdp := b.bandwidth() * (b.minLatency + aplQueueDelay).Seconds()
	switch {
	case bdp >= float64(b.maxBytes):
		b.bytes = b.maxBytes
	case bdp <= float64(b.minBytes):
		b.bytes = b.minBytes
	default:
		b.bytes = uint64(bdp)
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
	return fmt.Sprintf("slow start %t, bandwidth %.3f MB/s, minimum latency %v", b.startup, b.bandwidth()*1e-6, b.minLatency)
}
