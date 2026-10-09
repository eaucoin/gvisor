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

# smoke_test.sh checks an installed runsc (with its gvisor-bin/ sidecars next
# to it): "runsc do", then a container that counts in its memory is
# checkpointed, restored, and must go on counting from where it stopped.
# It needs root (through sudo) and Docker, for a busybox root filesystem.
#
# Usage: tools/fork/smoke_test.sh RUNSC [RUNSC_FLAG...]

set -euo pipefail

readonly RUNSC="${1:?usage: smoke_test.sh RUNSC [RUNSC_FLAG...]}"
shift
declare -ra FLAGS=("$@")

declare tmp
tmp="$(mktemp -d)"
readonly tmp
readonly root="${tmp}/root" bundle="${tmp}/bundle" image="${tmp}/image" logs="${tmp}/logs"

runsc() {
  sudo "${RUNSC}" --root="${root}" --network=none --debug --debug-log="${logs}/" "${FLAGS[@]}" "$@"
}

cleanup() {
  local status=$?
  if (( status != 0 )); then
    # What the sandbox said, for whoever reads the failure.
    sudo find "${logs}" -type f -name '*.boot.txt' -exec tail -n 40 {} + >&2 || true
  fi
  runsc delete --force counter >/dev/null 2>&1 || true
  # runsc leaves a bind mount (its null network namespace) in its root.
  findmnt -rn -o TARGET | { grep "^${tmp}/" || true; } | sort -r | xargs -r sudo umount
  sudo rm -rf "${tmp}"
}
trap cleanup EXIT

"${RUNSC}" --version
runsc "do" echo "runsc do: ok"

mkdir -p "${bundle}/rootfs" "${image}"
# Docker Hub's busybox, from Google's mirror of Docker Hub: Docker Hub limits
# the pulls of the runners' shared addresses.
container="$(docker create mirror.gcr.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e)"
docker export "${container}" | tar -C "${bundle}/rootfs" -x
docker rm "${container}" >/dev/null
# The shell's counter lives in its memory; each value is also written out.
# shellcheck disable=SC2016 # The container's shell expands it.
(cd "${bundle}" && "${RUNSC}" spec -- sh -c \
  'i=0; while true; do i=$((i + 1)); echo "${i}" >/tmp/count; sleep 0.05; done')
# Detached, with no console to attach a terminal to.
sed -i 's/"terminal": true/"terminal": false/' "${bundle}/config.json"

count() {
  runsc exec counter cat /tmp/count
}

runsc run --bundle="${bundle}" --detach counter
sleep 2
before="$(count)"
runsc checkpoint --image-path="${image}" counter
runsc delete counter
sudo test -s "${image}/checkpoint.img"

runsc restore --bundle="${bundle}" --image-path="${image}" --detach counter
after="$(count)"
sleep 1
later="$(count)"
echo "before the checkpoint: ${before}; after the restore: ${after}, then ${later}"
# Restarted from scratch rather than restored, it would count from 1 again.
if (( after < before || later <= after )); then
  echo "the restored counter did not resume from its checkpointed state" >&2
  exit 1
fi
echo "checkpoint and restore: ok"
