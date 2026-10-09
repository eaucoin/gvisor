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

package kernel

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
)

// Incremental saves.
//
// With dirty tracking, a save may be incremental: given the image the Kernel
// was last saved to or restored from (its parent), it writes only the pages
// dirtied since, and refers every other page to the layer of the parent that
// holds its data. The image's layers are therefore the image itself, the
// parent, and the parent's layers; checkpointimage.Writer drops those that no
// longer hold any page. This is CRIU's pre-dump chain (--prev-images-dir),
// with chains resolved at save so that restores read each page from its layer
// in one hop.

// incrementalSave is the state of an incremental save.
type incrementalSave struct {
	// parent is the parent image.
	parent *checkpointimage.Image

	// sets holds the pages dirtied since parent, for each tracked MemoryFile.
	sets map[*pgalloc.MemoryFile]*pgalloc.DirtySet

	// privateIndex maps the owners of parent's private MemoryFiles to their
	// index in parent.MemoryFiles.
	privateIndex map[checkpoint.ResourceID]int
}

// beginIncrementalSave starts an incremental save relative to the image whose
// digest is parent, which must be the image k was last saved to or restored
// from. e holds the pages dirtied since.
func (k *Kernel) beginIncrementalSave(parent checkpointimage.Digest, e *DirtyEpochResult) (*incrementalSave, error) {
	last := k.dirty.last
	if last == nil || e == nil {
		return nil, fmt.Errorf("incremental save: no parent image: the sandbox has not been saved to or restored from an image with a pages file since dirty tracking was enabled")
	}
	if last.Digest != parent {
		return nil, fmt.Errorf("incremental save: image %v is not the image the sandbox was last saved to or restored from (%v)", parent, last.Digest)
	}
	s := &incrementalSave{
		parent:       last,
		sets:         e.Sets,
		privateIndex: make(map[checkpoint.ResourceID]int),
	}
	for i, id := range last.Proto.GetPrivateMemoryFiles() {
		s.privateIndex[checkpoint.ResourceID{ContainerName: id.GetContainerName(), Path: id.GetPath()}] = i + 1
	}
	return s, nil
}

// layers returns the layers that the image being saved refers to besides
// itself: the parent, then the parent's layers. The parent's layer i is layer
// i+1 of the image being saved.
func (s *incrementalSave) layers() []*pgallocpb.LayerProto {
	parentLayers := s.parent.Layers()
	layers := []*pgallocpb.LayerProto{{
		Digest:    s.parent.Digest[:],
		PagesSize: parentLayers[0].PagesSize,
	}}
	for _, l := range parentLayers[1:] {
		layers = append(layers, &pgallocpb.LayerProto{
			Digest:    append([]byte(nil), l.Digest[:]...),
			PagesSize: l.PagesSize,
		})
	}
	return layers
}

// setBase makes opts save mf as a delta of its part of the parent image, if
// mf was tracked since the parent was saved or restored. owner identifies a
// private MemoryFile, and is nil for the application MemoryFile.
func (s *incrementalSave) setBase(opts *pgalloc.SaveOpts, mf *pgalloc.MemoryFile, owner *checkpoint.ResourceID) {
	opts.Base, opts.BaseLayers, opts.Clean = nil, nil, nil
	set, ok := s.sets[mf]
	if !ok {
		// mf was not tracked: save it whole.
		return
	}
	index := 0
	if owner != nil {
		if index, ok = s.privateIndex[*owner]; !ok {
			// mf is not in the parent: save it whole.
			return
		}
	}
	opts.Base = s.parent.MemoryFileImage(index)
	opts.BaseLayers = make([]uint32, len(s.parent.Layers()))
	for i := range opts.BaseLayers {
		opts.BaseLayers[i] = uint32(i + 1)
	}
	opts.Clean = func(off uint64) bool { return !set.Contains(off) }
}
