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

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
)

// failingPrefix names the files whose fsyncs and flushes failingNode fails.
const failingPrefix = "failing-"

// failingNode is a node of a loopback FUSE filesystem that fails every fsync
// and every flush with EIO, as a remote filesystem that commits written data
// asynchronously reports a commit that failed.
type failingNode struct {
	fs.LoopbackNode
}

var _ = (fs.NodeFsyncer)((*failingNode)(nil))
var _ = (fs.NodeFlusher)((*failingNode)(nil))

// Fsync implements fs.NodeFsyncer.Fsync.
func (*failingNode) Fsync(context.Context, fs.FileHandle, uint32) syscall.Errno {
	return syscall.EIO
}

// Flush implements fs.NodeFlusher.Flush.
func (*failingNode) Flush(context.Context, fs.FileHandle) syscall.Errno {
	return syscall.EIO
}

// mountFailingFUSE mounts at mnt a loopback FUSE filesystem of a new
// directory, whose files named with failingPrefix fail their fsyncs and
// flushes, and returns the directory.
func mountFailingFUSE(t *testing.T, mnt string) string {
	t.Helper()
	backing := t.TempDir()
	// Writable by the sandbox's processes, which run as another user.
	if err := os.Chmod(backing, 0777); err != nil {
		t.Fatalf("chmod %q: %v", backing, err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(backing, &st); err != nil {
		t.Fatalf("stat %q: %v", backing, err)
	}
	root := &fs.LoopbackRoot{
		Path: backing,
		Dev:  uint64(st.Dev),
		NewNode: func(rootData *fs.LoopbackRoot, _ *fs.Inode, name string, _ *syscall.Stat_t) fs.InodeEmbedder {
			if strings.HasPrefix(name, failingPrefix) {
				return &failingNode{fs.LoopbackNode{RootData: rootData}}
			}
			return &fs.LoopbackNode{RootData: rootData}
		},
	}
	// Mounted by mount(2), as the test runs as root, with no fusermount.
	opts := &fs.Options{MountOptions: fuse.MountOptions{AllowOther: true, Name: "failing", DirectMountStrict: true}}
	server, err := fs.Mount(mnt, &fs.LoopbackNode{RootData: root}, opts)
	if err != nil {
		t.Fatalf("mounting FUSE at %q: %v", mnt, err)
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Errorf("unmounting FUSE at %q: %v", mnt, err)
		}
	})

	// Outside the sandbox, the failing file's fsync and close fail.
	name := filepath.Join(mnt, failingPrefix+"host")
	f, err := os.Create(name)
	if err != nil {
		t.Fatalf("creating %q: %v", name, err)
	}
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("writing %q: %v", name, err)
	}
	if err := f.Sync(); err == nil {
		t.Fatalf("fsync of %q succeeded on the host", name)
	}
	if err := f.Close(); err == nil {
		t.Fatalf("close of %q succeeded on the host", name)
	}
	return backing
}

// TestFUSEWriteErrors checks that a write a FUSE filesystem fails to commit,
// as it says on fsync(2) or on the flush close(2) makes, fails fsync(2) and
// close(2) inside the sandbox too, whether the bind mount's source is the FUSE
// filesystem itself or a directory it is mounted in, whose file handles the
// gofer shares (https://github.com/google/gvisor/issues/16043).
func TestFUSEWriteErrors(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("FUSE is not available: %v", err)
	}
	for _, tc := range []struct {
		name string
		// The FUSE filesystem's path in the bind mount's source.
		inSource string
	}{
		{name: "source", inSource: "."},
		{name: "submount", inSource: "fuse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := os.MkdirTemp(testutil.TmpDir(), "write-errors")
			if err != nil {
				t.Fatalf("MkdirTemp: %v", err)
			}
			// Removed once the FUSE filesystem is unmounted.
			t.Cleanup(func() { os.RemoveAll(source) })
			mnt := filepath.Join(source, tc.inSource)
			for _, dir := range []string{source, mnt} {
				if err := os.MkdirAll(dir, 0777); err != nil {
					t.Fatalf("MkdirAll(%q): %v", dir, err)
				}
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatalf("chmod %q: %v", dir, err)
				}
			}
			backing := mountFailingFUSE(t, mnt)

			conf := testutil.TestConfig(t)
			conf.FileAccessMounts = config.FileAccessShared
			spec := testutil.NewSpecWithArgs("sleep", "1000")
			// Without the spec's mount of the test's temporary directory,
			// which is not recursive: the gofer mounts the source in it, where
			// the directory the FUSE filesystem is mounted on shows instead.
			var mounts []specs.Mount
			for _, m := range spec.Mounts {
				if m.Destination != testutil.TmpDir() {
					mounts = append(mounts, m)
				}
			}
			spec.Mounts = mounts
			// At the same path in the sandbox, whose root is the host's, so
			// that its mount point is there; recursive, as container runtimes
			// bind volumes, so that a FUSE filesystem mounted in the source is
			// in the mount too.
			spec.Mounts = append(spec.Mounts, specs.Mount{
				Type:        "bind",
				Source:      source,
				Destination: source,
				Options:     []string{"rbind"},
			})
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()
			c, err := New(conf, Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir})
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer c.Destroy()
			if err := c.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}

			for _, write := range []struct {
				file    string
				options []string
				fails   bool
			}{
				// Written, fsynced and closed whole.
				{file: "committed", options: []string{"conv=fsync"}, fails: false},
				// Failed by its fsync.
				{file: failingPrefix + "fsync", options: []string{"conv=fsync"}, fails: true},
				// Failed by its close's flush alone, as it is never fsynced.
				{file: failingPrefix + "close", fails: true},
			} {
				args := append([]string{"if=/dev/zero", "of=" + filepath.Join(mnt, write.file), "bs=4096", "count=1"}, write.options...)
				out, status, err := executeCombinedOutputWithStatus(conf, c, nil, "/bin/dd", args...)
				if err != nil {
					t.Fatalf("executing dd %v: %v", args, err)
				}
				if failed := status != 0; failed != write.fails {
					t.Errorf("dd %v exited with %d, want it to fail: %t; output: %s", args, status, write.fails, out)
				}
			}
			// The writes went to the FUSE filesystem.
			committed := filepath.Join(backing, "committed")
			if st, err := os.Stat(committed); err != nil || st.Size() != 4096 {
				t.Errorf("stat %q: %v, %v; want a file of 4096 bytes", committed, st, err)
			}
		})
	}
}
