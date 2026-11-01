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

package mm

import (
	"runtime"
	"strings"

	"gvisor.dev/gvisor/pkg/goid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// A measurement build's view of how the Sentry's own accesses to application
// memory await async page loading: an internal mapping of a pma that is
// cached maps, and so awaits, the whole pma, with mm.activeMu locked for
// writing.

// minLoggedInternalMapping is the smallest pma whose whole internal mapping is
// logged while pgalloc.LoggingAwaitedLoads().
const minLoggedInternalMapping = 1 << 20

// logWholeInternalMapping logs, while pgalloc.LoggingAwaitedLoads(), the
// internal mapping of the whole of pseg, if it is at least
// minLoggedInternalMapping long, for an access to ar: its size, its file, the
// calling goroutine and its stack.
func logWholeInternalMapping(pseg pmaIterator, ar hostarch.AddrRange) {
	if !pgalloc.LoggingAwaitedLoads() || pseg.Range().Length() < minLoggedInternalMapping {
		return
	}
	pma := pseg.ValuePtr()
	loading := false
	if mf, ok := pma.file.(*pgalloc.MemoryFile); ok {
		loading = mf.IsAsyncLoading()
	}
	log.Infof("Internal mapping of a whole %d-byte pma %v (%T %v, async loading %t) for %d bytes at %v; goid %d: %s", pseg.Range().Length(), pseg.Range(), pma.file, pseg.fileRange(), loading, ar.Length(), ar, goid.Get(), callers())
}

// callers returns the functions that called logWholeInternalMapping's caller,
// innermost first, in gVisor's packages without their common prefix.
func callers() string {
	var pcs [24]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs[:])])
	var b strings.Builder
	for {
		frame, more := frames.Next()
		if b.Len() != 0 {
			b.WriteString(" < ")
		}
		b.WriteString(strings.TrimPrefix(frame.Function, "gvisor.dev/gvisor/pkg/"))
		if !more {
			return b.String()
		}
	}
}
