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

package boot

import (
	"bytes"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	pb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
)

// slowStore implements stateipc.AsyncFileServerImpl by serving files from
// memory, each read answered after delay, as an object store answers each
// request after its first byte's latency.
type slowStore struct {
	files map[string][]byte
	delay time.Duration
}

// Destroy implements stateipc.AsyncFileServerImpl.Destroy.
func (s *slowStore) Destroy() {}

// OpenRead implements stateipc.AsyncFileServerImpl.OpenRead.
func (s *slowStore) OpenRead(path string) (stateio.AsyncReader, error) {
	data, ok := s.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return stateio.NewIOReader(delayedReaderAt{bytes.NewReader(data), s.delay}, 64<<10 /* maxReadBytes */, 1 /* maxRanges */, 16 /* maxParallel */), nil
}

// OpenWrite implements stateipc.AsyncFileServerImpl.OpenWrite.
func (s *slowStore) OpenWrite(path string) (stateio.AsyncWriter, error) {
	return nil, fs.ErrPermission
}

// delayedReaderAt is an io.ReaderAt whose every read takes delay.
type delayedReaderAt struct {
	r     io.ReaderAt
	delay time.Duration
}

// ReadAt implements io.ReaderAt.ReadAt.
func (r delayedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	time.Sleep(r.delay)
	return r.r.ReadAt(p, off)
}

// serveCheckpoint serves impl over stateipc as a checkpoint gofer does, and
// returns the client's end of the connection.
func serveCheckpoint(t *testing.T, impl stateipc.AsyncFileServerImpl) *os.File {
	t.Helper()
	server, err := stateipc.NewAsyncFileServer(impl)
	if err != nil {
		t.Fatalf("NewAsyncFileServer: %v", err)
	}
	usrv := urpc.NewServer()
	usrv.Register(server)
	clientSock, serverSock, err := unet.SocketPair(false /* packet */)
	if err != nil {
		t.Fatalf("SocketPair: %v", err)
	}
	usrv.StartHandling(serverSock)
	t.Cleanup(func() { usrv.Stop(0) })
	clientFD, err := clientSock.Release()
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	client := os.NewFile(uintptr(clientFD), "checkpoint gofer")
	t.Cleanup(func() { client.Close() })
	return client
}

// testPagesMetadata returns the pages metadata file of an image of one page
// of its own and one page of each of parents, its other layers, and the
// image's digest.
func testPagesMetadata(t *testing.T, parents ...checkpointimage.Digest) ([]byte, checkpointimage.Digest) {
	t.Helper()
	const page = checkpointimage.PageSize
	pages := uint64(1 + len(parents))
	ip := &pb.ImageProto{
		Layers:           []*pb.LayerProto{{}},
		PageHash:         checkpointimage.PageHashXXH64,
		PageHashesDigest: make([]byte, sha256.Size),
	}
	mf := &pb.MemoryFileMetadataProto{
		Version:    checkpointimage.MemoryFileMetadataVersion,
		MemAcct:    []*pb.MemAcctRangeProto{{Start: 0, End: pages * page, KnownCommitted: true}},
		Extents:    []*pb.ExtentProto{{Start: 0, End: page}},
		PageHashes: make([]byte, 8*pages),
	}
	for i, d := range parents {
		layer := uint64(i + 1)
		ip.Layers = append(ip.Layers, &pb.LayerProto{Digest: d[:], PagesSize: page})
		mf.Extents = append(mf.Extents, &pb.ExtentProto{Start: layer * page, End: (layer + 1) * page, Layer: uint32(layer)})
	}
	var buf bytes.Buffer
	w := checkpointimage.NewWriter(&buf, ip)
	if err := checkpointimage.WriteRecord(w, mf); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	img, err := w.Finish(page)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return buf.Bytes(), img.Digest
}

// testCheckpointFiles returns the files of a checkpoint whose image has three
// layers: the image, its parent and its parent's parent.
func testCheckpointFiles(t *testing.T, state []byte) (map[string][]byte, []checkpointimage.Digest) {
	t.Helper()
	grandparent, grandparentDigest := testPagesMetadata(t)
	parent, parentDigest := testPagesMetadata(t, grandparentDigest)
	top, _ := testPagesMetadata(t, parentDigest, grandparentDigest)
	page := make([]byte, checkpointimage.PageSize)
	files := map[string][]byte{
		checkpointfiles.StateFileName:         state,
		checkpointfiles.PagesMetadataFileName: top,
		checkpointfiles.PagesFileName:         page,
	}
	for d, meta := range map[checkpointimage.Digest][]byte{parentDigest: parent, grandparentDigest: grandparent} {
		files[checkpointimage.LayerPath(d, checkpointfiles.PagesMetadataFileName)] = meta
		files[checkpointimage.LayerPath(d, checkpointfiles.PagesFileName)] = page
	}
	return files, []checkpointimage.Digest{parentDigest, grandparentDigest}
}

