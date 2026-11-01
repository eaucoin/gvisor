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

package s3

import (
	"context"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gvisor.dev/gvisor/pkg/sync"
)

const (
	// hedgeSamples is the number of past times to a first byte of answer that
	// a hedger's delay is the percentile of.
	hedgeSamples = 512

	// hedgeMinSamples is the number of them that it needs before it hedges.
	hedgeMinSamples = 20

	// hedgeMaxFraction is the largest fraction of GETs that are hedged, as
	// "The Tail at Scale" (Dean and Barroso, 2013) bounds the load of hedged
	// requests at its 95th-percentile delay.
	hedgeMaxFraction = 0.05
)

// hedger decides when a GET of a pages file is made again on another
// connection: when the store has not begun to answer it within a percentile of
// the times it took to begin answering the GETs before, at least twice the
// shortest of them, for at most hedgeMaxFraction of GETs.
type hedger struct {
	percentile int

	mu sync.Mutex
	// samples holds the last hedgeSamples times to the beginning of an answer,
	// in a ring indexed by next; n is the number held.
	samples  [hedgeSamples]time.Duration
	next     int
	n        int
	quickest time.Duration
	// gets and hedges count the GETs made, and those made again.
	gets   uint64
	hedges uint64
}

func newHedger(percentile int) *hedger {
	return &hedger{percentile: percentile}
}

// record records that the store began to answer a GET in d.
func (h *hedger) record(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples[h.next] = d
	h.next = (h.next + 1) % hedgeSamples
	h.n = min(h.n+1, hedgeSamples)
	if h.quickest == 0 || d < h.quickest {
		h.quickest = d
	}
}

// begin counts a GET and returns the delay after which it may be hedged, or
// false if it may not be.
func (h *hedger) begin() (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gets++
	if h.n < hedgeMinSamples {
		return 0, false
	}
	sorted := slices.Clone(h.samples[:h.n])
	slices.Sort(sorted)
	d := sorted[min(h.n-1, h.n*h.percentile/100)]
	return max(d, 2*h.quickest), true
}

// allow returns true, and counts a hedge, if hedging one more GET keeps the
// hedges within hedgeMaxFraction of the GETs.
func (h *hedger) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if float64(h.hedges+1) > hedgeMaxFraction*float64(h.gets) {
		return false
	}
	h.hedges++
	return true
}

// counts returns the GETs made and hedged so far.
func (h *hedger) counts() (gets, hedges uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets, h.hedges
}

// getResult is the outcome of one GET of a hedged read.
type getResult struct {
	out   *s3.GetObjectOutput
	err   error
	hedge bool
	took  time.Duration
}

// get makes the GET in, hedged as h says (h may be nil): the first answer
// that begins is returned, with the function that cancels its request once
// its body is read, and whether the GET was hedged and the hedge answered
// first. The other request is canceled, and its answer's body closed.
func (h *hedger) get(ctx context.Context, client *s3.Client, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (out *s3.GetObjectOutput, cancel context.CancelFunc, hedged, hedgeWon bool, err error) {
	results := make(chan getResult, 2)
	// cancels[0] cancels the first request, cancels[1] the hedge.
	var cancels [2]context.CancelFunc
	start := func(hedge bool) {
		gctx, gcancel := context.WithCancel(ctx)
		if hedge {
			cancels[1] = gcancel
		} else {
			cancels[0] = gcancel
		}
		begun := time.Now()
		go func() {
			out, err := client.GetObject(gctx, in, optFns...)
			results <- getResult{out: out, err: err, hedge: hedge, took: time.Since(begun)}
		}()
	}
	delay, mayHedge := time.Duration(0), false
	if h != nil {
		delay, mayHedge = h.begin()
	}
	start(false)
	pending := 1
	var timer <-chan time.Time
	if mayHedge {
		t := time.NewTimer(delay)
		defer t.Stop()
		timer = t.C
	}
	for {
		select {
		case <-timer:
			timer = nil
			if h.allow() {
				hedged = true
				pending++
				start(true)
			}
		case res := <-results:
			pending--
			own, other := cancels[0], cancels[1]
			if res.hedge {
				own, other = other, own
			}
			if res.err != nil {
				own()
				if pending > 0 {
					// The other request may still be answered.
					timer = nil
					continue
				}
				return nil, nil, hedged, false, res.err
			}
			if h != nil {
				h.record(res.took)
			}
			if pending > 0 {
				other()
				go func() {
					if loser := <-results; loser.out != nil {
						loser.out.Body.Close()
					}
				}()
			}
			return res.out, own, hedged, res.hedge, nil
		}
	}
}
