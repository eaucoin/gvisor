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
	b.readCompleted(1*ms, testBudgetRead, 0, true)
	b.readCompleted(1*ms, testBudgetRead, 0, true)
	b.update(4 * ms)
	if want := uint64((aplInitialReads + 2) * testBudgetRead); b.bytes != want {
		t.Fatalf("budget after two reads without queueing = %d, want %d", b.bytes, want)
	}
	// Other reads do not grow it.
	b.readCompleted(5*ms, testBudgetRead, 4*ms, false)
	b.update(5 * ms)
	if want := uint64((aplInitialReads + 2) * testBudgetRead); b.bytes != want {
		t.Fatalf("budget after a read that is not a full-size background read = %d, want %d", b.bytes, want)
	}
	// A read that queued for more than aplSlowStartMinDelay ends the slow
	// start.
	b.readCompleted(10*ms, testBudgetRead, 1*ms, true)
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
		b.readCompleted(now, testBudgetRead, 0, true)
		b.update(now)
		b.readCompleted(now, testBudgetRead, now-int64(tc.minLatency+tc.queued), true)
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
			b.readCompleted(now, testBudgetRead, 0, true)
			b.update(now)
			b.readCompleted(now, testBudgetRead, now-2*int64(tc.latency)-int64(aplSlowStartMaxDelay), true)
			// Reads deliver tc.delivered bytes in each of the next windows.
			for range max(tc.windows, 1) {
				now += int64(max(window, tc.latency))
				b.readCompleted(now, tc.delivered, now-int64(tc.latency), false)
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
	b.readCompleted(now, testBudgetRead, 0, true)
	b.update(now)
	b.readCompleted(now, testBudgetRead, now-2*int64(latency)-int64(aplSlowStartMaxDelay), true)
	// Windows alternate between a burst of 8 MiB (400 MiB/s) and nothing:
	// 200 MiB/s sustained, with the read that queued in the first window.
	for i := range aplBandwidthWindows {
		now += int64(aplBandwidthWindow)
		if i%2 == 0 {
			b.readCompleted(now, 8<<20, now-int64(latency), false)
		}
		b.update(now)
	}
	sustained := float64(aplBandwidthWindows/2*(8<<20)+testBudgetRead) / (aplBandwidthWindows * aplBandwidthWindow).Seconds()
	if want := uint64(sustained * (latency + aplQueueDelay).Seconds()); b.bytes != want {
		t.Errorf("budget = %d, want %d (200 MiB/s for %v)", b.bytes, want, latency+aplQueueDelay)
	}
}
