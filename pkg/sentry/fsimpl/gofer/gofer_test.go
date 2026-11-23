// Copyright 2020 The gVisor Authors.
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

package gofer

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

func TestDestroyIdempotent(t *testing.T) {
	ctx := contexttest.Context(t)
	fs := filesystem{
		mf:         pgalloc.MemoryFileFromContext(ctx),
		inoByKey:   make(map[inoKey]uint64),
		inodeByKey: make(map[inoKey]*inode),
		clock:      ktime.RealtimeClockFromContext(ctx),
		// There is no VFS to allocate device numbers from.
		devMinorByHostDev: map[uint64]uint32{0: 1},
		// Test relies on no dentry being held in the cache.
		dentryCache: &dentryCache{maxCachedDentries: 0},
		client:      &lisafs.Client{},
	}

	parentInode := lisafs.Inode{
		ControlFD: 1,
		Stat: lisafs.Statx{
			Mask: linux.STATX_TYPE | linux.STATX_MODE,
			Mode: linux.S_IFDIR | 0666,
		},
	}
	parent, err := fs.newLisafsDentry(ctx, &parentInode)
	if err != nil {
		t.Fatalf("fs.newDentry(): %v", err)
	}

	childInode := lisafs.Inode{
		ControlFD: 2,
		Stat: lisafs.Statx{
			Mask: linux.STATX_TYPE | linux.STATX_MODE | linux.STATX_SIZE,
			Mode: linux.S_IFREG | 0666,
			Size: 0,
		},
	}

	child, err := fs.newLisafsDentry(ctx, &childInode)
	if err != nil {
		t.Fatalf("fs.newDentry(): %v", err)
	}
	parent.opMu.Lock()
	parent.childrenMu.Lock()
	parent.cacheNewChildLocked(child, "child")
	parent.childrenMu.Unlock()
	parent.opMu.Unlock()

	fs.renameMu.Lock()
	defer fs.renameMu.Unlock()
	child.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
	if got := child.refs.Load(); got != -1 {
		t.Fatalf("child.refs=%d, want: -1", got)
	}
	// Parent will also be destroyed when child reference is removed.
	if got := parent.refs.Load(); got != -1 {
		t.Fatalf("parent.refs=%d, want: -1", got)
	}
	child.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
	child.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
}

// TestPrepareSaveRemoteMetadataOfValidatedFiles checks that a save reads the
// remote metadata of only the regular files that its restore validates, as
// vfs.PrepareSaveOptions.RestoreValidatesFile says them, and of every regular
// file without it; and that the metadata of a file that the save did not read
// cannot be validated.
func TestPrepareSaveRemoteMetadataOfValidatedFiles(t *testing.T) {
	ctx := contexttest.Context(t)
	dir := t.TempDir()
	names := []string{"validated", "unvalidated"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatalf("os.WriteFile(%q): %v", name, err)
		}
	}
	fs := filesystem{
		mf:         pgalloc.MemoryFileFromContext(ctx),
		inoByKey:   make(map[inoKey]uint64),
		inodeByKey: make(map[inoKey]*inode),
		clock:      ktime.RealtimeClockFromContext(ctx),
		// There is no VFS to allocate device numbers from.
		opts:        filesystemOptions{virtualInodeNumbers: true},
		dentryCache: &dentryCache{maxCachedDentries: 0},
		iopts:       InternalFilesystemOptions{UniqueID: checkpoint.ResourceID{ContainerName: "container", Path: "/"}},
	}
	rootFD, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatalf("unix.Open(%q): %v", dir, err)
	}
	root, err := fs.newDirectfsDentry(rootFD)
	if err != nil {
		t.Fatalf("fs.newDirectfsDentry(%q): %v", dir, err)
	}
	fs.root = root
	files := make(map[string]*dentry)
	for _, name := range names {
		fd, err := unix.Openat(rootFD, name, unix.O_PATH, 0)
		if err != nil {
			t.Fatalf("unix.Openat(%q): %v", name, err)
		}
		child, err := fs.newDirectfsDentry(fd)
		if err != nil {
			t.Fatalf("fs.newDirectfsDentry(%q): %v", name, err)
		}
		root.opMu.Lock()
		root.childrenMu.Lock()
		root.cacheNewChildLocked(child, name)
		root.childrenMu.Unlock()
		root.opMu.Unlock()
		files[name] = child
	}

	for _, tc := range []struct {
		name string
		opts vfs.PrepareSaveOptions
		// saved are the files whose remote metadata the save reads.
		saved []string
	}{
		{
			name:  "every file",
			saved: names,
		},
		{
			name: "the files the restore validates",
			opts: vfs.PrepareSaveOptions{
				RestoreValidatesFile: func(fsID checkpoint.ResourceID, p string) bool {
					if fsID != fs.iopts.UniqueID {
						t.Errorf("RestoreValidatesFile(%v, %q): want the filesystem's unique ID %v", fsID, p, fs.iopts.UniqueID)
					}
					return path.Base(p) == "validated"
				},
			},
			saved: []string{"validated"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := fs.PrepareSave(ctx, tc.opts); err != nil {
				t.Fatalf("fs.PrepareSave(): %v", err)
			}
			defer fs.BeforeResume(ctx)
			for _, name := range names {
				size, _, err := files[name].inode.savedRemoteSizeAndMtime()
				if !slices.Contains(tc.saved, name) {
					if err == nil {
						t.Errorf("%q: the save read its remote metadata (size %d), want none to validate against", name, size)
					}
					continue
				}
				if err != nil {
					t.Errorf("%q: %v, want the remote metadata that the save read", name, err)
				} else if size != uint64(len(name)) {
					t.Errorf("%q: saved remote size %d, want %d", name, size, len(name))
				}
			}
		})
	}
}

