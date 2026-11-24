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

// Package summary defines the summaries of a checkpoint and of a restore that
// runsc writes as JSON for programs that drive it (runsc checkpoint
// --summary-file, runsc wait --restore --summary), so that they need not read
// its logs: what the image holds, and how long each part of the save or the
// restore took.
//
// Every duration is in milliseconds, as a float; every count and size is an
// integer. A value that the checkpoint or the restore does not have is null.
// Readers must ignore keys they do not know: later versions of a schema may
// add keys, but not change the meaning of those it has.
package summary

import (
	"encoding/json"
	"time"
)

// Schema is the version of the summaries' schema.
const Schema = 1

// Duration is a time.Duration that is encoded in JSON as a number of
// milliseconds, a float.
type Duration time.Duration

// Millis returns d as a *Duration.
func Millis(d time.Duration) *Duration {
	ms := Duration(d)
	return &ms
}

// MarshalJSON implements json.Marshaler.MarshalJSON.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(float64(d) / float64(time.Millisecond))
}

// UnmarshalJSON implements json.Unmarshaler.UnmarshalJSON.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var ms float64
	if err := json.Unmarshal(b, &ms); err != nil {
		return err
	}
	*d = Duration(ms * float64(time.Millisecond))
	return nil
}

// Checkpoint is the summary of a checkpoint, written by runsc checkpoint
// --summary-file once the image is complete.
type Checkpoint struct {
	// Schema is the schema's version, Schema.
	Schema int `json:"schema"`

	// Image describes the image, as runsc image inspect and runsc image layers
	// do. runsc fills it in from the image once the sandbox has written it.
	Image *Image `json:"image"`

	// Save holds how long the parts of the save took.
	Save Save `json:"save"`

	// Precopy describes the pre-copy that wrote memory while the sandbox ran
	// before the save paused it, or is nil if the checkpoint had none
	// (--precopy=off).
	Precopy *Precopy `json:"precopy"`

	// Pages sums up the pages of the image's memory files, or is nil if the
	// image holds its memory in its state file (a compressed checkpoint).
	Pages *Pages `json:"pages"`
}

// Image describes a checkpoint image.
type Image struct {
	// Digest is the image's digest, as runsc image inspect --json gives it
	// (.digest), or "" if the image holds its memory in its state file.
	Digest string `json:"digest"`

	// Version is the version of runsc that wrote the image, as runsc image
	// inspect --json gives it (.state.metadata, under runsc's version key).
	Version string `json:"version"`

	// CPUFeatures is the CPU features that the sandbox was saved with, which
	// a restore requires of the host, as runsc image inspect --json gives
	// them (.state.metadata.cpu_features): their names, separated by commas.
	CPUFeatures string `json:"cpu_features"`

	// Layers holds the digest of each image whose pages file holds data of
	// the image, as runsc image layers prints them, in that order.
	Layers []string `json:"layers"`

	// MemoryFiles describes the image's memory files, in the order of runsc
	// image inspect --json's .memory_files: the application's, then private
	// ones.
	MemoryFiles []MemoryFile `json:"memory_files"`

	// Files holds the regular files at the top of the image directory, by
	// name.
	Files []File `json:"files"`
}

// MemoryFile describes a memory file of an image.
type MemoryFile struct {
	// LayerBytes holds, for each layer of the image (0 is the image itself,
	// then Image.Layers in order), the bytes of the memory file's pages that
	// the layer's pages file holds, as runsc image inspect --json's
	// .memory_files[].layer_bytes.
	LayerBytes []uint64 `json:"layer_bytes"`
}

// File is a file of an image directory.
type File struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// Save holds how long the parts of a save took, and what it wrote of the
// kernel's state.
type Save struct {
	// Pause is how long the save kept the sandbox's tasks paused.
	Pause Duration `json:"pause_ms"`

	// PrepareSave holds how long the save took, tasks paused, to invalidate
	// the mappings that cannot be saved ("unsavable_mappings"), and to
	// prepare the filesystems of each type for the save (by filesystem type
	// name, such as "9p" or "tmpfs").
	PrepareSave map[string]Duration `json:"prepare_save_ms"`

	// CPUID, NetworkPause, Kernel, SplitFS, MemoryFiles and Overall are how
	// long the save took to write the CPU features, to pause the network
	// stack, to write the kernel's state, to write the split filesystem
	// checkpoint (nil without one), to write the memory files (concurrently
	// with the kernel's state if the image has a pages file), and all of
	// these, preparation included.
	CPUID        Duration  `json:"cpuid_ms"`
	NetworkPause Duration  `json:"network_pause_ms"`
	Kernel       Duration  `json:"kernel_ms"`
	SplitFS      *Duration `json:"split_fs_ms"`
	MemoryFiles  Duration  `json:"memory_files_ms"`
	Overall      Duration  `json:"overall_ms"`

	// GC and GCWorldStopped are how long the garbage collection after the
	// save took, and how long it stopped the world. They are always nil: the
	// sandbox collects once it has answered the checkpoint, so that the
	// collection does not delay it, and logs both.
	GC             *Duration `json:"gc_ms"`
	GCWorldStopped *Duration `json:"gc_world_stopped_ms"`

	// KernelObjects and KernelBytes are the objects and bytes of the kernel's
	// state that the save wrote, or nil if the state engine that wrote it does
	// not count them (--state-engine=wire).
	KernelObjects *int64 `json:"kernel_objects"`
	KernelBytes   *int64 `json:"kernel_bytes"`
}

