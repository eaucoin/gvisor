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
	"fmt"
	"io"

	"github.com/google/subcommands"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/runsc/flag"
)

// rewriteFlags are the flags common to the commands that rewrite an image.
type rewriteFlags struct {
	layerPaths      stringSlice
	output          string
	workingSetFirst bool
}

func (r *rewriteFlags) setFlags(f *flag.FlagSet) {
	f.Var(&r.layerPaths, "layer-path", "directory in which to look for the image's layers besides IMAGE/layers, as for runsc restore; can be repeated, or given comma-separated")
	f.StringVar(&r.output, "output", "", "directory of the new image, which must not exist")
	f.BoolVar(&r.workingSetFirst, "working-set-first", false, "put the pages of the working set recorded in the image first in the new pages file, in the order they were touched, so that a restore loads them first")
}

// checkArgs returns false if f's arguments are not one image directory and an
// output directory.
func (r *rewriteFlags) checkArgs(f *flag.FlagSet) bool {
	if f.NArg() != 1 || r.output == "" {
		f.Usage()
		return false
	}
	return true
}

// run rewrites the image in the directory named by f's argument into
// r.output, with opts.
func (r *rewriteFlags) run(f *flag.FlagSet, opts checkpointimage.RewriteOpts) subcommands.ExitStatus {
	opts.WorkingSetFirst = r.workingSetFirst
	d, err := openImageDir(f.Arg(0), r.layerPaths)
	if err != nil {
		return exitStatus(err)
	}
	files, err := d.openLayers()
	if err != nil {
		return exitStatus(err)
	}
	defer closeAll(files)
	img, err := writeImageDir(r.output, d, func(pages, meta io.Writer) (*checkpointimage.Image, error) {
		return checkpointimage.Rewrite(d.img, readersOf(files), opts, pages, meta)
	})
	if err != nil {
		return exitStatus(err)
	}
	fmt.Printf("Image %v: %d bytes of pages\n", img.Digest, img.Layers()[0].PagesSize)
	for _, l := range img.Layers()[1:] {
		fmt.Printf("  layer %v\n", l.Digest)
	}
	return subcommands.ExitSuccess
}

// keepLayers parses --keep-layer flags.
func keepLayers(digests []string) (map[checkpointimage.Digest]struct{}, error) {
	keep := make(map[checkpointimage.Digest]struct{})
	for _, s := range digests {
		d, err := checkpointimage.ParseDigest(s)
		if err != nil {
			return nil, err
		}
		keep[d] = struct{}{}
	}
	return keep, nil
}

// flatten implements subcommands.Command for the "image flatten" command.
type flatten struct {
	rewriteFlags
}

// Name implements subcommands.Command.
func (*flatten) Name() string {
	return "flatten"
}

// Synopsis implements subcommands.Command.
func (*flatten) Synopsis() string {
	return "copy a checkpoint image and the data of its layers into an image without layers"
}

// Usage implements subcommands.Command.
func (*flatten) Usage() string {
	return `flatten --output=DIR [--layer-path=DIR]... [--working-set-first] IMAGE - write to DIR an image with the memory of the checkpoint image IMAGE, all of it in its own pages file.
`
}

// SetFlags implements subcommands.Command.
func (c *flatten) SetFlags(f *flag.FlagSet) {
	c.setFlags(f)
}

// Execute implements subcommands.Command.
func (c *flatten) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if !c.checkArgs(f) {
		return subcommands.ExitUsageError
	}
	return c.run(f, checkpointimage.RewriteOpts{})
}

// compact implements subcommands.Command for the "image compact" command.
type compact struct {
	rewriteFlags
	keep stringSlice
}

// Name implements subcommands.Command.
func (*compact) Name() string {
	return "compact"
}

// Synopsis implements subcommands.Command.
func (*compact) Synopsis() string {
	return "rewrite a checkpoint image, copying the data of all but some of its layers"
}

// Usage implements subcommands.Command.
func (*compact) Usage() string {
	return `compact --output=DIR [--keep-layer=DIGEST]... [--layer-path=DIR]... [--working-set-first] IMAGE - write to DIR an image with the memory of the checkpoint image IMAGE. Pages in the layers given by --keep-layer stay there: the new image refers to them. Others are copied into its pages file, which holds only the data that the image refers to: data written over, e.g. by pre-copy rounds, is dropped.
`
}

// SetFlags implements subcommands.Command.
func (c *compact) SetFlags(f *flag.FlagSet) {
	c.setFlags(f)
	f.Var(&c.keep, "keep-layer", "digest of a layer of IMAGE that the new image keeps as a layer; can be repeated, or given comma-separated")
}

// Execute implements subcommands.Command.
func (c *compact) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if !c.checkArgs(f) {
		return subcommands.ExitUsageError
	}
	keep, err := keepLayers(c.keep)
	if err != nil {
		return exitStatus(err)
	}
	return c.run(f, checkpointimage.RewriteOpts{Keep: keep})
}

// rebase implements subcommands.Command for the "image rebase" command.
type rebase struct {
	rewriteFlags
	keep stringSlice
	onto string
}

// Name implements subcommands.Command.
func (*rebase) Name() string {
	return "rebase"
}

// Synopsis implements subcommands.Command.
func (*rebase) Synopsis() string {
	return "rewrite a checkpoint image as a delta of another one"
}

// Usage implements subcommands.Command.
func (*rebase) Usage() string {
	return `rebase --onto=BASE --output=DIR [--keep-layer=DIGEST]... [--layer-path=DIR]... [--working-set-first] IMAGE - write to DIR an image with the memory of the checkpoint image IMAGE whose pages refer to the image BASE wherever BASE has the same page at the same place, as for much of the memory of a sandbox restored from BASE; BASE becomes a layer of the new image. Other pages are copied, as by compact.
`
}

// SetFlags implements subcommands.Command.
func (c *rebase) SetFlags(f *flag.FlagSet) {
	c.setFlags(f)
	f.Var(&c.keep, "keep-layer", "digest of a layer of IMAGE that the new image keeps as a layer; can be repeated, or given comma-separated")
	f.StringVar(&c.onto, "onto", "", "directory of the image to rebase onto")
}

// Execute implements subcommands.Command.
func (c *rebase) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if !c.checkArgs(f) {
		return subcommands.ExitUsageError
	}
	if c.onto == "" {
		f.Usage()
		return subcommands.ExitUsageError
	}
	keep, err := keepLayers(c.keep)
	if err != nil {
		return exitStatus(err)
	}
	onto, err := openImageDir(c.onto, c.layerPaths)
	if err != nil {
		return exitStatus(fmt.Errorf("--onto: %w", err))
	}
	ontoFiles, err := onto.openLayers()
	if err != nil {
		return exitStatus(fmt.Errorf("--onto: %w", err))
	}
	defer closeAll(ontoFiles)
	return c.run(f, checkpointimage.RewriteOpts{Keep: keep, Onto: onto.img, OntoLayers: readersOf(ontoFiles)})
}