// TestPrepareSaveRunningEvictsCachedDentries checks that PrepareSaveRunning
// evicts the dentries cached as it is called and the ancestors that their
// eviction caches in turn, leaving PrepareSave none to evict in the pause,
// and that it does not destroy a cached dentry that was referenced again.
func TestPrepareSaveRunningEvictsCachedDentries(t *testing.T) {
	ctx := contexttest.Context(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0755); err != nil {
		t.Fatalf("os.MkdirAll(): %v", err)
	}
	for _, name := range []string{"a/b/c", "a/d", "e", "f"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
			t.Fatalf("os.WriteFile(%q): %v", name, err)
		}
	}
	vfsObj := &vfs.VirtualFilesystem{}
	if err := vfsObj.Init(ctx); err != nil {
		t.Fatalf("vfs.VirtualFilesystem.Init(): %v", err)
	}
	fs := &filesystem{
		mf:          pgalloc.MemoryFileFromContext(ctx),
		inoByKey:    make(map[inoKey]uint64),
		inodeByKey:  make(map[inoKey]*inode),
		clock:       ktime.RealtimeClockFromContext(ctx),
		opts:        filesystemOptions{virtualInodeNumbers: true},
		dentryCache: &dentryCache{maxCachedDentries: 100},
	}
	fs.vfsfs.Init(vfsObj, &FilesystemType{}, fs)
	rootFD, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatalf("unix.Open(%q): %v", dir, err)
	}
	root, err := fs.newDirectfsDentry(rootFD)
	if err != nil {
		t.Fatalf("fs.newDirectfsDentry(%q): %v", dir, err)
	}
	// The reference that a mount holds on its root.
	root.IncRef()
	fs.root = root
	dentries := map[string]*dentry{"": root}
	// Looked up in this order, each after its parent, as path resolution
	// would; each is cached once nothing references it: the leaves, as
	// their children reference the directories.
	for _, p := range []string{"a", "a/b", "a/b/c", "a/d", "e", "f"} {
		parent := dentries[path.Dir(p)]
		if path.Dir(p) == "." {
			parent = root
		}
		fd, err := unix.Openat(int(parent.inode.impl.(*directfsInode).controlFD), path.Base(p), unix.O_PATH, 0)
		if err != nil {
			t.Fatalf("unix.Openat(%q): %v", p, err)
		}
		d, err := fs.newDirectfsDentry(fd)
		if err != nil {
			t.Fatalf("fs.newDirectfsDentry(%q): %v", p, err)
		}
		parent.opMu.Lock()
		parent.childrenMu.Lock()
		parent.cacheNewChildLocked(d, path.Base(p))
		parent.childrenMu.Unlock()
		parent.opMu.Unlock()
		dentries[p] = d
	}
	for _, p := range []string{"a/b/c", "a/d", "e", "f"} {
		dentries[p].checkCachingLocked(ctx, false /* renameMuWriteLocked */)
		if !dentries[p].isCached() {
			t.Fatalf("%q is not cached", p)
		}
	}
	// f is referenced again, as by an open, after it was cached.
	dentries["f"].IncRef()

	fs.PrepareSaveRunning(ctx)

	fs.dentryCache.mu.Lock()
	cached := fs.dentryCache.dentriesLen
	fs.dentryCache.mu.Unlock()
	if cached != 0 {
		t.Errorf("%d dentries still cached, want none for PrepareSave to evict", cached)
	}
	for _, p := range []string{"a", "a/b", "a/b/c", "a/d", "e"} {
		if got := dentries[p].refs.Load(); got != -1 {
			t.Errorf("%q: refs %d, want -1 (destroyed)", p, got)
		}
	}
	if got := dentries["f"].refs.Load(); got != 1 {
		t.Errorf("f: refs %d, want 1 (referenced, not destroyed)", got)
	}
	if got := root.refs.Load(); got != 2 {
		t.Errorf("root: refs %d, want 2 (the mount's and f's)", got)
	}
}

