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

package tmpfs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

// newDiskBackedFile creates a file in a new tmpfs mount whose data is on mf,
// and returns its FD, released at the end of the test.
func newDiskBackedFile(t *testing.T, ctx context.Context, mf *pgalloc.MemoryFile) *vfs.FileDescription {
	t.Helper()
	creds := auth.CredentialsFromContext(ctx)
	vfsObj := &vfs.VirtualFilesystem{}
	if err := vfsObj.Init(ctx); err != nil {
		t.Fatalf("VFS init: %v", err)
	}
	vfsObj.MustRegisterFilesystemType("tmpfs", FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	mntns, err := vfsObj.NewMountNamespace(ctx, creds, "", "tmpfs", &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			InternalData: FilesystemOpts{MemoryFile: mf},
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	t.Cleanup(func() { mntns.DecRef(ctx) })
	root := mntns.Root(ctx)
	t.Cleanup(func() { root.DecRef(ctx) })
	fd, err := vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse("file"),
	}, &vfs.OpenOptions{
		Flags: linux.O_RDWR | linux.O_CREAT | linux.O_EXCL,
		Mode:  linux.ModeRegular | 0644,
	})
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	t.Cleanup(func() { fd.DecRef(ctx) })
	return fd
}

// TestDirtyTrackingDiskBackedWrite checks that writes to tmpfs files whose
// data is on a disk-backed MemoryFile, which tmpfs writes through the
// MemoryFile's FD rather than through MapInternal, mark the pages they write
// dirty: verification finds no escape, unless the mark is disabled.
func TestDirtyTrackingDiskBackedWrite(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			ctx := contexttest.Context(t)
			file, err := os.CreateTemp(t.TempDir(), "tmpfs-dirty-test")
			if err != nil {
				t.Fatalf("CreateTemp: %v", err)
			}
			mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
				DiskBackedFile:          true,
				DisableMemoryAccounting: true,
			})
			if err != nil {
				file.Close()
				t.Fatalf("NewMemoryFile: %v", err)
			}
			t.Cleanup(mf.Destroy)
			fd := newDiskBackedFile(t, ctx, mf)
			if !mf.IsDiskBacked() {
				t.Fatalf("MemoryFile is not disk-backed")
			}

			// The file's first pages are written in one epoch, and one of
			// them again in the next: both allocate nothing through
			// MapInternal.
			write := func(off int64, data []byte) {
				t.Helper()
				if _, err := fd.PWrite(ctx, usermem.BytesIOSequence(data), off, vfs.WriteOptions{}); err != nil {
					t.Fatalf("PWrite: %v", err)
				}
			}
			endEpoch := func() *pgalloc.DirtyVerification {
				t.Helper()
				if err := mf.SaveTo(ctx, io.Discard, &pgalloc.SaveOpts{}); err != nil {
					t.Fatalf("SaveTo: %v", err)
				}
				v, err := mf.VerifyDirty(mf.SwapDirty(true /* paused */))
				if err != nil {
					t.Fatalf("VerifyDirty: %v", err)
				}
				v.Commit()
				return v
			}
			mf.EnableDirtyTracking()
			if err := mf.RecordPageHashes(); err != nil {
				t.Fatalf("RecordPageHashes: %v", err)
			}
			if disabled {
				pgalloc.TestOnlyDisableDirtyMarkPath(pgalloc.DirtyMarkTmpfsWrite)
				t.Cleanup(func() { pgalloc.TestOnlyDisableDirtyMarkPath(pgalloc.DirtyMarkNone) })
			}
			write(0, bytes.Repeat([]byte{'a'}, 3*hostarch.PageSize))
			want := uint64(0)
			if disabled {
				want = 3
			}
			if v := endEpoch(); v.Escapes != want {
				t.Errorf("first write: %d escapes %v, want %d", v.Escapes, v.First, want)
			}
			write(hostarch.PageSize+10, []byte("rewritten"))
			if disabled {
				want = 1
			}
			if v := endEpoch(); v.Escapes != want {
				t.Errorf("second write: %d escapes %v, want %d", v.Escapes, v.First, want)
			}
		})
	}
}
