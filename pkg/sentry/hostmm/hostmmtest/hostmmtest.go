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

// Package hostmmtest helps tests that need the host's write tracking
// (userfaultfd write-protection in asynchronous mode, hostmm).
package hostmmtest

import (
	"os"
	"testing"
)

// RequireUffdWPEnv is the environment variable that, set to "1", makes tests
// that need write tracking fail where the host lacks it rather than skip:
// CI sets it on hosts that have it, so that a skipped test cannot pass for
// one that ran. test/hostmm's C++ tests read it too.
const RequireUffdWPEnv = "GVISOR_REQUIRE_UFFD_WP"

// Unavailable ends a test that needs write tracking, which the host lacks as
// err says: it skips the test, or fails it if RequireUffdWPEnv is "1".
func Unavailable(tb testing.TB, err error) {
	tb.Helper()
	if os.Getenv(RequireUffdWPEnv) == "1" {
		tb.Fatalf("write tracking is not available, and %s=1 requires it: %v", RequireUffdWPEnv, err)
	}
	tb.Skipf("write tracking is not available: %v", err)
}
