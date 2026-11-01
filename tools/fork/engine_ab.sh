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

# Spike, not for main: the kernel-standby validation's engine A/B (the endpoint
# package's G-16). Each build named on the command line checkpoints and
# restores a busybox container holding WORKLOAD's processes or open files, RUNS
# times per workload, the builds and workloads interleaved in a random order
# on every round, since runners differ more than builds do. Each cycle is one
# JSON line in OUT: the checkpoint's and the restore's wall times, what the
# boot logs say of the kernel's save and load, its pause, the VFS restore, and
# the state file's size; and whether the restored container counts on from
# its checkpoint.
#
# Usage: tools/fork/engine_ab.sh RUNS OUT WORKLOADS BUILD...
#   WORKLOADS: comma-separated, each "procs:N" (N sleeping processes) or
#              "files:N" (N files of the root filesystem held open).
#   BUILD:     NAME=RUNSC[,FLAG...]: a runsc, with its gvisor-bin/ beside it,
#              and the global flags it runs with (e.g. --state-engine=wire).

set -euo pipefail

readonly RUNS="${1:?usage: engine_ab.sh RUNS OUT WORKLOADS BUILD...}"
readonly OUT="${2:?usage: engine_ab.sh RUNS OUT WORKLOADS BUILD...}"
readonly WORKLOADS="${3:?usage: engine_ab.sh RUNS OUT WORKLOADS BUILD...}"
shift 3
(( $# > 0 )) || { echo "usage: engine_ab.sh RUNS OUT WORKLOADS BUILD..." >&2; exit 1; }
declare -ra BUILDS=("$@")

declare tmp
tmp="$(mktemp -d)"
readonly tmp
readonly base="${tmp}/rootfs"

cleanup() {
  findmnt -rn -o TARGET | { grep "^${tmp}/" || true; } | sort -r | xargs -r sudo umount
  sudo rm -rf "${tmp}"
}
trap cleanup EXIT

mkdir -p "${base}"
# Docker Hub's busybox, from Google's mirror of Docker Hub.
container="$(docker create mirror.gcr.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e)"
docker export "${container}" | tar -C "${base}" -x
docker rm "${container}" >/dev/null
mkdir -p "${base}/data"
for (( i = 1; i <= 1000; i++ )); do echo "${i}" >"${base}/data/f${i}"; done

now_ms() {
  echo $(( $(date +%s%N) / 1000000 ))
}

# The workload's shell: its sleepers or its open files, then a counter that
# lives in its memory, each value also written out.
script_of() {
  local -r kind="${1%%:*}" n="${1##*:}"
  local setup
  case "${kind}" in
    procs) setup="for i in \$(seq ${n}); do sleep 1000000 & done" ;;
    files) setup="i=1; while [ \$i -le ${n} ]; do eval \"exec \$((i + 9))</data/f\$i\"; i=\$((i + 1)); done" ;;
    *) echo "no such workload: $1" >&2; exit 1 ;;
  esac
  echo "${setup}; i=0; while true; do i=\$((i + 1)); echo \"\${i}\" >/tmp/count; sleep 0.05; done"
}

# cycle BUILD WORKLOAD ROUND: one checkpoint and restore, one JSON line.
cycle() {
  local -r build="$1" workload="$2" round="$3"
  local -r name="${build%%=*}" spec="${build#*=}"
  local -a flags
  IFS=, read -ra flags <<<"${spec}"
  local -r runsc_bin="${flags[0]}"
  flags=("${flags[@]:1}")
  local -r dir="${tmp}/${name}-${workload//:/-}-${round}"
  local -r root="${dir}/root" bundle="${dir}/bundle" image="${dir}/image" logs="${dir}/logs"
  mkdir -p "${bundle}" "${image}" "${logs}"
  runsc() {
    sudo "${runsc_bin}" --root="${root}" --network=none --debug-log="${logs}/" --debug-command=boot,restore,checkpoint "${flags[@]}" "$@"
  }
  sudo cp -a "${base}" "${bundle}/rootfs"
  (cd "${bundle}" && "${runsc_bin}" spec -- sh -c "$(script_of "${workload}")")
  sed -i 's/"terminal": true/"terminal": false/' "${bundle}/config.json"
  runsc run --bundle="${bundle}" --detach w
  # Set up: every sleeper or file there, and counting.
  until runsc exec w cat /tmp/count >/dev/null 2>&1; do sleep 0.1; done
  sleep 1
  local procs fds
  procs="$(runsc exec w sh -c 'ls -d /proc/[0-9]* | wc -l')"
  fds="$(runsc exec w sh -c 'ls /proc/1/fd | wc -l')"
  local before after later t0 t1 t2
  before="$(runsc exec w cat /tmp/count)"
  t0="$(now_ms)"
  runsc checkpoint --image-path="${image}" w
  t1="$(now_ms)"
  # The checkpointed sandbox may take a moment to be seen stopped.
  runsc delete --force w
  local -r size="$(sudo stat -c %s "${image}/checkpoint.img")"
  runsc restore --bundle="${bundle}" --image-path="${image}" --detach w
  t2="$(now_ms)"
  after="$(runsc exec w cat /tmp/count)"
  sleep 0.3
  later="$(runsc exec w cat /tmp/count)"
  runsc delete --force w
  local ok=true
  if (( after < before || later <= after )); then ok=false; fi
  # The two boot logs: the checkpointed sandbox's first, then the restored one's.
  local boot
  boot="$(sudo sh -c "cat ${logs}/*.boot.txt" | grep -E 'Kernel save took|paused for|Kernel load took|VFS restored|Kernel loaded|Network stack restored|Overall load took' | sed -e 's/["\\]/ /g' -e 's/\t/ /g' | paste -sd '|')"
  echo "{\"build\": \"${name}\", \"workload\": \"${workload}\", \"round\": ${round}, \"procs\": ${procs}, \"fds\": ${fds}, \"checkpoint_ms\": $(( t1 - t0 )), \"restore_ms\": $(( t2 - t1 )), \"image_bytes\": ${size}, \"counted_on\": ${ok}, \"boot\": \"${boot}\"}" >>"${OUT}"
  if [[ "${ok}" != true ]]; then
    echo "${name} ${workload} round ${round}: restored at ${after}, then ${later}, checkpointed at ${before}" >&2
  fi
  findmnt -rn -o TARGET | { grep "^${dir}/" || true; } | sort -r | xargs -r sudo umount
  sudo rm -rf "${dir}"
}

IFS=, read -ra workloads <<<"${WORKLOADS}"
: >"${OUT}"
for (( round = 1; round <= RUNS; round++ )); do
  cells=()
  for build in "${BUILDS[@]}"; do
    for workload in "${workloads[@]}"; do cells+=("${build}|${workload}"); done
  done
  # The cells read from a descriptor of their own: runsc and sudo read the
  # standard input.
  while IFS= read -r -u 3 cell; do
    cycle "${cell%%|*}" "${cell##*|}" "${round}" </dev/null
  done 3< <(printf '%s\n' "${cells[@]}" | shuf)
  echo "round ${round} of ${RUNS} done"
done
