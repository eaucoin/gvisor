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
	"os"
	"slices"
	"strings"

	"gvisor.dev/gvisor/pkg/sentry/state/summary"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"gvisor.dev/gvisor/runsc/config"
)

// SummaryImage describes the checkpoint image at path, an image directory, for
// the summary of the checkpoint that wrote it (runsc checkpoint
// --summary-file): as inspect --json and layers describe it, and with the
// regular files at the top of the directory.
func SummaryImage(conf *config.Config, path string) (*summary.Image, error) {
	d, closeImage, err := openImageMetadata(conf, path, nil /* layerPaths */)
	if err != nil {
		return nil, err
	}
	defer closeImage()
	info, err := describe(d)
	if err != nil {
		return nil, err
	}
	img := &summary.Image{
		Digest:      info.Digest,
		Version:     info.State.Metadata[boot.VersionKey],
		CPUFeatures: info.State.Metadata[sentryapi.CPUFeaturesKey],
		Layers:      []string{},
		MemoryFiles: []summary.MemoryFile{},
		Files:       []summary.File{},
	}
	if d.img != nil {
		// As layers prints them.
		for _, l := range d.img.Layers()[1:] {
			img.Layers = append(img.Layers, l.Digest.String())
		}
	}
	for _, mf := range info.MemoryFiles {
		img.MemoryFiles = append(img.MemoryFiles, summary.MemoryFile{LayerBytes: mf.LayerBytes})
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		img.Files = append(img.Files, summary.File{Name: e.Name(), Bytes: fi.Size()})
	}
	slices.SortFunc(img.Files, func(a, b summary.File) int { return strings.Compare(a.Name, b.Name) })
	return img, nil
}
