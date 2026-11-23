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

package gofer

import (
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// saveStats counts and times a filesystem's PrepareSave, for a measurement
// build: where the pause's file metadata work goes.
type saveStats struct {
	evict, special, sync, walk time.Duration
	// cached is the length of the dentry cache, which all gofer filesystems
	// may share, before the eviction; evicted how many dentries it destroyed
	// (some may belong to other filesystems sharing the cache, and evicting a
	// dentry may cache its parent), of which regular and dirs; kept those it
	// found referenced again.
	cached                               uint64
	evicted, evictedRegular, evictedDirs int
	kept                                 int
	// invalidate is the time of the evictions' VFS invalidation and removal
	// from their parents; destroy that of the dentries' destruction, of
	// which inodeDestroy that of their inodes' (writeback of dirty cached
	// data, of which writeback, dropping cached data, closing host FDs or
	// lisafs control FDs), and parent that of releasing their parents.
	invalidate, destroy, inodeDestroy, writeback, parent time.Duration
	inodesDestroyed                                      int
	dentries, regular                                    int
	// statted are the regular files whose cached metadata is authoritative,
	// whose remote size and mtime are stat'ed; refreshed those whose cached
	// metadata is refreshed from the remote file.
	statted, refreshed int
	stat, refresh      time.Duration
}

func (s *saveStats) String() string {
	return fmt.Sprintf("evict %v (%d cached: %d evicted, %d regular, %d dirs, %d kept; invalidate %v, destroy %v, of which %d inodes %v, writeback %v, parents %v), special %v, sync %v, walk %v: %d dentries, %d regular, %d statted in %v, %d refreshed in %v",
		s.evict, s.cached, s.evicted, s.evictedRegular, s.evictedDirs, s.kept, s.invalidate, s.destroy, s.inodesDestroyed, s.inodeDestroy, s.writeback, s.parent, s.special, s.sync, s.walk, s.dentries, s.regular, s.statted, s.stat, s.refreshed, s.refresh)
}

// evictionStats is the saveStats into which the evictions of a PrepareSave
// count, nil outside of it. Only PrepareSave sets it, while the kernel is
// paused.
var evictionStats *saveStats

// lazyEstimate returns what a lazy restore (one that restored eagerly only
// the dentries held open, the regular files that opts validates, the mount
// points and their ancestors) would have restored of fs's tree, and the
// longest chain of restores (the time of a restore parallel enough) of the
// whole tree and of that part, from the restore just done.
//
// Preconditions: fs's restore has restored its descendants, and has not
// discarded fs.savedDentryRW.
func (fs *filesystem) lazyEstimate(opts *vfs.CompleteRestoreOptions) string {
	durs := fs.restoreStats.durs
	var n, eager, held, validated, mounts int
	var serialAll, serialEager time.Duration
	var walk func(d *dentry) (time.Duration, time.Duration, bool)
	walk = func(d *dentry) (time.Duration, time.Duration, bool) {
		var cpAll, cpEager time.Duration
		anyEager := false
		for _, c := range d.restorableChildren() {
			a, e, ce := walk(c)
			cpAll = max(cpAll, a)
			if ce {
				anyEager = true
				cpEager = max(cpEager, e)
			}
		}
		if d == fs.root {
			return cpAll, cpEager, true
		}
		n++
		self := durs[d]
		_, h := fs.savedDentryRW[d]
		v := d.inode.isRegularFile() && (opts.ValidateFileSizes || opts.ValidateFileModificationTimestamps) && opts.ValidatesFile(fs.iopts.UniqueID, genericDebugPathname(fs, d))
		m := d.forMountpoint
		if h {
			held++
		}
		if v {
			validated++
		}
		if m {
			mounts++
		}
		ce := h || v || m || anyEager
		serialAll += self
		if ce {
			eager++
			serialEager += self
		}
		return self + cpAll, self + cpEager, ce
	}
	cpAll, cpEager, _ := walk(fs.root)
	return fmt.Sprintf("%d dentries: %d eager (%d held open, %d validated, %d mount points), %d lazy; critical path %v all, %v eager; serial %v all, %v eager",
		n, eager, held, validated, mounts, n-eager, cpAll, cpEager, serialAll, serialEager)
}

// restoreStatsMu serializes the updates of restoreStats by restores that
// may run in parallel (restoreTree).
var restoreStatsMu sync.Mutex

// restoreStats counts and times a filesystem's CompleteRestore, for a
// measurement build: where the restore's VFS time goes.
type restoreStats struct {
	root                  time.Duration
	dirs, regular, others int
	// shared are the dentries whose inode another dentry had restored.
	shared int
	// walk is the time of the walks (lisafs) or opens (directfs) from each
	// dentry's parent; inode that of the rest of each dentry's restore: its
	// stat (directfs), inoKey maps, validation and handles.
	walk, inode time.Duration
	validated   int
	validate    time.Duration
	handles     int
	handle      time.Duration
	maxDepth    int
	deleted     int
	deletedTime time.Duration
	specials    int
	specialTime time.Duration
	cleanup     time.Duration
	// durs is each restored dentry's restoreFile time.
	durs map[*dentry]time.Duration
}

func (s *restoreStats) String() string {
	return fmt.Sprintf("root %v; %d dentries (%d dirs, %d regular, %d others, %d shared, depth %d) in %v walk + %v inode (validate %d in %v, handles %d in %v); deleted %d in %v; special FDs %d in %v; cleanup %v",
		s.root, s.dirs+s.regular+s.others, s.dirs, s.regular, s.others, s.shared, s.maxDepth, s.walk, s.inode, s.validated, s.validate, s.handles, s.handle, s.deleted, s.deletedTime, s.specials, s.specialTime, s.cleanup)
}
