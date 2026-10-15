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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
	"gvisor.dev/gvisor/runsc/specutils"
)

// startCheckpointGofer starts a checkpoint gofer that reads the checkpoint
// files of the object store that the options file in dir selects, and returns
// a socket connected to it; or nil if dir holds no options file. Tests replace
// it.
var startCheckpointGofer = func(conf *config.Config, dir string) (*os.File, error) {
	return sandbox.StartCheckpointGofer(conf, nil /* cg */, dir, &specutils.LogFileOpts{
		Command:   "checkpointgofer",
		Timestamp: time.Now(),
	}, "-allow-checkpoint-reads")
}

// openRemoteImage reads, through a checkpoint gofer, the metadata of the image
// in the object store that the checkpoint gofer's options file in dir selects,
// as runsc restore reads it: its state file's metadata, its pages metadata, if
// it has any (a single state file has none), and the pages metadata of each of
// its layers, which the store holds in the image's layers/ directory and which
// identifies the layer. It returns nil if dir holds no options file.
func openRemoteImage(conf *config.Config, dir string) (*imageDir, error) {
	sock, err := startCheckpointGofer(conf, dir)
	if err != nil || sock == nil {
		return nil, err
	}
	uri := sandbox.CheckpointGoferURI(dir)
	clientFD, err := unix.Dup(int(sock.Fd()))
	sock.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to dup checkpoint gofer client FD: %w", err)
	}
	clientSock, err := unet.NewSocket(clientFD)
	if err != nil {
		unix.Close(clientFD)
		return nil, fmt.Errorf("failed to create unet.Socket for checkpoint gofer client FD: %w", err)
	}
	afc, err := stateipc.NewAsyncFileClient(urpc.NewClient(clientSock) /* transfers ownership */)
	if err != nil {
		return nil, fmt.Errorf("failed to create stateipc client: %w", err)
	}
	defer afc.DecRef()

	d := &imageDir{path: dir, store: uri}
	if d.state, err = readRemoteState(afc); err != nil {
		return nil, fmt.Errorf("%s: %w", d.displayPath(filepath.Join(dir, checkpointfiles.StateFileName)), err)
	}
	d.img, err = readRemoteMetadata(afc, checkpointfiles.PagesMetadataFileName)
	if errors.Is(err, fs.ErrNotExist) {
		// A single state file.
		return d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.displayPath(filepath.Join(dir, checkpointfiles.PagesMetadataFileName)), err)
	}
	d.layerDirs = make([]string, len(d.img.Layers())-1)
	for i, l := range d.img.Layers()[1:] {
		name := checkpointimage.LayerPath(l.Digest, checkpointfiles.PagesMetadataFileName)
		layer, err := readRemoteMetadata(afc, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("layer %d (%v): %w", i+1, l.Digest, err)
		}
		if layer.Digest != l.Digest {
			return nil, fmt.Errorf("layer %d (%v): %w: %s has digest %v", i+1, l.Digest, checkpointimage.ErrFormat, d.displayPath(filepath.Join(dir, name)), layer.Digest)
		}
		d.layerDirs[i] = filepath.Join(dir, filepath.Dir(name))
	}
	return d, nil
}

// readRemoteState reads the metadata of the state file through afc.
func readRemoteState(afc *stateipc.AsyncFileClient) (map[string]string, error) {
	f, err := afc.OpenRead(checkpointfiles.StateFileName)
	if err != nil {
		return nil, err
	}
	r, err := stateio.NewBufReader(f /* transfers ownership */, 1<<20 /* size = 1 MiB */)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return statefile.MetadataUnsafe(r)
}

// readRemoteMetadata reads, through afc, the pages metadata of the pages
// metadata file name, without its page hashes. The checkpoint gofer opens any
// file, and reports a missing one at its first read: the error then wraps
// fs.ErrNotExist.
func readRemoteMetadata(afc *stateipc.AsyncFileClient, name string) (*checkpointimage.Image, error) {
	f, err := afc.OpenRead(name)
	if err != nil {
		return nil, err
	}
	r, err := stateio.NewBufReader(f /* transfers ownership */, 1<<20 /* size = 1 MiB */)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return checkpointimage.ReadMetadata(r)
}
