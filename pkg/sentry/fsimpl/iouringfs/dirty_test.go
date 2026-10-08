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

package iouringfs

import (
	"fmt"
	"io"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// TestDirtyTrackingRings checks that the writes the Sentry makes to an
// io_uring's rings through the mappings it keeps, which bypass MapInternal,
// are dirty at every swap: verification finds no escape, unless the rings'
// registration as always dirty is disabled.
func TestDirtyTrackingRings(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			ctx := contexttest.Context(t)
			mf := pgalloc.MemoryFileFromContext(ctx)
			if disabled {
				pgalloc.TestOnlyDisableDirtyMarkPath(pgalloc.DirtyMarkIOUring)
				t.Cleanup(func() { pgalloc.TestOnlyDisableDirtyMarkPath(pgalloc.DirtyMarkNone) })
			}
			vfsObj := &vfs.VirtualFilesystem{}
			if err := vfsObj.Init(ctx); err != nil {
				t.Fatalf("VFS init: %v", err)
			}
			vfd, err := New(ctx, vfsObj, 4, &linux.IOUringParams{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer vfd.DecRef(ctx)
			fd := vfd.Impl().(*FileDescription)

			// Start tracking once the rings exist, as when a sandbox using
			// io_uring is checkpointed.
			mf.EnableDirtyTracking()
			if err := mf.SaveTo(ctx, io.Discard, &pgalloc.SaveOpts{}); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			mf.SwapDirty(true /* paused */)
			if err := mf.RecordPageHashes(); err != nil {
				t.Fatalf("RecordPageHashes: %v", err)
			}

			// Post a completion, as ProcessSubmissions does: advance the
			// completion queue's tail through the rings' mapping.
			n := fd.ioRings.SizeBytes()
			view, err := fd.ioRingsBuf.view(n)
			if err != nil {
				t.Fatalf("view: %v", err)
			}
			atomicUint32AtOffset(view, int(linux.PreComputedIOCqRingOffsets().Tail)).Add(1)
			if _, err := fd.ioRingsBuf.writeback(n); err != nil {
				t.Fatalf("writeback: %v", err)
			}

			if err := mf.SaveTo(ctx, io.Discard, &pgalloc.SaveOpts{}); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			v := mf.VerifyDirty(mf.SwapDirty(true /* paused */))
			want := uint64(0)
			if disabled {
				// The page holding the rings' header.
				want = 1
			}
			if v.Escapes != want {
				t.Errorf("%d escapes %v, want %d", v.Escapes, v.First, want)
			}
		})
	}
}
