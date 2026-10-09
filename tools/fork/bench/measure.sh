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

# measure.sh runs tier 3's measurements (bench.py) on a runsc build, and on a
# baseline build if given, on this host. It is the list of what tier 3
# measures, for the weekly bench and for every release:
#
# - On both builds, 512 MiB workloads checkpointed and restored from local disk
#   and from a source throttled to 100 and 400 MiB/s, plainly, in the
#   background and with direct I/O, in C and as a Python REPL. The repetitions
#   alternate between the two builds, since hosts differ more than builds do.
# - On the measured build only, since upstream lacks what they measure:
#   incremental checkpoints (--dirty-tracking=wp, a delta of 8 s of dirtying,
#   restored as a chain; and the cost of tracked first writes, against the same
#   run untracked); checkpoints to a store throttled to 100 MiB/s for writes,
#   stop-the-world against pre-copy, at a dirtying rate below and above half
#   the store's speed; checkpoints to and background restores from an S3 store
#   (SeaweedFS in Docker); and restores of an image with a working set, with
#   and without prefetch, compacted or not.
#
# Every measurement checks that the restored workload answers and that its
# memory holds its last writes. It takes about 28 minutes on a free GitHub
# runner. It writes OUT/results.jsonl, OUT/baseline.jsonl and OUT/summary.md
# (Markdown, for release notes). It needs root (through sudo) and Docker;
# bench.py prepare must have run.
#
# Usage: tools/fork/bench/measure.sh OUT RUNSC [BASELINE_RUNSC] [-- RUNSC_FLAG...]
#
# Each RUNSC_FLAG is a global runsc flag for RUNSC only.

set -euo pipefail

declare bench
bench="$(dirname "$(realpath "$0")")/bench.py"
readonly BENCH="${bench}"
readonly OUT="${1:?usage: measure.sh OUT RUNSC [BASELINE_RUNSC] [-- RUNSC_FLAG...]}"
readonly RUNSC="${2:?usage: measure.sh OUT RUNSC [BASELINE_RUNSC] [-- RUNSC_FLAG...]}"
shift 2
declare baseline=""
if (( $# > 0 )) && [[ "$1" != -- ]]; then
  baseline="$1"
  shift
fi
readonly BASELINE="${baseline}"
if (( $# > 0 )); then
  shift  # --
fi
declare -a flags=()
for flag in "$@"; do
  flags+=("--runsc-flag=${flag}")
done
readonly flags

mkdir -p "${OUT}"

# both COMMAND ARGS...: three repetitions on each build, alternating.
both() {
  local rep
  for rep in 1 2 3; do
    sudo python3 "${BENCH}" "$1" --runsc "${RUNSC}" "${flags[@]}" --reps 1 \
      --out "${OUT}/results.jsonl" "${@:2}"
    if [[ -n "${BASELINE}" ]]; then
      sudo python3 "${BENCH}" "$1" --runsc "${BASELINE}" --reps 1 \
        --out "${OUT}/baseline.jsonl" "${@:2}"
    fi
  done
}

# only COMMAND ARGS...: three repetitions on the measured build.
only() {
  sudo python3 "${BENCH}" "$1" --runsc "${RUNSC}" "${flags[@]}" --reps 3 \
    --out "${OUT}/results.jsonl" "${@:2}"
}

both run --workload c --restore plain --restore background \
  --restore plain@100 --restore background@100 --restore plain@400 --restore background@400
both run --workload c --checkpoint-flag=--direct --restore plain+direct --restore background+direct
both run --workload py --restore plain --restore background --restore background@100 --restore background@400

only incremental --workload c --tracking wp --restore plain --restore background
only incremental --workload c --restore plain

# 5 %/s of 512 MiB is a quarter of the store's 100 MiB/s, 12 %/s more than half.
for pct in 5 12; do
  only run --workload c --tracking wp --dirty-pct "${pct}" --write-mib-per-s 100 \
    --checkpoint-flag=--direct --restore plain
  only run --workload c --tracking wp --dirty-pct "${pct}" --write-mib-per-s 100 \
    --checkpoint-flag=--direct --checkpoint-flag=--precopy=on --restore plain
done

sudo python3 "${BENCH}" seaweedfs start
trap 'sudo python3 "${BENCH}" seaweedfs stop' EXIT
only run --workload c --s3 --restore background
only run --workload py --s3 --restore background
sudo python3 "${BENCH}" seaweedfs stop
trap - EXIT

only working-set --workload py --dirty-pct 1 --window 3s --restore background@100

declare against="no baseline"
if [[ -n "${BASELINE}" ]]; then
  against="$("${BASELINE}" --version | head -n 1)"
fi
readonly against
{
  echo "$("${RUNSC}" --version | head -n 1)${flags[*]:+, with ${flags[*]#--runsc-flag=}}," \
    "against ${against}:"
  echo
  if [[ -n "${BASELINE}" ]]; then
    python3 "${BENCH}" summary "${OUT}/results.jsonl" --baseline "${OUT}/baseline.jsonl"
  else
    python3 "${BENCH}" summary "${OUT}/results.jsonl"
  fi
} >"${OUT}/summary.md"
