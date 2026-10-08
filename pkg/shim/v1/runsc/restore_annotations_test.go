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

package runsc

import (
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/shim/v1/utils"
)

func TestTakeRestoreAnnotations(t *testing.T) {
	const other = "io.kubernetes.cri.container-type"
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        restoreAnnotations
		wantErr     bool
	}{
		{
			name:        "none",
			annotations: map[string]string{other: "sandbox"},
		},
		{
			name: "all",
			annotations: map[string]string{
				other:                       "sandbox",
				restoreImagePathAnnotation:  "/ckpt",
				restoreDirectAnnotation:     "true",
				restoreBackgroundAnnotation: "true",
			},
			want: restoreAnnotations{imagePath: "/ckpt", direct: true, background: true},
		},
		{
			name:        "image path only",
			annotations: map[string]string{restoreImagePathAnnotation: "/ckpt"},
			want:        restoreAnnotations{imagePath: "/ckpt"},
		},
		{
			name: "not a boolean",
			annotations: map[string]string{
				restoreImagePathAnnotation:  "/ckpt",
				restoreBackgroundAnnotation: "yes",
			},
			wantErr: true,
		},
		{
			name: "misspelt",
			annotations: map[string]string{
				restoreAnnotationPrefix + "host-image-paths": "/ckpt",
			},
			wantErr: true,
		},
		{
			name:        "no image path",
			annotations: map[string]string{restoreBackgroundAnnotation: "true"},
			wantErr:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			if err := utils.WriteSpec(bundle, &specs.Spec{Annotations: tc.annotations}); err != nil {
				t.Fatalf("WriteSpec: %v", err)
			}
			got, err := takeRestoreAnnotations(bundle)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("takeRestoreAnnotations() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("takeRestoreAnnotations() failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("takeRestoreAnnotations() = %+v, want %+v", got, tc.want)
			}
			spec, err := utils.ReadSpec(bundle)
			if err != nil {
				t.Fatalf("ReadSpec: %v", err)
			}
			for key := range spec.Annotations {
				if key != other {
					t.Errorf("annotation %q left in the spec", key)
				}
			}
			if _, ok := spec.Annotations[other]; !ok && tc.annotations[other] != "" {
				t.Errorf("annotation %q removed from the spec", other)
			}
		})
	}
}
