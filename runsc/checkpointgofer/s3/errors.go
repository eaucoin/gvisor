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

package s3

import (
	"errors"
	"fmt"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
)

// mapError returns the error of the operation op on obj, with the errno that
// stateipc conveys to the Sentry: ENOENT if the bucket or object does not
// exist, EACCES if the credentials may not access it, and EIO otherwise.
func mapError(err error, op string, obj object) error {
	errno := unix.EIO
	var apiErr smithy.APIError
	var respErr *awshttp.ResponseError
	switch {
	case errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NoSuchBucket"):
		errno = unix.ENOENT
	case errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDenied":
		errno = unix.EACCES
	case errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound:
		// Not every store's error responses have a code.
		errno = unix.ENOENT
	case errors.As(err, &respErr) && (respErr.HTTPStatusCode() == http.StatusForbidden || respErr.HTTPStatusCode() == http.StatusUnauthorized):
		errno = unix.EACCES
	}
	log.Warningf("s3.%s on %s failed: %v", op, obj, err)
	return fmt.Errorf("%s on %s: %w: %v", op, obj, errno, err)
}
