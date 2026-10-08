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
	"fmt"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	"gvisor.dev/gvisor/pkg/shim/v1/utils"
)

// Restore annotations let a pod be restored from a checkpoint by Kubernetes
// versions whose CRI has no RestorePod, which would otherwise set
// CreateTaskRequest.checkpoint: containerd passes pod annotations to the
// containers' specs when its runtime configuration lists them in
// pod_annotations. They select the same restore as a
// CreateTaskRequest.checkpoint does.
const (
	// restoreAnnotationPrefix prefixes the restore annotations. A spec that
	// carries an annotation with this prefix that is not one of the following
	// is refused, so that a misspelt one cannot make a restore start the
	// container afresh instead.
	restoreAnnotationPrefix = "dev.gvisor.internal.restore."

	// restoreImagePathAnnotation is the host path of the checkpoint to restore
	// the container from. Like a CreateTaskRequest.checkpoint, it must exist
	// when the container is created.
	restoreImagePathAnnotation = restoreAnnotationPrefix + "host-image-path"

	// restoreDirectAnnotation, if "true", restores with `runsc restore
	// --direct`, as the restore_direct option does.
	restoreDirectAnnotation = restoreAnnotationPrefix + "direct"

	// restoreBackgroundAnnotation, if "true", restores with `runsc restore
	// --background`, as the restore_background option does.
	restoreBackgroundAnnotation = restoreAnnotationPrefix + "background"
)

// restoreAnnotations is the restore that a container's spec annotations
// select.
type restoreAnnotations struct {
	imagePath  string
	direct     bool
	background bool
}

// takeRestoreAnnotations returns the restore that the annotations of the
// container's spec in bundle select, and removes them from the spec, which
// they do not belong to: runsc never sees them.
func takeRestoreAnnotations(bundle string) (restoreAnnotations, error) {
	var r restoreAnnotations
	spec, err := utils.ReadSpec(bundle)
	if err != nil {
		return r, fmt.Errorf("read oci spec: %w", err)
	}
	found := false
	for key, value := range spec.Annotations {
		if !strings.HasPrefix(key, restoreAnnotationPrefix) {
			continue
		}
		found = true
		switch key {
		case restoreImagePathAnnotation:
			r.imagePath = value
		case restoreDirectAnnotation:
			r.direct, err = strconv.ParseBool(value)
		case restoreBackgroundAnnotation:
			r.background, err = strconv.ParseBool(value)
		default:
			err = errdefs.ErrInvalidArgument
		}
		if err != nil {
			return r, fmt.Errorf("annotation %s=%q: %w", key, value, err)
		}
		delete(spec.Annotations, key)
	}
	if !found {
		return r, nil
	}
	if r.imagePath == "" && (r.direct || r.background) {
		return r, fmt.Errorf("restore annotations without %s: %w", restoreImagePathAnnotation, errdefs.ErrInvalidArgument)
	}
	if err := utils.WriteSpec(bundle, spec); err != nil {
		return r, fmt.Errorf("write oci spec: %w", err)
	}
	return r, nil
}