// TestPrepareSaveRunningEndsWhileTasksCache checks that PrepareSaveRunning
// runs beside tasks that cache dentries as it evicts them, as tasks that look
// files up do, and ends.
func TestPrepareSaveRunningEndsWhileTasksCache(t *testing.T) {
	ctx := contexttest.Context(t)
	dir := t.TempDir()
	const files = 200
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), nil, 0644); err != nil {
			t.Fatalf("os.WriteFile(): %v", err)
		}
	}
	vfsObj := &vfs.VirtualFilesystem{}
	if err := vfsObj.Init(ctx); err != nil {
		t.Fatalf("vfs.VirtualFilesystem.Init(): %v", err)
	}
	fs := &filesystem{
		mf:          pgalloc.MemoryFileFromContext(ctx),
		inoByKey:    make(map[inoKey]uint64),
		inodeByKey:  make(map[inoKey]*inode),
		clock:       ktime.RealtimeClockFromContext(ctx),
		opts:        filesystemOptions{virtualInodeNumbers: true},
		dentryCache: &dentryCache{maxCachedDentries: 2 * files},
	}
	fs.vfsfs.Init(vfsObj, &FilesystemType{}, fs)
	rootFD, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatalf("unix.Open(%q): %v", dir, err)
	}
	root, err := fs.newDirectfsDentry(rootFD)
	if err != nil {
		t.Fatalf("fs.newDirectfsDentry(%q): %v", dir, err)
	}
	root.IncRef()
	fs.root = root
	// lookUp caches the dentry of file i, as path resolution leaves it once
	// nothing references it, unless root holds it already.
	lookUp := func(i int) error {
		name := fmt.Sprintf("f%d", i)
		root.opMu.Lock()
		root.childrenMu.Lock()
		if _, ok := root.children[name]; ok {
			root.childrenMu.Unlock()
			root.opMu.Unlock()
			return nil
		}
		fd, err := unix.Openat(rootFD, name, unix.O_PATH, 0)
		if err != nil {
			root.childrenMu.Unlock()
			root.opMu.Unlock()
			return err
		}
		d, err := fs.newDirectfsDentry(fd)
		if err != nil {
			root.childrenMu.Unlock()
			root.opMu.Unlock()
			return err
		}
		root.cacheNewChildLocked(d, name)
		root.childrenMu.Unlock()
		root.opMu.Unlock()
		d.checkCachingLocked(ctx, false /* renameMuWriteLocked */)
		return nil
	}
	for i := 0; i < files/2; i++ {
		if err := lookUp(i); err != nil {
			t.Fatalf("lookUp(%d): %v", i, err)
		}
	}

	stop := make(chan struct{})
	tasks := make(chan error, 1)
	go func() {
		for i := 0; ; i = (i + 1) % files {
			select {
			case <-stop:
				tasks <- nil
				return
			default:
			}
			if err := lookUp(i); err != nil {
				tasks <- err
				return
			}
		}
	}()
	prepared := make(chan struct{})
	go func() {
		fs.PrepareSaveRunning(ctx)
		close(prepared)
	}()
	select {
	case <-prepared:
	case <-time.After(30 * time.Second):
		t.Errorf("PrepareSaveRunning did not end within 30s while tasks cached dentries")
	}
	close(stop)
	if err := <-tasks; err != nil {
		t.Fatalf("lookUp(): %v", err)
	}
}

