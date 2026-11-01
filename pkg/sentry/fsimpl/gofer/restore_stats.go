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
)

// saveStats counts and times a filesystem's PrepareSave, for a measurement
// build: where the pause's file metadata work goes.
type saveStats struct {
	evict, special, sync, walk time.Duration
	dentries, regular          int
	// statted are the regular files whose cached metadata is authoritative,
	// whose remote size and mtime are stat'ed; refreshed those whose cached
	// metadata is refreshed from the remote file.
	statted, refreshed int
	stat, refresh      time.Duration
}

func (s *saveStats) String() string {
	return fmt.Sprintf("evict %v, special %v, sync %v, walk %v: %d dentries, %d regular, %d statted in %v, %d refreshed in %v",
		s.evict, s.special, s.sync, s.walk, s.dentries, s.regular, s.statted, s.stat, s.refreshed, s.refresh)
}

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
}

func (s *restoreStats) String() string {
	return fmt.Sprintf("root %v; %d dentries (%d dirs, %d regular, %d others, %d shared, depth %d) in %v walk + %v inode (validate %d in %v, handles %d in %v); deleted %d in %v; special FDs %d in %v; cleanup %v",
		s.root, s.dirs+s.regular+s.others, s.dirs, s.regular, s.others, s.shared, s.maxDepth, s.walk, s.inode, s.validated, s.validate, s.handles, s.handle, s.deleted, s.deletedTime, s.specials, s.specialTime, s.cleanup)
}
