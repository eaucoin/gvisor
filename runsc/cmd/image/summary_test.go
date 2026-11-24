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

package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/subcommands"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/summary"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/version"
)

// TestSummaryImage checks that SummaryImage describes an incremental image as
// inspect --json and layers do, with the regular files at the top of its
// directory.
func TestSummaryImage(t *testing.T) {
	root, template, _ := testImages(t)
	dir := filepath.Join(root, "delta")
	// The CPU features that the sandbox was saved with.
	writeStateFile(t, filepath.Join(dir, checkpointfiles.StateFileName), map[string]string{
		boot.VersionKey:                   version.Version(),
		checkpointimage.FormatMetadataKey: checkpointimage.FormatVersion(),
		checkpointimage.ParentMetadataKey: template.Digest.String(),
		sentryapi.CPUFeaturesKey:          "fpu,sse,sse2",
		boot.ContainerSpecsKey:            `{"app":{"ociVersion":"1.0.2"}}`,
	})
	got, err := SummaryImage(&config.Config{}, dir)
	if err != nil {
		t.Fatalf("SummaryImage: %v", err)
	}

	status, out := run(t, new(inspect), "--json", dir)
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect --json: %v", status)
	}
	var info imageInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("inspect --json printed %q: %v", out, err)
	}
	status, out = run(t, new(layers), dir)
	if status != subcommands.ExitSuccess {
		t.Fatalf("layers: %v", status)
	}
	want := &summary.Image{
		Digest:      info.Digest,
		Version:     info.State.Metadata[boot.VersionKey],
		CPUFeatures: info.State.Metadata[sentryapi.CPUFeaturesKey],
		Layers:      strings.Fields(out),
	}
	for _, mf := range info.MemoryFiles {
		want.MemoryFiles = append(want.MemoryFiles, summary.MemoryFile{LayerBytes: mf.LayerBytes})
	}
	// The layers/ directory is not a regular file.
	for _, name := range []string{checkpointfiles.StateFileName, checkpointfiles.PagesFileName, checkpointfiles.PagesMetadataFileName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		want.Files = append(want.Files, summary.File{Name: name, Bytes: fi.Size()})
	}
	slices.SortFunc(want.Files, func(a, b summary.File) int { return strings.Compare(a.Name, b.Name) })
	if want.Digest == "" || want.Version != version.Version() || want.CPUFeatures != "fpu,sse,sse2" || len(want.Layers) != 1 || len(want.MemoryFiles) != 1 {
		t.Fatalf("inspect --json and layers describe %+v, want an image with a digest, this version, its CPU features, a layer and a memory file", want)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("SummaryImage differs from inspect --json and layers (-want +got):\n%s", diff)
	}
}

// TestSummaryImageSingleStateFile checks that SummaryImage describes a
// compressed checkpoint, a single state file that holds its memory too, as one
// without digest, layers or memory files.
func TestSummaryImageSingleStateFile(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, filepath.Join(dir, checkpointfiles.StateFileName), map[string]string{
		boot.VersionKey:        version.Version(),
		boot.ContainerSpecsKey: `{"app":{"ociVersion":"1.0.2"}}`,
	})
	got, err := SummaryImage(&config.Config{}, dir)
	if err != nil {
		t.Fatalf("SummaryImage: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, checkpointfiles.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	want := &summary.Image{
		Version:     version.Version(),
		Layers:      []string{},
		MemoryFiles: []summary.MemoryFile{},
		Files:       []summary.File{{Name: checkpointfiles.StateFileName, Bytes: fi.Size()}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("SummaryImage (-want +got):\n%s", diff)
	}
	// Empty lists are encoded as such, not as null.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"layers":[],"memory_files":[]`; !strings.Contains(string(b), want) {
		t.Errorf("SummaryImage encodes as %s, which lacks %s", b, want)
	}
}
