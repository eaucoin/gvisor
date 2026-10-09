#!/bin/bash

# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# affected_tests.sh prints the Bazel query expression for the tests of every
# package that a changed file since BASE belongs to, nogo's included, and for
# the tests that check every runsc flag and command whatever package defines
# them (runsc/cli/maincli's, runsc/config's): tier 1 runs them.
# runsc/container's and test/'s are left to the save/restore tier.
# With "dependents", it prints the expression for those tests and the tests of
# every target that depends directly on such a package: the save/restore tier
# builds them, which for a package as central as the kernel means hundreds of
# packages and a C++ toolchain built from source. It prints nothing when no
# such package changed.
#
# Usage: tools/fork/affected_tests.sh BASE [dependents]

set -euo pipefail

readonly BASE="${1:?usage: affected_tests.sh BASE [dependents]}"
readonly MODE="${2:-}"

readonly -a ALWAYS=(//runsc/cli/maincli:all //runsc/config:all)

declare -A packages=()
while IFS= read -r -d '' file; do
  # Files no Bazel test reads: the fork's own files, workflows and docs.
  case "${file}" in
    SERIES | .github/* | tools/fork/* | g3doc/* | website/* | *.md) continue ;;
  esac
  dir="$(dirname "${file}")"
  while [[ "${dir}" != "." && ! -e "${dir}/BUILD" && ! -e "${dir}/BUILD.bazel" ]]; do
    dir="$(dirname "${dir}")"
  done
  case "${dir}" in
    runsc/container | runsc/container/* | test | test/*) ;;
    *) packages["//${dir#.}:all"]=1 ;;
  esac
done < <(git diff -z --name-only "${BASE}...HEAD")

if [[ ${#packages[@]} -eq 0 ]]; then
  exit 0
fi

case "${MODE}" in
  "") echo "tests(set(${!packages[*]} ${ALWAYS[*]}))" ;;
  dependents)
    # The root package's dependents are every nogo test (//:nogo_config):
    # a change there, to MODULE.bazel say, is the whole tree's, not tier 2's.
    unset 'packages[//:all]'
    [[ ${#packages[@]} -eq 0 ]] ||
      echo "tests(rdeps(//pkg/... + //runsc/... + //shim/... + //tools/..., set(${!packages[*]}), 1))" \
        "except //runsc/container/..."
    ;;
  *) echo "affected_tests.sh: unknown mode ${MODE}" >&2 && exit 1 ;;
esac
