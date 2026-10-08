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

package specutils

import (
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// podSpec returns the spec of a container of a Kubernetes pod with the given
// UID, as the containerd shim hands it to runsc.
func podSpec(podUID string) *specs.Spec {
	return &specs.Spec{
		Version: specs.Version,
		Root:    &specs.Root{Path: "/run/containerd/" + podUID + "/rootfs"},
		Process: &specs.Process{
			Args: []string{"/bin/sleep", "1000"},
			Cwd:  "/",
		},
		Hostname: "pod-" + podUID,
		Annotations: map[string]string{
			CgroupParentAnnotation: "/kubepods/burstable/pod" + podUID,
		},
		Linux: &specs.Linux{
			CgroupsPath: "/kubepods/burstable/pod" + podUID + "/" + podUID + "-container",
		},
	}
}

// TestValidateSpecsAcrossPods checks which spec changes restore validation
// accepts when a sandbox is restored into a new pod.
func TestValidateSpecsAcrossPods(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mutate changes the restored pod's spec.
		mutate func(spec *specs.Spec)
		// wantErr is a substring of the expected error, or empty if the restore
		// must be accepted.
		wantErr string
	}{
		{
			name:   "new pod",
			mutate: func(*specs.Spec) {},
		},
		{
			name: "other gVisor annotation",
			mutate: func(spec *specs.Spec) {
				spec.Annotations["dev.gvisor.spec.rootfs.overlay"] = "memory"
			},
			wantErr: `"Annotations" does not match`,
		},
		{
			name: "args",
			mutate: func(spec *specs.Spec) {
				spec.Process.Args = []string{"/bin/sleep", "2000"}
			},
			wantErr: `"Args" does not match`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreSpec := podSpec("restored")
			tc.mutate(restoreSpec)
			err := validateSpecs(
				map[string]*specs.Spec{"app": podSpec("checkpointed")},
				map[string]*specs.Spec{"app": restoreSpec},
			)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("validateSpecs() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("validateSpecs() = nil, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("validateSpecs() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
