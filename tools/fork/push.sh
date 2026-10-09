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

# push.sh runs "git push ARGS..." to this repository from a workflow, with the
# repository's deploy key ($FORK_PUSH_KEY) rather than the workflow's token.
#
# The rulesets let only this key write master, go, main and release tags, and
# unlike the token it may push commits that change workflow files and its
# pushes start the workflows they trigger.

set -euo pipefail

: "${FORK_PUSH_KEY:?the deploy key is not set}"
: "${GITHUB_REPOSITORY:?not running in a workflow}"
: "${RUNNER_TEMP:?not running in a workflow}"

declare -r key="${RUNNER_TEMP}/fork-push-key"
declare -r known_hosts="${RUNNER_TEMP}/fork-push-known-hosts"
trap 'rm -f "${key}"' EXIT

install -m 0600 /dev/null "${key}"
printf '%s\n' "${FORK_PUSH_KEY}" >"${key}"
# GitHub's published host key (docs.github.com, "GitHub's SSH key fingerprints").
echo 'github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' >"${known_hosts}"

GIT_SSH_COMMAND="ssh -i ${key} -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=${known_hosts}" \
  git push "git@github.com:${GITHUB_REPOSITORY}.git" "$@"