// treeNode is a node of the trees that TestRestoreTree restores.
type treeNode struct {
	path     string
	parent   *treeNode
	children []*treeNode
	fail     bool

	// restores counts the node's restores; parentRestored says whether its
	// parent had been restored when it was.
	restores       atomic.Int32
	parentRestored bool
}

// newTree returns the root of a tree of the given depth below it, each node
// above the leaves with width children.
func newTree(width, depth int) (*treeNode, []*treeNode) {
	root := &treeNode{}
	nodes := []*treeNode{}
	level := []*treeNode{root}
	for range depth {
		var next []*treeNode
		for _, parent := range level {
			for i := range width {
				n := &treeNode{path: path.Join(parent.path, fmt.Sprintf("%d", i)), parent: parent}
				parent.children = append(parent.children, n)
				next = append(next, n)
			}
		}
		nodes = append(nodes, next...)
		level = next
	}
	return root, nodes
}

// TestRestoreTree checks that restoreTree restores every node of a tree once,
// after its parent, as many at once as its parallelism allows: so in about
// the time of as many restores as the tree is deep, not as it has nodes; and
// that it returns the error of the failed node with the least path, whatever
// the order of the restores, having restored every node below no failed one.
func TestRestoreTree(t *testing.T) {
	const (
		width       = 4
		depth       = 3
		restoreTime = 20 * time.Millisecond
	)
	for _, tc := range []struct {
		name        string
		parallelism int
		fail        []string
		wantErr     string
	}{
		{name: "parallel", parallelism: 64},
		{name: "bounded", parallelism: 4},
		{name: "failures", parallelism: 16, fail: []string{"3", "1/2", "2/0/1"}, wantErr: "1/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, nodes := newTree(width, depth)
			for _, n := range nodes {
				n.fail = slices.Contains(tc.fail, n.path)
			}
			var (
				running, most atomic.Int32
				restored      sync.Map
			)
			restored.Store(root, true)
			restore := func(n *treeNode) error {
				now := running.Add(1)
				defer running.Add(-1)
				for {
					m := most.Load()
					if now <= m || most.CompareAndSwap(m, now) {
						break
					}
				}
				n.restores.Add(1)
				_, n.parentRestored = restored.Load(n.parent)
				time.Sleep(restoreTime)
				if n.fail {
					return fmt.Errorf("%s failed", n.path)
				}
				restored.Store(n, true)
				return nil
			}
			start := time.Now()
			err := restoreTree(root, tc.parallelism, func(n *treeNode) []*treeNode { return n.children }, restore, func(n *treeNode) string { return n.path })
			elapsed := time.Since(start)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("restoreTree(): %v", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr+" failed"):
				t.Fatalf("restoreTree(): got error %v, want %q's", err, tc.wantErr)
			}
			for _, n := range nodes {
				below := false
				for a := n.parent; a != nil; a = a.parent {
					below = below || a.fail
				}
				want := int32(1)
				if below {
					want = 0
				}
				if got := n.restores.Load(); got != want {
					t.Errorf("%q restored %d times, want %d", n.path, got, want)
				}
				if got := n.restores.Load(); got != 0 && !n.parentRestored {
					t.Errorf("%q restored before its parent", n.path)
				}
			}
			if got := most.Load(); got > int32(tc.parallelism) {
				t.Errorf("%d restores at once, want at most %d", got, tc.parallelism)
			}
			// The serial walk takes as many restores as the tree has nodes; the
			// parallel one, as it is deep where every level's nodes restore at
			// once, which the slack of half the serial walk's time leaves room
			// for on a loaded machine.
			if tc.parallelism >= width*width*width && elapsed >= time.Duration(len(nodes))*restoreTime/2 {
				t.Errorf("restoreTree() took %v for %d nodes %d deep, want about %v", elapsed, len(nodes), depth, time.Duration(depth)*restoreTime)
			}
		})
	}
}