// TestRestoreReadersForCheckpointGoferWaits checks that the readers of a
// restore through a checkpoint gofer are ready, and the state file's first
// bytes read, after two waits for the store's first byte whatever the image's
// depth: the state file and the pages metadata at once, then the metadata of
// every other layer and the end of every layer's pages file at once.
func TestRestoreReadersForCheckpointGoferWaits(t *testing.T) {
	const delay = 300 * time.Millisecond
	state := []byte("the state file")
	files, _ := testCheckpointFiles(t, state)
	o := &sentryapi.RestoreOpts{UseCheckpointGofer: true}
	o.Files = []*os.File{serveCheckpoint(t, &slowStore{files: files, delay: delay})}

	start := time.Now()
	stateFile, image, pagesFiles, err := getRestoreReadersForCheckpointGofer(o)
	if err != nil {
		t.Fatalf("getRestoreReadersForCheckpointGofer: %v", err)
	}
	defer stateFile.Close()
	defer func() {
		for _, pagesFile := range pagesFiles {
			pagesFile.Close()
		}
	}()
	got := make([]byte, len(state))
	if _, err := io.ReadFull(stateFile, got); err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	took := time.Since(start)
	if !bytes.Equal(got, state) {
		t.Errorf("state file reads %q, want %q", got, state)
	}
	if !o.HavePagesFile || len(image.Layers()) != 3 || len(pagesFiles) != 3 {
		t.Errorf("got HavePagesFile %t, %d layers and %d pages files, want true, 3 and 3", o.HavePagesFile, len(image.Layers()), len(pagesFiles))
	}
	// One after another, the reads would take 4 delays: the pages metadata,
	// each other layer's, then the state file.
	if took >= delay*5/2 {
		t.Errorf("readers ready and the state file's first bytes read after %v, want less than %v (2 reads of %v each)", took, delay*5/2, delay)
	}
}

// TestRestoreReadersForCheckpointGoferLayers checks that a restore through a
// checkpoint gofer fails before any page is loaded where a layer's pages
// metadata is not the layer's or is missing, or where a layer's pages file is
// missing or shorter than the image says.
func TestRestoreReadersForCheckpointGoferLayers(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(files map[string][]byte, layers []checkpointimage.Digest)
		want string
	}{
		{
			name: "a parent's metadata another image's",
			edit: func(files map[string][]byte, layers []checkpointimage.Digest) {
				meta := checkpointimage.LayerPath(layers[1], checkpointfiles.PagesMetadataFileName)
				files[meta] = files[checkpointimage.LayerPath(layers[0], checkpointfiles.PagesMetadataFileName)]
			},
			want: "layer 2: ",
		},
		{
			name: "a parent's metadata missing",
			edit: func(files map[string][]byte, layers []checkpointimage.Digest) {
				delete(files, checkpointimage.LayerPath(layers[0], checkpointfiles.PagesMetadataFileName))
			},
			want: "layer 1 (",
		},
		{
			name: "a parent's pages file missing",
			edit: func(files map[string][]byte, layers []checkpointimage.Digest) {
				delete(files, checkpointimage.LayerPath(layers[1], checkpointfiles.PagesFileName))
			},
			want: "layer 2: failed to open ",
		},
		{
			name: "a parent's pages file shorter",
			edit: func(files map[string][]byte, layers []checkpointimage.Digest) {
				pages := checkpointimage.LayerPath(layers[0], checkpointfiles.PagesFileName)
				files[pages] = files[pages][:checkpointimage.PageSize-1]
			},
			want: "layer 1: layers/",
		},
		{
			name: "its own pages file shorter",
			edit: func(files map[string][]byte, layers []checkpointimage.Digest) {
				files[checkpointfiles.PagesFileName] = nil
			},
			want: "layer 0: pages.img: reading its last byte",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, layers := testCheckpointFiles(t, []byte("the state file"))
			tc.edit(files, layers)
			o := &sentryapi.RestoreOpts{UseCheckpointGofer: true}
			o.Files = []*os.File{serveCheckpoint(t, &slowStore{files: files})}
			stateFile, _, pagesFiles, err := getRestoreReadersForCheckpointGofer(o)
			if err == nil {
				stateFile.Close()
				for _, pagesFile := range pagesFiles {
					pagesFile.Close()
				}
				t.Fatalf("getRestoreReadersForCheckpointGofer succeeded, want an error")
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("getRestoreReadersForCheckpointGofer: %v, want an error starting %q", err, tc.want)
			}
		})
	}
}
