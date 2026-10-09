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

package gcs

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"golang.org/x/oauth2"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
)

// TestOpenReadLayers checks that the files of an image's layers can be
// opened for reading, as the image's own pages files can.
func TestOpenReadLayers(t *testing.T) {
	s, err := NewFileServer(context.Background(), &FileServerOptions{
		AllowCheckpointReads: true,
		TokenSource:          oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token"}),
		Bucket:               "bucket",
	})
	if err != nil {
		t.Fatalf("NewFileServer: %v", err)
	}
	defer s.Destroy()
	d := checkpointimage.Digest{1}
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{path: checkpointimage.LayerPath(d, checkpointfiles.PagesMetadataFileName), ok: true},
		{path: checkpointimage.LayerPath(d, checkpointfiles.PagesFileName), ok: true},
		{path: checkpointimage.LayerPath(d, checkpointfiles.StateFileName)},
		{path: "layers/1/" + checkpointfiles.PagesFileName},
	} {
		r, err := s.OpenRead(tc.path)
		if err == nil {
			r.Close()
		}
		if tc.ok != (err == nil) || (err != nil && !errors.Is(err, fs.ErrPermission)) {
			t.Errorf("OpenRead(%q): %v, want success %t", tc.path, err, tc.ok)
		}
	}
}