// TestRestoreDescendantsParallel checks that a filesystem restores its
// dentries in parallel as it does serially: every dentry's file reopened,
// once for an inode that several share.
func TestRestoreDescendantsParallel(t *testing.T) {
	for _, parallelism := range []int{1, 8} {
		t.Run(fmt.Sprintf("parallelism %d", parallelism), func(t *testing.T) {
			ctx := contexttest.Context(t)
			dir := t.TempDir()
			var paths []string
			for i := range 4 {
				d := fmt.Sprintf("d%d", i)
				paths = append(paths, d)
				if err := os.Mkdir(filepath.Join(dir, d), 0755); err != nil {
					t.Fatalf("os.Mkdir(): %v", err)
				}
				for j := range 4 {
					f := path.Join(d, fmt.Sprintf("f%d", j))
					paths = append(paths, f)
					if err := os.WriteFile(filepath.Join(dir, f), nil, 0644); err != nil {
						t.Fatalf("os.WriteFile(): %v", err)
					}
				}
			}
			// Hard links of d0/f0, which share its inode.
			for _, link := range []string{"d1/l", "d2/l", "d3/l"} {
				paths = append(paths, link)
				if err := os.Link(filepath.Join(dir, "d0/f0"), filepath.Join(dir, link)); err != nil {
					t.Fatalf("os.Link(): %v", err)
				}
			}
			fs := &filesystem{
				mf:          pgalloc.MemoryFileFromContext(ctx),
				inoByKey:    make(map[inoKey]uint64),
				inodeByKey:  make(map[inoKey]*inode),
				clock:       ktime.RealtimeClockFromContext(ctx),
				opts:        filesystemOptions{virtualInodeNumbers: true},
				dentryCache: &dentryCache{maxCachedDentries: 0},
			}
			rootFD, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
			if err != nil {
				t.Fatalf("unix.Open(%q): %v", dir, err)
			}
			root, err := fs.newDirectfsDentry(rootFD)
			if err != nil {
				t.Fatalf("fs.newDirectfsDentry(%q): %v", dir, err)
			}
			fs.root = root
			dentries := map[string]*dentry{".": root}
			for _, p := range paths {
				parent := dentries[path.Dir(p)]
				fd, err := unix.Openat(parent.inode.impl.(*directfsInode).controlFD, path.Base(p), unix.O_PATH, 0)
				if err != nil {
					t.Fatalf("unix.Openat(%q): %v", p, err)
				}
				d, err := fs.newDirectfsDentry(fd)
				if err != nil {
					t.Fatalf("fs.newDirectfsDentry(%q): %v", p, err)
				}
				parent.opMu.Lock()
				parent.childrenMu.Lock()
				parent.cacheNewChildLocked(d, path.Base(p))
				parent.childrenMu.Unlock()
				parent.opMu.Unlock()
				dentries[p] = d
			}
			if dentries["d1/l"].inode != dentries["d0/f0"].inode {
				t.Fatalf("the hard links of d0/f0 do not share its inode")
			}
			// As a load leaves them: no dentry below the root restored.
			for _, p := range paths {
				i := dentries[p].inode.impl.(*directfsInode)
				if i.controlFD >= 0 {
					if err := unix.Close(i.controlFD); err != nil {
						t.Fatalf("unix.Close(): %v", err)
					}
				}
				i.controlFD = -1
				dentries[p].inode.restoreClaimed.Store(false)
			}

			if err := fs.restoreDescendants(ctx, &vfs.CompleteRestoreOptions{Parallelism: parallelism}); err != nil {
				t.Fatalf("fs.restoreDescendants(): %v", err)
			}
			for _, p := range paths {
				fd := dentries[p].inode.impl.(*directfsInode).controlFD
				var stat unix.Stat_t
				if err := unix.Fstat(fd, &stat); err != nil {
					t.Errorf("%q: its file is not reopened: fstat(%d): %v", p, fd, err)
					continue
				}
				var want unix.Stat_t
				if err := unix.Stat(filepath.Join(dir, p), &want); err != nil {
					t.Fatalf("unix.Stat(%q): %v", p, err)
				}
				if stat.Ino != want.Ino {
					t.Errorf("%q: reopened inode %d, want %d", p, stat.Ino, want.Ino)
				}
			}
		})
	}
}

