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

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/summary"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
)

// TestWriteFileAtomically checks that writeFileAtomically replaces a file
// whole, readable by all, and leaves no temporary file behind.
func TestWriteFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")
	for _, data := range []string{"first\n", "second\n"} {
		if err := writeFileAtomically(path, []byte(data)); err != nil {
			t.Fatalf("writeFileAtomically: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatalf("%s holds %q (%v), want %q", path, got, err, data)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fi.Mode().Perm(), os.FileMode(0644); got != want {
			t.Errorf("%s has mode %v, want %v", path, got, want)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("%s holds %v, want only %s", dir, entries, filepath.Base(path))
		}
	}
	if err := writeFileAtomically(filepath.Join(dir, "missing", "summary.json"), nil); err == nil {
		t.Errorf("writeFileAtomically in a missing directory succeeded")
	}
}

// requireKeys fails t unless the JSON object obj, at where, has every key of
// keys.
func requireKeys(t *testing.T, where string, obj any, keys ...string) {
	t.Helper()
	m, ok := obj.(map[string]any)
	if !ok {
		t.Fatalf("%s is %v, want an object", where, obj)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s lacks %q: %v", where, k, m)
		}
	}
}

// TestCheckpointRestoreSummaryCLI checks the summaries of a checkpoint and of
// a restore that runsc checkpoint --summary-file writes and runsc wait
// --restore --summary prints: the keys that their schema promises, and
// numbers consistent with the image, as runsc image inspect describes it, and
// with each other.
func TestCheckpointRestoreSummaryCLI(t *testing.T) {
	for _, precopy := range []bool{false, true} {
		t.Run(fmt.Sprintf("precopy=%t", precopy), func(t *testing.T) {
			conf := testutil.TestConfig(t)
			if precopy {
				conf.DirtyTracking = config.DirtyTrackingWriteProtect
			}
			spec := testutil.NewSpecWithArgs("/bin/sleep", "10000")
			_, bundle, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			id := testutil.RandomContainerID()
			command := func(args ...string) *exec.Cmd {
				return exec.Command(specutils.ExePath, append(conf.ToFlags(), args...)...)
			}
			// run runs runsc with args, and returns its standard output.
			// Detached containers retain stdio, so a command that starts one
			// gets the test's own instead of a pipe whose EOF would wait for
			// the container to exit.
			run := func(detach bool, args ...string) []byte {
				t.Helper()
				cmd := command(args...)
				cmd.Stderr = os.Stderr
				var stdout bytes.Buffer
				if detach {
					cmd.Stdout = os.Stdout
				} else {
					cmd.Stdout = &stdout
				}
				if err := cmd.Run(); err != nil {
					t.Fatalf("runsc %v: %v", args, err)
				}
				return stdout.Bytes()
			}
			defer func() {
				if out, err := command("delete", "--force", id).CombinedOutput(); err != nil {
					t.Errorf("deleting container: %v\n%s", err, out)
				}
			}()
			run(true, "run", "--detach", "--bundle", bundle, id)

			imageDir := t.TempDir()
			summaryPath := filepath.Join(t.TempDir(), "checkpoint.json")
			args := []string{"checkpoint", "--image-path", imageDir, "--compression", "none", "--summary-file", summaryPath}
			if precopy {
				args = append(args, "--precopy", "on")
			}
			run(false, append(args, id)...)
			data, err := os.ReadFile(summaryPath)
			if err != nil {
				t.Fatalf("reading the checkpoint's summary: %v", err)
			}
			var keys map[string]any
			if err := json.Unmarshal(data, &keys); err != nil {
				t.Fatalf("the checkpoint's summary %s: %v", data, err)
			}
			requireKeys(t, "summary", keys, "schema", "image", "save", "precopy", "pages")
			requireKeys(t, "summary.image", keys["image"], "digest", "version", "cpu_features", "layers", "memory_files", "files")
			requireKeys(t, "summary.save", keys["save"], "pause_ms", "kernel_ms", "memory_files_ms", "overall_ms", "cpuid_ms", "network_pause_ms", "split_fs_ms", "prepare_save_ms", "gc_ms", "gc_world_stopped_ms", "kernel_objects", "kernel_bytes")
			requireKeys(t, "summary.pages", keys["pages"], "written_bytes", "base_bytes", "unchanged_by_hash_bytes", "zero_bytes", "extents")
			if precopy {
				requireKeys(t, "summary.precopy", keys["precopy"], "rounds", "bytes", "ms", "converged", "skipped")
			}
			var sum summary.Checkpoint
			if err := json.Unmarshal(data, &sum); err != nil {
				t.Fatalf("the checkpoint's summary %s: %v", data, err)
			}

			var info struct {
				Digest string `json:"digest"`
				State  struct {
					Metadata map[string]string `json:"metadata"`
				} `json:"state"`
				MemoryFiles []struct {
					Extents    int      `json:"extents"`
					LayerBytes []uint64 `json:"layer_bytes"`
				} `json:"memory_files"`
			}
			if err := json.Unmarshal(run(false, "image", "inspect", "--json", imageDir), &info); err != nil {
				t.Fatalf("image inspect --json: %v", err)
			}
			img := sum.Image
			if sum.Schema != summary.Schema || img == nil || img.Digest == "" || img.Digest != info.Digest || img.Version != info.State.Metadata[boot.VersionKey] || img.CPUFeatures == "" || img.CPUFeatures != info.State.Metadata[sentryapi.CPUFeaturesKey] || len(img.Layers) != 0 || len(img.MemoryFiles) != len(info.MemoryFiles) {
				t.Errorf("the checkpoint's summary is %s; want schema %d, and the image that image inspect --json describes as %+v", data, summary.Schema, info)
			}
			var imageBytes uint64
			extents := 0
			for _, mf := range info.MemoryFiles {
				imageBytes += mf.LayerBytes[0]
				extents += mf.Extents
			}
			pagesFile, err := os.Stat(filepath.Join(imageDir, checkpointfiles.PagesFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !containsFile(img.Files, checkpointfiles.PagesFileName, pagesFile.Size()) || !containsFile(img.Files, checkpointfiles.StateFileName, -1) {
				t.Errorf("the checkpoint's summary lists the image's files as %+v, want %s of %d bytes and %s among them", img.Files, checkpointfiles.PagesFileName, pagesFile.Size(), checkpointfiles.StateFileName)
			}
			if p := sum.Pages; p == nil || p.WrittenBytes+p.PrecopiedBytes != imageBytes || p.Extents != extents || p.BaseBytes != 0 || p.UnchangedByHashBytes != 0 {
				t.Errorf("the checkpoint's summary says its pages are %+v, want %d bytes written or pre-copied in %d extents, as image inspect says, and none in a base image", sum.Pages, imageBytes, extents)
			}
			s := sum.Save
			if s.Kernel <= 0 || s.MemoryFiles <= 0 || s.CPUID <= 0 || s.Overall < s.Kernel || s.Overall < s.MemoryFiles || s.Pause < s.Overall || s.SplitFS != nil || s.GC != nil {
				t.Errorf("the checkpoint's summary says that the save took %+v; want its parts to take some time, the whole at least each part, and the pause at least the whole", s)
			}
			if _, ok := s.PrepareSave["unsavable_mappings"]; !ok || len(s.PrepareSave) < 2 {
				t.Errorf("the checkpoint's summary says that preparing the save took %v, want the unsavable mappings' and some filesystems'", s.PrepareSave)
			}
			if (s.KernelObjects == nil) != (s.KernelBytes == nil) || (s.KernelObjects != nil && (*s.KernelObjects <= 0 || *s.KernelBytes <= 0)) {
				t.Errorf("the checkpoint's summary says that the kernel's state has %v objects and %v bytes, want both counted or neither", s.KernelObjects, s.KernelBytes)
			}
			switch pc := sum.Precopy; {
			case !precopy && pc != nil:
				t.Errorf("the checkpoint's summary describes a pre-copy %+v, want none", pc)
			case precopy && (pc == nil || pc.Rounds < 1 || pc.Bytes == 0 || pc.Skipped || pc.Stop == "" || pc.Converged != (pc.Stop == "converged")):
				t.Errorf("the checkpoint's summary describes the pre-copy as %+v, want rounds that wrote bytes", pc)
			}

			run(false, "delete", "--force", id)
			run(true, "restore", "--detach", "--image-path", imageDir, "--bundle", bundle, id)
			out := run(false, "wait", "--restore", "--summary", id)
			if bytes.Count(out, []byte("\n")) != 1 {
				t.Errorf("wait --restore --summary printed %q, want one line", out)
			}
			if err := json.Unmarshal(out, &keys); err != nil {
				t.Fatalf("the restore's summary %s: %v", out, err)
			}
			requireKeys(t, "summary", keys, "schema", "restore", "loading")
			requireKeys(t, "summary.restore", keys["restore"], "kernel_load_ms", "memory_load_ms", "cpuid_load_ms", "overall_load_ms", "restore_ms", "working_set_bytes", "working_set_read_first_bytes")
			var rsum summary.WaitRestore
			if err := json.Unmarshal(out, &rsum); err != nil {
				t.Fatalf("the restore's summary %s: %v", out, err)
			}
			r := rsum.Restore
			if rsum.Schema != summary.Schema || r.CPUIDLoad <= 0 || r.KernelLoad <= 0 || r.MemoryLoad != nil || r.OverallLoad < r.KernelLoad || r.Restore < r.OverallLoad || r.Boot < 0 || r.WorkingSetReadFirstBytes > r.WorkingSetBytes {
				t.Errorf("the restore's summary is %s; want schema %d, its parts to take some time, the memory files loading asynchronously, and the whole at least each part", out, summary.Schema)
			}
			if len(rsum.Loading) != 1 {
				t.Fatalf("the restore's summary describes the loading of %d layers, want 1: %s", len(rsum.Loading), out)
			}
			requireKeys(t, "summary.loading[0]", keys["loading"].([]any)[0], "bytes", "ms", "waiters", "waited_ms", "waited_bytes", "background_bandwidth_mb_s")
			if l := rsum.Loading[0]; l.Bytes == 0 || l.Bytes > imageBytes || l.Duration <= 0 || l.BackgroundBandwidth <= 0 {
				t.Errorf("the restore's summary says that the image loaded as %+v, want some of its %d bytes of pages, in some time", l, imageBytes)
			}
		})
	}
}

// containsFile returns whether files lists name, of size bytes unless size is
// negative.
func containsFile(files []summary.File, name string, size int64) bool {
	for _, f := range files {
		if f.Name == name && (size < 0 || f.Bytes == size) {
			return true
		}
	}
	return false
}
