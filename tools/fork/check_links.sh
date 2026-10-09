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

# check_links.sh checks that the relative links of the Markdown files changed
# since BASE lead to files of the tree. Links to the web, to anchors and to the
# website's absolute paths (/docs/...) are not checked: the fork does not
# build the website.
#
# Usage: tools/fork/check_links.sh BASE

set -euo pipefail

readonly BASE="${1:?usage: check_links.sh BASE}"

failed=0
while IFS= read -r -d '' file; do
  [[ -f "${file}" ]] || continue
  dir="$(dirname "${file}")"
  # Inline links and reference definitions: ](target) and [ref]: target.
  while IFS= read -r target; do
    target="${target%%#*}"
    case "${target}" in
      "" | /* | *://* | mailto:*) continue ;;
    esac
    if [[ ! -e "${dir}/${target}" ]]; then
      echo "${file}: broken link to ${target}" >&2
      failed=1
    fi
  done < <(grep -oE '\]\([^) ]+\)|^\[[^]]+\]: *[^ ]+' "${file}" |
    sed -E 's/^\]\(//; s/\)$//; s/^\[[^]]+\]: *//')
done < <(git diff -z --name-only --diff-filter=AM "${BASE}...HEAD" -- '*.md')
exit "${failed}"