// Precopy describes a pre-copy.
type Precopy struct {
	// Rounds is the number of rounds that ran, and Bytes the bytes they
	// wrote, in Duration.
	Rounds   int      `json:"rounds"`
	Bytes    uint64   `json:"bytes"`
	Duration Duration `json:"ms"`

	// Stop is why the rounds stopped: "converged" (the pages dirtied during
	// the last round would take at most the budget to write), "round_cap"
	// (the maximum number of rounds ran), "not_halved" (a round did not halve
	// the bytes left to write), or "skipped" (--precopy=auto found that the
	// save would write its pages within the budget anyway, and no round ran).
	Stop string `json:"stop"`

	// Converged and Skipped are whether Stop is "converged" and "skipped".
	Converged bool `json:"converged"`
	Skipped   bool `json:"skipped"`
}

// Pages sums up the pages of an image's memory files.
type Pages struct {
	// WrittenBytes is the bytes of pages that the save wrote to the image's
	// pages file, pre-copied pages excepted.
	WrittenBytes uint64 `json:"written_bytes"`

	// PrecopiedBytes is the bytes of pages whose last copy by the pre-copy's
	// rounds, in the pages file, the image refers to.
	PrecopiedBytes uint64 `json:"precopied_bytes"`

	// BaseBytes is the bytes of pages that an incremental image refers to its
	// parent for, since they were not written since; UnchangedByHashBytes
	// the bytes of pages that it refers to its parent for too, since they
	// were written since but the save found them unchanged by their hash.
	BaseBytes            uint64 `json:"base_bytes"`
	UnchangedByHashBytes uint64 `json:"unchanged_by_hash_bytes"`

	// ZeroBytes is the bytes of pages that the memory files held but that
	// the image records without data, since they hold only zeroes, besides
	// those that the save found zero as it scanned the memory files and so
	// left out of them.
	ZeroBytes uint64 `json:"zero_bytes"`

	// Extents is the number of extents of the memory files' pages.
	Extents int `json:"extents"`
}

// WaitRestore is the summary of a restore, printed by runsc wait --restore
// --summary once the restore is complete and every page is loaded.
type WaitRestore struct {
	// Schema is the schema's version, Schema.
	Schema int `json:"schema"`

	// Restore holds how long the parts of the restore took.
	Restore Restore `json:"restore"`

	// Loading describes the loading of the pages of each layer of the image,
	// in the order of the image's layers (0 is the image itself). It is empty
	// if the image holds its memory in its state file.
	Loading []Loading `json:"loading"`
}

// Restore holds how long the parts of a restore took. The restore starts when
// the sandbox is asked to restore (runsc restore, or a restore into a sandbox
// that was started before).
type Restore struct {
	// Boot is the time from the start of the sandbox's process to the
	// restore's start: about none for a sandbox started to restore, the time
	// that it waited for a sandbox started before.
	Boot Duration `json:"boot_ms"`

	// CPUIDLoad, KernelLoad and MemoryLoad are how long the restore took to
	// load the CPU features, the kernel's state, and the memory files, which
	// it loads at once only if the image holds them in its state file (nil
	// otherwise: their pages load asynchronously, as Loading describes).
	CPUIDLoad  Duration  `json:"cpuid_load_ms"`
	KernelLoad Duration  `json:"kernel_load_ms"`
	MemoryLoad *Duration `json:"memory_load_ms"`

	// OverallLoad is the time from the restore's start until the kernel was
	// loaded, its tasks runnable.
	OverallLoad Duration `json:"overall_load_ms"`

	// Restore is the time from the restore's start until it completed, every
	// page loaded ("Restore successful" in the log).
	Restore Duration `json:"restore_ms"`

	// WorkingSetBytes is the bytes of loaded pages that the image's working
	// set holds; WorkingSetReadFirstBytes the bytes of them that the loaders
	// read before the rest of their pages.
	WorkingSetBytes          uint64 `json:"working_set_bytes"`
	WorkingSetReadFirstBytes uint64 `json:"working_set_read_first_bytes"`
}

// Loading describes the loading of the pages of a layer of an image.
type Loading struct {
	// Bytes is the bytes of pages that the loader read, in Duration, and
	// BackgroundBandwidth Bytes over Duration, in MB/s (1e6 bytes a second).
	Bytes               uint64   `json:"bytes"`
	Duration            Duration `json:"ms"`
	BackgroundBandwidth float64  `json:"background_bandwidth_mb_s"`

	// Waiters is the number of times that a task, or the sandbox, waited for
	// pages that were not loaded yet; Waited how long they waited in all, and
	// WaitedBytes the bytes that at least one of them waited for.
	Waiters     uint64   `json:"waiters"`
	Waited      Duration `json:"waited_ms"`
	WaitedBytes uint64   `json:"waited_bytes"`

	// WaitedAny is how long at least one of them was waiting.
	WaitedAny Duration `json:"waited_any_ms"`
}
