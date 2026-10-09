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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/subcommands"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/flag"
)

// inspect implements subcommands.Command for the "image inspect" command.
type inspect struct {
	layerPaths stringSlice
	json       bool
}

// Name implements subcommands.Command.
func (*inspect) Name() string {
	return "inspect"
}

// Synopsis implements subcommands.Command.
func (*inspect) Synopsis() string {
	return "describe a checkpoint image"
}

// Usage implements subcommands.Command.
func (*inspect) Usage() string {
	return `inspect [--json] [--layer-path=DIR]... IMAGE - describe the checkpoint image in the directory IMAGE: its identity, format, state file metadata, layers, memory and working set.
`
}

// SetFlags implements subcommands.Command.
func (i *inspect) SetFlags(f *flag.FlagSet) {
	f.Var(&i.layerPaths, "layer-path", "directory in which to look for the image's layers besides IMAGE/layers, as for runsc restore; can be repeated, or given comma-separated")
	f.BoolVar(&i.json, "json", false, "print a JSON object")
}

// Execute implements subcommands.Command.
func (i *inspect) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() != 1 {
		f.Usage()
		return subcommands.ExitUsageError
	}
	d, err := openImageDir(f.Arg(0), i.layerPaths)
	if err != nil {
		return exitStatus(err)
	}
	info := describe(d)
	if i.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			return exitStatus(err)
		}
		return subcommands.ExitSuccess
	}
	info.print(os.Stdout)
	return subcommands.ExitSuccess
}

// imageInfo is the output of "image inspect".
type imageInfo struct {
	Digest      string           `json:"digest"`
	Format      formatInfo       `json:"format"`
	State       stateInfo        `json:"state"`
	Layers      []layerInfo      `json:"layers"`
	MemoryFiles []memoryFileInfo `json:"memory_files"`
	WorkingSet  *workingSetInfo  `json:"working_set,omitempty"`
}

type formatInfo struct {
	Major uint16 `json:"major"`
	Minor uint16 `json:"minor"`
}

type stateInfo struct {
	Size int64 `json:"size"`
	// Metadata is the state file's metadata, but for the container specs,
	// which are long and printed by "runsc spec" in a better form.
	Metadata map[string]string `json:"metadata"`
}

type layerInfo struct {
	// Digest is empty for layer 0, the image itself.
	Digest    string `json:"digest,omitempty"`
	PagesSize uint64 `json:"pages_size"`
	// Path is the path of the layer's pages file, or empty if it was not
	// found.
	Path string `json:"path,omitempty"`
}

type memoryFileInfo struct {
	// Owner is "" for the application MemoryFile, and "container:path" for
	// private ones.
	Owner          string   `json:"owner"`
	CommittedBytes uint64   `json:"committed_bytes"`
	Extents        int      `json:"extents"`
	LayerBytes     []uint64 `json:"layer_bytes"`
}

type workingSetInfo struct {
	Unit    uint64        `json:"unit"`
	Window  time.Duration `json:"window_ns"`
	Extents int           `json:"extents"`
	Bytes   uint64        `json:"bytes"`
}

func describe(d *imageDir) *imageInfo {
	img := d.img
	info := &imageInfo{
		Digest: img.Digest.String(),
		Format: formatInfo{Major: img.Header.Major, Minor: img.Header.Minor},
		State:  stateInfo{Size: d.stateSize, Metadata: make(map[string]string)},
	}
	for k, v := range d.state {
		if k != boot.ContainerSpecsKey && !strings.HasPrefix(k, "_") {
			info.State.Metadata[k] = v
		}
	}
	for i, l := range img.Layers() {
		li := layerInfo{PagesSize: l.PagesSize, Path: d.pagesPath(i)}
		if i != 0 {
			li.Digest = l.Digest.String()
		}
		info.Layers = append(info.Layers, li)
	}
	for i, mf := range img.MemoryFiles {
		mi := memoryFileInfo{
			CommittedBytes: img.CommittedBytes(i),
			Extents:        len(mf.GetExtents()),
			LayerBytes:     make([]uint64, len(info.Layers)),
		}
		if i != 0 {
			owner := img.Proto.GetPrivateMemoryFiles()[i-1]
			mi.Owner = owner.GetContainerName() + ":" + owner.GetPath()
		}
		for _, e := range mf.GetExtents() {
			mi.LayerBytes[e.GetLayer()] += e.GetEnd() - e.GetStart()
		}
		info.MemoryFiles = append(info.MemoryFiles, mi)
	}
	if ws := img.Proto.GetWorkingSet(); ws != nil {
		wi := &workingSetInfo{
			Unit:    ws.GetUnit(),
			Window:  time.Duration(ws.GetWindowNs()),
			Extents: len(ws.GetExtents()),
		}
		for _, fr := range ws.GetExtents() {
			wi.Bytes += fr.GetEnd() - fr.GetStart()
		}
		info.WorkingSet = wi
	}
	return info
}

func (info *imageInfo) print(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "Image:\t%s\n", info.Digest)
	fmt.Fprintf(tw, "Format:\t%d.%d\n", info.Format.Major, info.Format.Minor)
	for _, k := range []string{boot.VersionKey, boot.PlatformKey, boot.CPUFeaturesKey, "timestamp"} {
		if v, ok := info.State.Metadata[k]; ok {
			fmt.Fprintf(tw, "%s:\t%s\n", k, v)
		}
	}
	fmt.Fprintf(tw, "State file:\t%d bytes\n", info.State.Size)
	tw.Flush()

	fmt.Fprintf(w, "\nLayers:\n")
	tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "  #\tDIGEST\tPAGES\tPAGES FILE\n")
	for i, l := range info.Layers {
		digest := l.Digest
		if i == 0 {
			digest = "(this image)"
		}
		path := l.Path
		if path == "" {
			path = "NOT FOUND"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%d\t%s\n", i, digest, l.PagesSize, path)
	}
	tw.Flush()

	fmt.Fprintf(w, "\nMemory files:\n")
	tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "  OWNER\tCOMMITTED\tEXTENTS\tBYTES PER LAYER\n")
	for _, mf := range info.MemoryFiles {
		owner := mf.Owner
		if owner == "" {
			owner = "(application)"
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%v\n", owner, mf.CommittedBytes, mf.Extents, mf.LayerBytes)
	}
	tw.Flush()

	if ws := info.WorkingSet; ws != nil {
		fmt.Fprintf(w, "\nWorking set: %d bytes in %d extents, recorded for %v in units of %d bytes\n", ws.Bytes, ws.Extents, ws.Window, ws.Unit)
	}
}