func TestStringFixedCache(t *testing.T) {
	names := []string{"a", "b", "c"}
	cache := stringFixedCache{}

	cache.init(uint64(len(names)))
	if inited := cache.isInited(); !inited {
		t.Fatalf("cache.isInited(): %v, want: true", inited)
	}
	for _, s := range names {
		victim := cache.add(s)
		if victim != "" {
			t.Fatalf("cache.add(): %v, want: \"\"", victim)
		}
	}
	for _, s := range names {
		victim := cache.add("something")
		if victim != s {
			t.Fatalf("cache.add(): %v, want: %v", victim, s)
		}
	}
}

func TestXattrCacheValues(t *testing.T) {
	ino := &inode{}

	// 1. Initial state
	if _, _, found := ino.xattrCache.get("user.foo"); found {
		t.Errorf("get(user.foo) got found=true, want false")
	}

	// 2. Cache present xattr
	ino.xattrCache.add("user.foo", "bar")
	val, negative, found := ino.xattrCache.get("user.foo")
	if !found || negative || val != "bar" {
		t.Errorf("get(user.foo) = (%q, %v, %v), want (%q, false, true)", val, negative, found, "bar")
	}

	// 3. Cache negative xattr
	ino.xattrCache.addNegative("user.neg")
	val, negative, found = ino.xattrCache.get("user.neg")
	if !found || !negative || val != "" {
		t.Errorf("get(user.neg) = (%q, %v, %v), want (%q, true, true)", val, negative, found, "")
	}

	// Overwrite negative xattr with present xattr
	ino.xattrCache.add("user.neg", "baz")
	val, negative, found = ino.xattrCache.get("user.neg")
	if !found || negative || val != "baz" {
		t.Errorf("get(user.neg) = (%q, %v, %v), want (%q, false, true)", val, negative, found, "baz")
	}

	// 4. Overlay opaque xattr
	ino.xattrCache.addNegative(xattrOverlayOpaque)
	val, negative, found = ino.xattrCache.get(xattrOverlayOpaque)
	if !found || !negative || val != "" {
		t.Errorf("get(xattrOverlayOpaque) = (%q, %v, %v), want (%q, true, true)", val, negative, found, "")
	}

	// Overwrite overlay opaque xattr with present xattr
	ino.xattrCache.add(xattrOverlayOpaque, "y")
	val, negative, found = ino.xattrCache.get(xattrOverlayOpaque)
	if !found || negative || val != "y" {
		t.Errorf("get(xattrOverlayOpaque) = (%q, %v, %v), want (%q, false, true)", val, negative, found, "y")
	}
}

func TestXattrCacheList(t *testing.T) {
	ino := &inode{}

	// 1. Initial state
	if _, found := ino.xattrCache.getList(); found {
		t.Errorf("getList() got found=true, want false")
	}

	// 2. Cache empty list
	ino.xattrCache.setList([]string{})
	list, found := ino.xattrCache.getList()
	if !found || len(list) != 0 {
		t.Errorf("getList() = (%v, %v), want ([], true)", list, found)
	}

	// 3. Cache non-empty list
	initialList := []string{"user.foo", "user.bar"}
	ino.xattrCache.setList(initialList)
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, initialList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, initialList)
	}

	// 4. Cache new xattr updates list
	ino.xattrCache.add("user.baz", "val")
	wantList := []string{"user.foo", "user.bar", "user.baz"}
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, wantList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, wantList)
	}

	// 5. Cache existing xattr does not duplicate in list
	ino.xattrCache.add("user.foo", "val2")
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, wantList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, wantList)
	}

	// 6. Cache negative xattr removes from list
	ino.xattrCache.addNegative("user.bar")
	wantList = []string{"user.foo", "user.baz"}
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, wantList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, wantList)
	}
}
