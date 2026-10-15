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
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/google/go-cmp/cmp"
	"github.com/google/subcommands"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3/s3test"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/version"
)

// remotePrefix is the prefix of the objects of the images that the tests put
// in their store.
const remotePrefix = "image/"

// remoteImage returns a store, and a directory holding the S3 options file
// that selects the image under remotePrefix in it, and its layers under
// layersPrefix if it is not empty, which the image commands read through a
// checkpoint gofer that serves the store in this process.
func remoteImage(t *testing.T, layersPrefix string) (*s3test.Store, string) {
	t.Helper()
	store := s3test.NewStore(t)
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	opts := fmt.Sprintf(`{"endpoint": %q, "bucket": %q, "object_prefix": %q, "layers_prefix": %q}`, srv.URL, s3test.Bucket, remotePrefix, layersPrefix)
	if err := os.WriteFile(filepath.Join(dir, "s3_opts.json"), []byte(opts), 0600); err != nil {
		t.Fatal(err)
	}

	start := startCheckpointGofer
	t.Cleanup(func() { startCheckpointGofer = start })
	startCheckpointGofer = func(_ *config.Config, imagePath string) (*os.File, error) {
		if imagePath != dir {
			return nil, nil
		}
		fs, err := s3.NewFileServer(context.Background(), &s3.FileServerOptions{
			AllowCheckpointReads: true,
			Endpoint:             srv.URL,
			Bucket:               s3test.Bucket,
			ObjectPrefix:         remotePrefix,
			LayersPrefix:         layersPrefix,
			UsePathStyle:         true,
			Credentials:          credentials.NewStaticCredentialsProvider("key", "secret", ""),
		})
		if err != nil {
			return nil, err
		}
		t.Cleanup(fs.Destroy)
		afs, err := stateipc.NewAsyncFileServer(fs)
		if err != nil {
			return nil, err
		}
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		sock, err := unet.NewSocket(fds[1])
		if err != nil {
			unix.Close(fds[0])
			unix.Close(fds[1])
			return nil, err
		}
		server := urpc.NewServer()
		server.Register(afs)
		server.StartHandling(sock)
		return os.NewFile(uintptr(fds[0]), "checkpointgofer-socket"), nil
	}
	return store, dir
}

// putImage puts the files of the image directory dir, and of the layers in its
// layers/ directory, in store under remotePrefix, but those that skip returns
// true for, given their path relative to dir.
func putImage(t *testing.T, store *s3test.Store, dir string, skip func(rel string) bool) {
	t.Helper()
	// filepath.Walk does not follow the symbolic links of layers/.
	var put func(rel string)
	put = func(rel string) {
		path := filepath.Join(dir, rel)
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				put(filepath.Join(rel, e.Name()))
			}
			return
		}
		if skip != nil && skip(rel) {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		store.SetObject(remotePrefix+rel, data)
	}
	put(".")
}

// inspectJSON runs inspect --json on image, and returns its output decoded.
func inspectJSON(t *testing.T, image string) *imageInfo {
	t.Helper()
	status, out := run(t, new(inspect), "--json", image)
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect --json %s: %v", image, status)
	}
	var info imageInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("inspect --json printed %q: %v", out, err)
	}
	return &info
}

// TestInspectRemote checks that inspect describes an image in an object store
// as it describes the same image in a directory, but for its paths and the
// size of its state file, without reading its pages; and that layers lists its
// layers.
func TestInspectRemote(t *testing.T) {
	root, template, _ := testImages(t)
	deltaDir := filepath.Join(root, "delta")
	store, dir := remoteImage(t, "")
	putImage(t, store, deltaDir, nil)

	want := inspectJSON(t, deltaDir)
	want.State.Size = 0
	uri := "s3://" + s3test.Bucket + "/" + remotePrefix
	want.Layers[0].Path = uri + checkpointfiles.PagesFileName
	want.Layers[1].Path = uri + checkpointimage.LayerPath(template.Digest, checkpointfiles.PagesFileName)
	if diff := cmp.Diff(want, inspectJSON(t, dir)); diff != "" {
		t.Errorf("inspect --json of the image in the store differs from its description in a directory (-want +got):\n%s", diff)
	}
	for _, r := range store.Requests() {
		if strings.HasSuffix(r.Key, "/"+checkpointfiles.PagesFileName) {
			t.Errorf("inspect read a pages file: %+v", r)
		}
	}

	status, out := run(t, new(inspect), dir)
	if status != subcommands.ExitSuccess {
		t.Fatalf("inspect: %v", status)
	}
	for _, want := range []string{"Image:          ", "Containers:     app, sidecar\n", want.Layers[1].Path + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect printed:\n%s\nwhich lacks %q", out, want)
		}
	}
	if strings.Contains(out, "State file:") {
		t.Errorf("inspect printed:\n%s\nwith a state file size, which the store does not report", out)
	}

	if status, out := run(t, new(layers), dir); status != subcommands.ExitSuccess || out != template.Digest.String()+"\n" {
		t.Errorf("layers = %v, %q; want %q", status, out, template.Digest.String()+"\n")
	}
}

