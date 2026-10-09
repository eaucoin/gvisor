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
	b.init(0, testBudgetRead, testBudgetMax)
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
		b.init(0, testBudgetRead, testBudgetMax)
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
			b.init(0, testBudgetRead, testBudgetMax)
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
	b.init(0, testBudgetRead, testBudgetMax)
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
	b.init(0, testBudgetRead, testBudgetMax)
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
// awaited reads did before, or than its bytes take at the rate of one
// full-size read alone, the budget drops to the bandwidth-delay product and
// probing is deferred.
func TestBackgroundBudgetProbeFaultLatency(t *testing.T) {
	ms := int64(time.Millisecond)
	// An awaited read took 1 ms before the probe for 2 reads.
	b := startParallel(15 * time.Millisecond)
	b.update(21 * ms)
	if b.probe != 2 {
		t.Fatalf("probe = %d, want a probe for 2 reads", b.probe)
	}
	// A page takes 0.16 ms at the rate of one read alone (256 KiB in 10
	// ms), less than an awaited read took before probing. While probing,
	// an awaited read of a page takes 2 ms: still fine.
	b.readCompleted(24*ms, hostarch.PageSize, 22*ms, aplReadAwaited, 2*testBudgetRead)
	if b.probe != 2 {
		t.Fatalf("an awaited read taking 2 ms ended the probe")
	}
	// One that takes 3 ms ends it.
	b.readCompleted(25*ms, hostarch.PageSize, 22*ms, aplReadAwaited, 2*testBudgetRead)
	if b.probe != 0 || b.floor != 0 || b.bytes != b.bdpBytes || b.nextProbe <= 25*ms {
		t.Errorf("after an awaited read 6 ms slower: probe %d, floor %d, budget %d (bandwidth-delay product %d), next probe at %v; want the bandwidth-delay product and probing deferred", b.probe, b.floor, b.bytes, b.bdpBytes, time.Duration(b.nextProbe))
	}
}
