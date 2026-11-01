// Copyright 2024 The gVisor Authors.
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

import "gvisor.dev/gvisor/pkg/atomicbitops"

// If logAwaitedLoads is true, events relevant to awaited async page loads, and
// the first touches of a working set being recorded, are logged at level Info.
// runsc sets it from --async-awaits-trace=log, before any MemoryFile loads.
var logAwaitedLoads atomicbitops.Bool

// SetLogAwaitedLoads sets whether events relevant to awaited async page loads,
// and the first touches of a working set being recorded, are logged.
func SetLogAwaitedLoads(enabled bool) {
	logAwaitedLoads.Store(enabled)
}

// LoggingAwaitedLoads returns true if events relevant to awaited async page
// loads are logged, so that their users can log what they await.
func LoggingAwaitedLoads() bool {
	return logAwaitedLoads.Load()
}
