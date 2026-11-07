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
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
)

const (
	testBudgetRead = 256 << 10
	testBudgetMax  = 127 * testBudgetRead
)

// TestBackgroundBudgetSlowStart checks that the budget starts at
// aplInitialReads reads, grows by every read that completes without queueing,
// and drops to the bandwidth-delay product at the first read that queued.
func TestBackgroundBudgetSlowStart(t *testing.T) {
	var b aplBackgroundBudget
	b.init(0, testBudgetRead, testBudgetMax, 0)
	if want := uint64(aplInitialReads * testBudgetRead); b.bytes != want {
		t.Fatalf("initial budget = %d, want %d", b.bytes, want)
	}
	// Two reads complete after 1 ms, without queueing; the loader sees them
	// at 4 ms, which ends the first window: 128 MiB/s.
	ms := int64(time.Millisecond)
	b.readCompleted(1*ms, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
	b.readCompleted(1*ms, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
	b.update(4 * ms)
	if want := uint64((aplInitialReads + 2) * testBudgetRead); b.bytes != want {
		t.Fatalf("budget after two reads without queueing = %d, want %d", b.bytes, want)
	}
	// Other reads do not grow it.
	b.readCompleted(5*ms, testBudgetRead, 4*ms, aplReadBackground, testBudgetRead)
	b.update(5 * ms)
	if want := uint64((aplInitialReads + 2) * testBudgetRead); b.bytes != want {
		t.Fatalf("budget after a read that is not a full-size background read = %d, want %d", b.bytes, want)
	}
	// A read that queued for more than aplSlowStartMinDelay ends the slow
	// start.
	b.readCompleted(10*ms, testBudgetRead, 1*ms, aplReadFullBackground, testBudgetRead)
	if b.startup {
		t.Fatalf("slow start continues after a read took 9ms, with a minimum latency of 1ms")
	}
	if b.minLatency != time.Millisecond {
		t.Errorf("minimum latency = %v, want 1ms", b.minLatency)
	}
	// 128 MiB/s for 1 ms plus aplQueueDelay.
	if want := uint64(2 * testBudgetRead * (time.Millisecond + aplQueueDelay) / (4 * time.Millisecond)); b.bytes != want {
		t.Errorf("budget after the slow start = %d, want %d", b.bytes, want)
	}
}

// TestBackgroundBudgetSlowStartDelay checks that the slow start continues
// past reads whose latency varies less than an eighth of the minimum latency,
// within [aplSlowStartMinDelay, aplSlowStartMaxDelay], and ends at a read that
// queued for longer.
func TestBackgroundBudgetSlowStartDelay(t *testing.T) {
	for _, tc := range []struct {
		minLatency time.Duration
		queued     time.Duration
		exits      bool
	}{
		{minLatency: time.Millisecond, queued: 3 * time.Millisecond},
		{minLatency: time.Millisecond, queued: 5 * time.Millisecond, exits: true},
		{minLatency: 92 * time.Millisecond, queued: 11 * time.Millisecond},
		{minLatency: 92 * time.Millisecond, queued: 12 * time.Millisecond, exits: true},
		{minLatency: time.Second, queued: 15 * time.Millisecond},
		{minLatency: time.Second, queued: 17 * time.Millisecond, exits: true},
	} {
		var b aplBackgroundBudget
		b.init(0, testBudgetRead, testBudgetMax, 0)
		now := int64(tc.minLatency)
		b.readCompleted(now, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
		b.update(now)
		b.readCompleted(now, testBudgetRead, now-int64(tc.minLatency+tc.queued), aplReadFullBackground, testBudgetRead)
		if b.startup == tc.exits {
			t.Errorf("minimum latency %v, a read queued for %v: slow start continues = %t, want %t", tc.minLatency, tc.queued, b.startup, !tc.exits)
		}
	}
}

// TestBackgroundBudgetBandwidthDelayProduct checks the budget after the slow
// start: the bandwidth over the last aplBandwidthWindows windows times the
// minimum latency plus aplQueueDelay, within one read and the maximum.
func TestBackgroundBudgetBandwidthDelayProduct(t *testing.T) {
	const window = 100 * time.Millisecond
	for _, tc := range []struct {
		name    string
		latency time.Duration
		// delivered is the number of bytes reads deliver in a window of
		// 100 ms after the slow start.
		delivered uint64
		// windows is the number of such windows (default 1).
		windows int
		// minBytes and maxBytes are true if the budget must be at its
		// minimum (one read) or maximum.
		minBytes, maxBytes bool
	}{
		{
			name:      "disk",
			latency:   2600 * time.Microsecond,
			delivered: 10 << 20, // 100 MiB/s
		},
		{
			name:      "object store",
			latency:   40 * time.Millisecond,
			delivered: 64 << 20, // 640 MiB/s
		},
		{
			// The first two windows, which hold whole reads, leave the
			// bandwidth after aplBandwidthWindows more windows.
			name:      "slower than one read",
			latency:   100 * time.Millisecond,
			delivered: 64 << 10, // 640 KiB/s
			windows:   aplBandwidthWindows + 1,
			minBytes:  true,
		},
		{
			name:      "faster than the pages file allows",
			latency:   time.Millisecond,
			delivered: 4 << 30, // 40 GiB/s
			maxBytes:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b aplBackgroundBudget
			b.init(0, testBudgetRead, testBudgetMax, 0)
			// The first read measures the latency and ends the first window;
			// a read that queued ends the slow start.
			now := int64(tc.latency)
			b.readCompleted(now, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
			b.update(now)
			b.readCompleted(now, testBudgetRead, now-2*int64(tc.latency)-int64(aplSlowStartMaxDelay), aplReadFullBackground, testBudgetRead)
			// Reads deliver tc.delivered bytes in each of the next windows.
			for range max(tc.windows, 1) {
				now += int64(max(window, tc.latency))
				b.readCompleted(now, tc.delivered, now-int64(tc.latency), aplReadBackground, testBudgetRead)
				b.update(now)
			}

			// The first window delivered one read in tc.latency, the second
			// one read more and tc.delivered, and the others tc.delivered.
			window := max(window, tc.latency)
			bytes := []uint64{testBudgetRead, testBudgetRead + tc.delivered}
			durations := []time.Duration{tc.latency, window}
			for range max(tc.windows, 1) - 1 {
				bytes = append(bytes, tc.delivered)
				durations = append(durations, window)
			}
			if n := len(bytes) - aplBandwidthWindows; n > 0 {
				bytes, durations = bytes[n:], durations[n:]
			}
			var sumBytes uint64
			var sumDurations time.Duration
			for i := range bytes {
				sumBytes += bytes[i]
				sumDurations += durations[i]
			}
			want := uint64(float64(sumBytes) / sumDurations.Seconds() * (tc.latency + aplQueueDelay).Seconds())
			switch {
			case tc.minBytes:
				want = testBudgetRead
			case tc.maxBytes:
				want = testBudgetMax
			}
			if b.bytes != want {
				t.Errorf("budget = %d, want %d", b.bytes, want)
			}
			if tc.minBytes != (b.bytes == testBudgetRead) || tc.maxBytes != (b.bytes == testBudgetMax) {
				t.Errorf("budget = %d is not clamped as expected (minimum %v, maximum %v)", b.bytes, tc.minBytes, tc.maxBytes)
			}
		})
	}
}

// TestBackgroundBudgetTokenBucket checks that the bandwidth of a pages file
// throttled by a token bucket, which delivers bursts above its sustained rate,
// is its sustained rate.
func TestBackgroundBudgetTokenBucket(t *testing.T) {
	const latency = 500 * time.Microsecond
	var b aplBackgroundBudget
	b.init(0, testBudgetRead, testBudgetMax, 0)
	// The first read measures the latency and ends the first window; a read
	// that queued ends the slow start.
	now := int64(latency)
	b.readCompleted(now, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
	b.update(now)
	b.readCompleted(now, testBudgetRead, now-2*int64(latency)-int64(aplSlowStartMaxDelay), aplReadFullBackground, testBudgetRead)
	// Windows alternate between a burst of 8 MiB (400 MiB/s) and nothing:
	// 200 MiB/s sustained, with the read that queued in the first window.
	for i := range aplBandwidthWindows {
		now += int64(aplBandwidthWindow)
		if i%2 == 0 {
			b.readCompleted(now, 8<<20, now-int64(latency), aplReadBackground, testBudgetRead)
		}
		b.update(now)
	}
	sustained := float64(aplBandwidthWindows/2*(8<<20)+testBudgetRead) / (aplBandwidthWindows * aplBandwidthWindow).Seconds()
	if want := uint64(sustained * (latency + aplQueueDelay).Seconds()); b.bytes != want {
		t.Errorf("budget = %d, want %d (200 MiB/s for %v)", b.bytes, want, latency+aplQueueDelay)
	}
}

// startParallel returns a budget whose slow start ended at a read issued with
// two reads in flight that took exitLatency, the first read alone having
// taken 10 ms, at 15 ms. An awaited read took 1 ms before.
func startParallel(exitLatency time.Duration) *aplBackgroundBudget {
	b := &aplBackgroundBudget{}
	b.init(0, testBudgetRead, testBudgetMax, 0)
	ms := int64(time.Millisecond)
	b.readCompleted(1*ms, hostarch.PageSize, 0, aplReadAwaited, 0)
	b.readCompleted(10*ms, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
	b.update(10 * ms)
	b.readCompleted(15*ms, testBudgetRead, 15*ms-int64(exitLatency), aplReadFullBackground, 2*testBudgetRead)
	b.update(15 * ms)
	return b
}

// TestBackgroundBudgetParallel checks that the slow start tells a pages file
// that delivers more with more reads in flight from one that serves them in
// order, by Little's law.
func TestBackgroundBudgetParallel(t *testing.T) {
	// Two reads in 15 ms deliver 33% more than one in 10 ms, more than the
	// 25% that twice as many reads must: parallel.
	if b := startParallel(15 * time.Millisecond); b.startup || !b.parallel {
		t.Errorf("two reads in flight taking 15 ms, one alone 10 ms: slow start %t, parallel %t; want false, true", b.startup, b.parallel)
	}
	// Two reads in 20 ms deliver what one does in 10 ms: in order.
	if b := startParallel(20 * time.Millisecond); b.startup || b.parallel {
		t.Errorf("two reads in flight taking 20 ms, one alone 10 ms: slow start %t, parallel %t; want false, false", b.startup, b.parallel)
	}
}

// TestBackgroundBudgetProbe checks probing after the slow start of a pages
// file that delivers more with more reads in flight: a probe that delivers
// enough more is kept and followed by one for as many reads more, one that
// does not is followed by one for half as many, and a probe for one read
// more that fails defers probing.
func TestBackgroundBudgetProbe(t *testing.T) {
	ms := int64(time.Millisecond)
	b := startParallel(15 * time.Millisecond)
	// The slow start leaves one read, and a probe for 2 starts once no
	// awaited read completed for 20 ms.
	if b.probe != 0 {
		t.Fatalf("a probe started 14 ms after an awaited read completed")
	}
	now := 21 * ms
	b.update(now)
	if b.probeFrom != 1 || b.probe != 2 || b.bytes != 2*testBudgetRead {
		t.Fatalf("after the slow start: a probe from %d reads to %d, budget %d; want from 1 to 2", b.probeFrom, b.probe, b.bytes)
	}
	// complete completes a full-size read issued with reads in flight that
	// took latency, at now + latency.
	complete := func(reads uint64, latency time.Duration) {
		now += int64(latency)
		b.readCompleted(now, testBudgetRead, now-int64(latency), aplReadFullBackground, reads*testBudgetRead)
		b.update(now)
	}
	// Two in 12 ms deliver 67% more than one in 10 ms: kept, and a probe
	// for 4 starts.
	complete(2, 12*time.Millisecond)
	complete(2, 12*time.Millisecond)
	if b.floor != 2 || b.probe != 4 {
		t.Fatalf("floor = %d, probe = %d; want 2 kept and a probe for 4", b.floor, b.probe)
	}
	// Four in 23 ms deliver 4% more than two in 12 ms: not kept; a probe for
	// 3 starts.
	complete(4, 23*time.Millisecond)
	complete(4, 23*time.Millisecond)
	if b.floor != 2 || b.probe != 3 {
		t.Fatalf("floor = %d, probe = %d; want 2 kept and a probe for 3", b.floor, b.probe)
	}
	// Three in 18 ms deliver nothing more either: probing is deferred.
	complete(3, 18*time.Millisecond)
	complete(3, 18*time.Millisecond)
	if b.floor != 2 || b.probe != 0 || b.bytes != 2*testBudgetRead || b.nextProbe <= now {
		t.Fatalf("floor = %d, probe = %d, budget %d, next probe at %v (now %v); want 2 reads and probing deferred", b.floor, b.probe, b.bytes, time.Duration(b.nextProbe), time.Duration(now))
	}
}

// TestBackgroundBudgetProbeFaultLatency checks that when an awaited read
// issued while probing raised the budget takes aplQueueDelay longer than
// awaited reads did before, or than it would take alone, the budget drops to
// the bandwidth-delay product and probing is deferred.
func TestBackgroundBudgetProbeFaultLatency(t *testing.T) {
	ms := int64(time.Millisecond)
	// An awaited read took 1 ms before the probe for 2 reads.
	b := startParallel(15 * time.Millisecond)
	b.update(21 * ms)
	if b.probe != 2 {
		t.Fatalf("probe = %d, want a probe for 2 reads", b.probe)
	}
	// A page alone takes the quickest read's 1 ms plus 0.14 ms at the rate
	// of a full-size read (256 KiB in 10 ms), more than the awaited read
	// before probing took. While probing, an awaited read of a page takes
	// 3 ms: still fine.
	b.readCompleted(25*ms, hostarch.PageSize, 22*ms, aplReadAwaited, 2*testBudgetRead)
	if b.probe != 2 {
		t.Fatalf("an awaited read taking 3 ms ended the probe")
	}
	// One that takes 4 ms ends it.
	b.readCompleted(26*ms, hostarch.PageSize, 22*ms, aplReadAwaited, 2*testBudgetRead)
	if b.probe != 0 || b.floor != 0 || b.bytes != b.bdpBytes || b.nextProbe <= 26*ms {
		t.Errorf("after an awaited read 3 ms slower: probe %d, floor %d, budget %d (bandwidth-delay product %d), next probe at %v; want the bandwidth-delay product and probing deferred", b.probe, b.floor, b.bytes, b.bdpBytes, time.Duration(b.nextProbe))
	}
}

// TestBackgroundBudgetParallelReads checks that the budget allows at least the
// reads that the pages file serves at once, as it says, from the start of the
// slow start and past its end, where the bandwidth-delay product is about one
// read.
func TestBackgroundBudgetParallelReads(t *testing.T) {
	var b aplBackgroundBudget
	b.init(0, testBudgetRead, testBudgetMax, 8)
	if want := uint64(8 * testBudgetRead); b.bytes != want {
		t.Fatalf("initial budget = %d, want %d: the 8 reads that the pages file serves at once", b.bytes, want)
	}
	// A read alone takes 30 ms; one issued with the 8 in flight takes 50 ms,
	// which ends the slow start with a bandwidth-delay product of one read.
	ms := int64(time.Millisecond)
	b.readCompleted(30*ms, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
	b.update(30 * ms)
	b.readCompleted(50*ms, testBudgetRead, 0, aplReadFullBackground, 8*testBudgetRead)
	if b.startup || !b.parallel {
		t.Fatalf("slow start %t, parallel %t; want false, true", b.startup, b.parallel)
	}
	if b.bdpBytes >= 2*testBudgetRead || b.bytes != 8*testBudgetRead {
		t.Errorf("after the slow start: bandwidth-delay product %d, budget %d; want less than 2 reads, and 8 reads", b.bdpBytes, b.bytes)
	}
	// More than the reads it can have in flight are not allowed.
	b.init(0, testBudgetRead, testBudgetMax, 1000)
	if b.bytes != testBudgetMax {
		t.Errorf("initial budget of a pages file that serves 1000 reads at once = %d, want the maximum %d", b.bytes, uint64(testBudgetMax))
	}
}

// TestBackgroundBudgetParallelReadsProbeFaultLatency checks that while a probe
// raises the budget of a pages file that serves reads in parallel, an awaited
// read is slowed by the reads in flight only if it takes aplQueueDelay longer
// than it would alone, the latency of the quickest read plus its bytes at the
// rate of a full-size read; that one that was drops the budget to the
// bandwidth-delay product; and that the reads that the pages file serves at
// once return once the backoff is over.
func TestBackgroundBudgetParallelReadsProbeFaultLatency(t *testing.T) {
	ms := int64(time.Millisecond)
	var b aplBackgroundBudget
	b.init(0, testBudgetRead, testBudgetMax, 8)
	b.readCompleted(30*ms, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
	b.update(30 * ms)
	b.readCompleted(50*ms, testBudgetRead, 0, aplReadFullBackground, 8*testBudgetRead)
	b.update(50 * ms)
	if !b.raised || b.probe == 0 {
		t.Fatalf("raised %t, probe %d: want the budget raised by a probe above the 8 reads", b.raised, b.probe)
	}
	// A page read in 21 ms, mostly the store's latency to a first byte,
	// against 0.47 ms at the rate of a full-size read: not slowed, since no
	// read was quicker. Another in 23 ms is within aplQueueDelay of it.
	b.readCompleted(71*ms, hostarch.PageSize, 50*ms, aplReadAwaited, 8*testBudgetRead)
	b.readCompleted(95*ms, hostarch.PageSize, 72*ms, aplReadAwaited, 8*testBudgetRead)
	if b.drops != 0 || b.floor != 8 {
		t.Fatalf("awaited reads of 21 and 23 ms, the quickest read 21 ms: %d drops, floor %d; want none and 8", b.drops, b.floor)
	}
	// One in 24 ms was slowed.
	now := 120 * ms
	b.readCompleted(now, hostarch.PageSize, now-24*ms, aplReadAwaited, 8*testBudgetRead)
	if b.drops != 1 || b.floor != 0 || b.bytes != b.bdpBytes {
		t.Fatalf("after an awaited read of 24 ms: %d drops, floor %d, budget %d (bandwidth-delay product %d); want the bandwidth-delay product", b.drops, b.floor, b.bytes, b.bdpBytes)
	}
	b.update(b.nextProbe - 1)
	if b.floor != 0 {
		t.Errorf("floor = %d before the backoff is over, want 0", b.floor)
	}
	b.update(b.nextProbe)
	if b.floor != 8 || b.bytes < 8*testBudgetRead {
		t.Errorf("once the backoff is over: floor %d, budget %d; want 8 reads", b.floor, b.bytes)
	}
}

// TestBackgroundBudgetGapBytes checks that a background read reads through gaps
// as long as a full-size read until one completed, then through gaps as long as
// what a full-size read delivers in the latency of the quickest read.
func TestBackgroundBudgetGapBytes(t *testing.T) {
	var b aplBackgroundBudget
	b.init(0, testBudgetRead, testBudgetMax, 0)
	ms := int64(time.Millisecond)
	// A short read completes first, in 1 ms.
	b.readCompleted(1*ms, hostarch.PageSize, 0, aplReadBackground, hostarch.PageSize)
	if got := b.gapBytes(); got != testBudgetRead {
		t.Errorf("gapBytes() before a full-size read completed = %d, want %d", got, testBudgetRead)
	}
	// A full-size read takes 8 ms: it delivers an eighth of its bytes in the
	// 1 ms that the short read took.
	b.readCompleted(8*ms, testBudgetRead, 0, aplReadFullBackground, testBudgetRead)
	if got, want := b.gapBytes(), uint64(testBudgetRead/8); got != want {
		t.Errorf("gapBytes() = %d, want %d", got, want)
	}
	// A quicker awaited read lowers it, to whole pages.
	b.readCompleted(9*ms, hostarch.PageSize, 9*ms-ms/3, aplReadAwaited, 0)
	if got, want := b.gapBytes(), hostarch.PageRoundDown(uint64(testBudgetRead/24)); got != want {
		t.Errorf("gapBytes() after a read of 1/3 ms = %d, want %d", got, want)
	}
}

// startParallelReads returns a budget for a pages file that serves reads
// reads at once, as many as loading may have in flight, so that no probe
// raises the budget above them, whose slow start ended at a full-size read
// issued with them in flight, 1.2 s after loading started: the pages file's
// rate per read, about one read's bandwidth-delay product.
func startParallelReads(t *testing.T, reads uint64) *aplBackgroundBudget {
	t.Helper()
	b := &aplBackgroundBudget{}
	b.init(0, testBudgetRead, reads*testBudgetRead, reads)
	// Far more is left to load than any drain delivers.
	b.unloaded = 1 << 40
	b.readCompleted(int64(time.Second), testBudgetRead, 0, aplReadFullBackground, reads*testBudgetRead)
	b.update(int64(time.Second))
	b.readCompleted(int64(1200*time.Millisecond), testBudgetRead, 0, aplReadFullBackground, reads*testBudgetRead)
	b.update(int64(1200 * time.Millisecond))
	if b.startup || b.floor != reads || b.bytes != reads*testBudgetRead {
		t.Fatalf("after the slow start: slow start %t, floor %d, budget %d; want false, %d reads", b.startup, b.floor, b.bytes, reads)
	}
	return b
}

// testAwaited completes awaited reads of a page on b, the first issued at
// *now, one after another, each taking the next of latencies in turn, with
// inflight bytes of background reads in flight when it is issued, until done
// returns true or n reads completed, and returns the number completed.
func testAwaited(b *aplBackgroundBudget, now *int64, latencies []time.Duration, inflight func() uint64, n int, done func() bool) int {
	for i := 0; i < n; i++ {
		if done != nil && done() {
			return i
		}
		issued, bg := *now, inflight()
		*now += int64(latencies[i%len(latencies)])
		b.readCompleted(*now, hostarch.PageSize, issued, aplReadAwaited, bg)
		b.update(*now)
	}
	return n
}

// testJitter is the latency of awaited reads from an object store whose first
// bytes vary by 200 ms, whatever is in flight: 150-350 ms.
var testJitter = []time.Duration{
	310 * time.Millisecond, 170 * time.Millisecond, 250 * time.Millisecond, 150 * time.Millisecond,
	350 * time.Millisecond, 210 * time.Millisecond, 190 * time.Millisecond, 290 * time.Millisecond,
	230 * time.Millisecond, 270 * time.Millisecond, 330 * time.Millisecond, 160 * time.Millisecond,
}

// TestBackgroundBudgetParallelReadsJitter checks that awaited reads whose
// latency varies by far more than aplQueueDelay, as much with the floor's
// reads in flight as without them, keep the floor: the first judgement drains
// the reads in flight to measure awaited reads alone, after which the floor
// returns and stays. One awaited read 2 ms slower than the quickest dropped it
// for good before: restoring 3.25 GB from an S3-like store, loading then kept
// one 16 MiB read in flight and took 35-48 s rather than ~17.
func TestBackgroundBudgetParallelReadsJitter(t *testing.T) {
	const reads = 15
	b := startParallelReads(t, reads)
	now := b.windowStart
	// Background reads in flight: one read while the budget allows one,
	// else the floor's.
	inflight := func() uint64 { return min(b.bytes, reads*testBudgetRead) }
	// The first judgement finds no awaited read alone, and drains.
	testAwaited(b, &now, testJitter, inflight, aplAwaitedSamples, nil)
	if !b.draining || b.floor != 0 || b.bytes != testBudgetRead || b.drops != 0 {
		t.Fatalf("after %d awaited reads with the floor in flight: draining %t, floor %d, budget %d, %d drops; want a drain to one read", aplAwaitedSamples, b.draining, b.floor, b.bytes, b.drops)
	}
	// The drain ends once enough awaited reads were issued alone.
	testAwaited(b, &now, testJitter, inflight, aplAwaitedSamples, nil)
	if b.draining || b.floor != reads || b.bytes != reads*testBudgetRead {
		t.Fatalf("after %d awaited reads alone: draining %t, floor %d, budget %d; want the floor of %d reads back", aplAwaitedSamples, b.draining, b.floor, b.bytes, reads)
	}
	// The floor stays through 100 more.
	least := b.floor
	testAwaited(b, &now, testJitter, inflight, 100, func() bool { least = min(least, b.floor); return false })
	if least != reads || b.drops != 0 || b.drains != 1 {
		t.Errorf("over 100 more awaited reads: floor as low as %d, %d drops, %d drains; want %d reads, none, one (%v)", least, b.drops, b.drains, reads, b)
	}
}

// TestBackgroundBudgetParallelReadsSlowAwaited checks that awaited reads that
// the floor's reads slow by more than aplAwaitedSlowdown at the median halve
// the floor at each judgement, down to one read; that the floor then grows by
// one read at each judgement while too few awaited reads are issued with
// background reads in flight to judge it by; and that it halves again once they
// are slowed.
func TestBackgroundBudgetParallelReadsSlowAwaited(t *testing.T) {
	const reads = 15
	b := startParallelReads(t, reads)
	now := b.windowStart
	// Measure awaited reads alone first: 200 ms each.
	inflight := func() uint64 { return min(b.bytes, reads*testBudgetRead) }
	alone := []time.Duration{200 * time.Millisecond}
	testAwaited(b, &now, alone, inflight, aplAwaitedSamples, nil)
	testAwaited(b, &now, alone, inflight, aplAwaitedSamples, func() bool { return !b.draining })
	if b.draining || b.floor != reads {
		t.Fatalf("after the drain: draining %t, floor %d; want the floor of %d reads back", b.draining, b.floor, reads)
	}
	// With more than one background read in flight, awaited reads take 30%
	// longer than alone, beyond aplQueueDelay: 262 ms.
	slowed := []time.Duration{262 * time.Millisecond}
	inflight = func() uint64 {
		if b.floor > 1 {
			return b.floor * testBudgetRead
		}
		return testBudgetRead
	}
	for _, want := range []uint64{7, 3, 1} {
		testAwaited(b, &now, slowed, inflight, aplAwaitedSamples, nil)
		if b.floor != want || b.floorReads != want {
			t.Fatalf("floor %d (%d allowed), want %d", b.floor, b.floorReads, want)
		}
	}
	// One read alone: nothing to judge the floor by, which grows by one
	// read; with two, awaited reads are slowed again, and it halves.
	testAwaited(b, &now, alone, inflight, aplAwaitedSamples, nil)
	if b.floor != 2 {
		t.Fatalf("floor %d after %d awaited reads alone, want 2", b.floor, aplAwaitedSamples)
	}
	testAwaited(b, &now, slowed, inflight, aplAwaitedSamples, nil)
	if b.floor != 1 {
		t.Fatalf("floor %d after %d awaited reads slowed by 2 reads in flight, want 1", b.floor, aplAwaitedSamples)
	}
	// Awaited reads 20% slower than alone, plus aplQueueDelay, let it grow
	// by one read at each judgement.
	within := []time.Duration{242 * time.Millisecond}
	testAwaited(b, &now, alone, inflight, aplAwaitedSamples, nil)
	for want := uint64(3); want <= reads; want++ {
		testAwaited(b, &now, within, inflight, aplAwaitedSamples, nil)
		if b.floor != want {
			t.Fatalf("floor %d, want %d", b.floor, want)
		}
	}
	testAwaited(b, &now, within, inflight, aplAwaitedSamples, nil)
	if b.floor != reads || b.drains != 1 {
		t.Errorf("floor %d, %d drains; want %d reads and one drain", b.floor, b.drains, reads)
	}
}

// TestBackgroundBudgetParallelReadsQuiet checks that the floor that awaited
// reads lowered grows by one read each time no awaited read completed for the
// minimum latency of a background read.
func TestBackgroundBudgetParallelReadsQuiet(t *testing.T) {
	const reads = 15
	b := startParallelReads(t, reads)
	now := b.windowStart
	b.alone = [aplAwaitedSamples]time.Duration{}
	b.aloneCount = aplAwaitedSamples
	for i := range b.alone {
		b.alone[i] = 100 * time.Millisecond
	}
	testAwaited(b, &now, []time.Duration{500 * time.Millisecond}, func() uint64 { return b.floor * testBudgetRead }, aplAwaitedSamples, nil)
	if b.floor != reads/2 {
		t.Fatalf("floor %d after awaited reads 5 times slower than alone, want %d", b.floor, reads/2)
	}
	start := now
	for ; now < start+int64(5*b.minLatency); now += int64(aplBandwidthWindow) {
		b.update(now)
	}
	if want := uint64(reads/2 + 4); b.floor != want {
		t.Errorf("floor %d after 5 minimum latencies (%v) without awaited reads, want %d", b.floor, b.minLatency, want)
	}
}

// TestBackgroundBudgetParallelReadsDrainTimeout checks that a drain during
// which no awaited read was issued alone ends after twice the minimum latency
// and the time the awaited reads of a judgement take, restoring the floor,
// and that the next drain waits for a backoff, during which the floor is kept.
func TestBackgroundBudgetParallelReadsDrainTimeout(t *testing.T) {
	const reads = 15
	b := startParallelReads(t, reads)
	now := b.windowStart
	loaded := func() uint64 { return reads * testBudgetRead }
	latency := []time.Duration{100 * time.Millisecond}
	testAwaited(b, &now, latency, loaded, aplAwaitedSamples, nil)
	if !b.draining {
		t.Fatalf("no drain after %d awaited reads with the floor in flight", aplAwaitedSamples)
	}
	// The awaited reads' latency, less their bytes at the rate of a full-size
	// read, is at most theirs.
	if least, most := now+int64(2*b.minLatency), now+int64(2*b.minLatency+aplAwaitedSamples*latency[0]); b.drainEnd < least || b.drainEnd > most {
		t.Fatalf("drain ends at %v, want within [%v, %v]", time.Duration(b.drainEnd), time.Duration(least), time.Duration(most))
	}
	b.update(b.drainEnd - 1)
	if !b.draining {
		t.Fatalf("drain ended before its end")
	}
	now = b.drainEnd
	b.update(now)
	if b.draining || b.floor != reads || b.nextDrain <= now {
		t.Fatalf("after the drain's end: draining %t, floor %d, next drain at %v (now %v); want the floor back and the next drain deferred", b.draining, b.floor, time.Duration(b.nextDrain), time.Duration(now))
	}
	testAwaited(b, &now, latency, loaded, aplAwaitedSamples, func() bool { return now >= b.nextDrain })
	if b.draining || b.floor != reads {
		t.Errorf("before the backoff is over: draining %t, floor %d; want no drain and %d reads", b.draining, b.floor, reads)
	}
	now = b.nextDrain
	testAwaited(b, &now, latency, loaded, aplAwaitedSamples, nil)
	if !b.draining || b.drains != 2 {
		t.Errorf("after the backoff: draining %t, %d drains; want a second drain", b.draining, b.drains)
	}
}

// TestBackgroundBudgetParallelReadsNoDrainNearEnd checks that no drain starts
// while less is left to load than the pages file delivers over the drain,
// where loading would end within it at one read: the floor stays.
func TestBackgroundBudgetParallelReadsNoDrainNearEnd(t *testing.T) {
	const reads = 15
	b := startParallelReads(t, reads)
	now := b.windowStart
	loaded := func() uint64 { return reads * testBudgetRead }
	latency := []time.Duration{100 * time.Millisecond}
	// A drain takes at least twice the minimum latency, 2 s, over which the
	// pages file delivers twice its bandwidth's bytes, more than are left.
	if b.bandwidth() == 0 {
		t.Fatalf("no bandwidth measured after the slow start")
	}
	b.unloaded = uint64(b.bandwidth())
	testAwaited(b, &now, latency, loaded, 3*aplAwaitedSamples, nil)
	if b.draining || b.drains != 0 || b.floor != reads {
		t.Fatalf("with %d bytes left at %.0f bytes/s: draining %t, %d drains, floor %d; want no drain and %d reads", b.unloaded, b.bandwidth(), b.draining, b.drains, b.floor, reads)
	}
	// With more left, the next judgement drains.
	b.unloaded = 1 << 40
	testAwaited(b, &now, latency, loaded, aplAwaitedSamples, nil)
	if !b.draining || b.drains != 1 {
		t.Errorf("with %d bytes left: draining %t, %d drains; want a drain", b.unloaded, b.draining, b.drains)
	}
}