// TestInspectRemoteLayersPrefix checks that inspect and layers read the layers
// of an image in an object store under the layers prefix of its options, where
// the store keeps each image once, by its digest, and show them there.
func TestInspectRemoteLayersPrefix(t *testing.T) {
	root, template, _ := testImages(t)
	deltaDir := filepath.Join(root, "delta")
	const layersPrefix = "layers/"
	store, dir := remoteImage(t, layersPrefix)
	putImage(t, store, deltaDir, func(rel string) bool {
		return strings.HasPrefix(rel, checkpointimage.LayersDir+"/")
	})
	layerDir := filepath.Join(deltaDir, checkpointimage.LayersDir, template.Digest.String())
	for _, name := range []string{checkpointfiles.PagesMetadataFileName, checkpointfiles.PagesFileName} {
		data, err := os.ReadFile(filepath.Join(layerDir, name))
		if err != nil {
			t.Fatal(err)
		}
		store.SetObject(layersPrefix+template.Digest.String()+"/"+name, data)
	}

	want := inspectJSON(t, deltaDir)
	want.State.Size = 0
	want.Layers[0].Path = "s3://" + s3test.Bucket + "/" + remotePrefix + checkpointfiles.PagesFileName
	want.Layers[1].Path = "s3://" + s3test.Bucket + "/" + layersPrefix + template.Digest.String() + "/" + checkpointfiles.PagesFileName
	if diff := cmp.Diff(want, inspectJSON(t, dir)); diff != "" {
		t.Errorf("inspect --json of the image in the store differs from its description in a directory (-want +got):\n%s", diff)
	}
	if status, out := run(t, new(layers), dir); status != subcommands.ExitSuccess || out != template.Digest.String()+"\n" {
		t.Errorf("layers = %v, %q; want %q", status, out, template.Digest.String()+"\n")
	}
}

// TestInspectRemoteLayerNotFound checks that inspect reports a layer that the
// store does not hold as not found.
func TestInspectRemoteLayerNotFound(t *testing.T) {
	root, _, _ := testImages(t)
	store, dir := remoteImage(t, "")
	putImage(t, store, filepath.Join(root, "delta"), func(rel string) bool {
		return strings.HasPrefix(rel, checkpointimage.LayersDir+"/")
	})
	info := inspectJSON(t, dir)
	if len(info.Layers) != 2 || info.Layers[1].Path != "" {
		t.Errorf("inspect --json: layers %+v, want the second not found", info.Layers)
	}
}

// TestInspectRemoteWrongLayer checks that inspect refuses, as an invalid image,
// an image whose layer in the store is another image.
func TestInspectRemoteWrongLayer(t *testing.T) {
	root, template, _ := testImages(t)
	store, dir := remoteImage(t, "")
	deltaDir := filepath.Join(root, "delta")
	putImage(t, store, deltaDir, nil)
	meta, err := os.ReadFile(filepath.Join(deltaDir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	store.SetObject(remotePrefix+checkpointimage.LayerPath(template.Digest, checkpointfiles.PagesMetadataFileName), meta)
	if status, out := run(t, new(inspect), dir); status != exitInvalid {
		t.Errorf("inspect = %v, want %v; output:\n%s", status, exitInvalid, out)
	}
}

// TestInspectRemoteSingleStateFile checks that inspect describes a single
// state file in an object store by its metadata and containers.
func TestInspectRemoteSingleStateFile(t *testing.T) {
	local := t.TempDir()
	writeStateFile(t, filepath.Join(local, checkpointfiles.StateFileName), map[string]string{
		boot.VersionKey:        version.Version(),
		boot.ContainerSpecsKey: `{"app":{"ociVersion":"1.0.2"}}`,
	})
	store, dir := remoteImage(t, "")
	putImage(t, store, local, nil)
	info := inspectJSON(t, dir)
	if info.Digest != "" || len(info.Layers) != 0 || !slices.Equal(info.Containers, []string{"app"}) {
		t.Errorf("inspect --json: %+v, want no digest or layers, and the container app", info)
	}
}

// TestRemoteRefused checks that the commands that read pages files, and
// inspect given layer paths, refuse an image in an object store.
func TestRemoteRefused(t *testing.T) {
	root, _, _ := testImages(t)
	store, dir := remoteImage(t, "")
	putImage(t, store, filepath.Join(root, "delta"), nil)
	for _, tc := range []struct {
		cmd  subcommands.Command
		args []string
	}{
		{cmd: new(inspect), args: []string{"--layer-path", root}},
		{cmd: new(verify)},
		{cmd: new(flatten), args: []string{"--output", filepath.Join(t.TempDir(), "out")}},
	} {
		if status, out := run(t, tc.cmd, append(tc.args, dir)...); status != subcommands.ExitFailure {
			t.Errorf("%s %v = %v, want %v; output:\n%s", tc.cmd.Name(), tc.args, status, subcommands.ExitFailure, out)
		}
	}
}
