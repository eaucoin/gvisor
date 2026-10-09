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
# baseline build if given, on this host: 512 MiB workloads checkpointed and
# restored from local disk and from a source throttled to 100 MiB/s, plainly,
# in the background and with direct I/O, in C and as a Python REPL. The
# repetitions of each measurement alternate between the two builds, since
# hosts differ more than builds do. It writes OUT/results.jsonl,
# OUT/baseline.jsonl and OUT/summary.md. It needs root (through sudo) and
# Docker; bench.py prepare must have run.
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

measure() {
  local rep
  for rep in 1 2 3; do
    sudo python3 "${BENCH}" run --runsc "${RUNSC}" "${flags[@]}" --mib 512 --reps 1 \
      --out "${OUT}/results.jsonl" "$@"
    if [[ -n "${BASELINE}" ]]; then
      sudo python3 "${BENCH}" run --runsc "${BASELINE}" --mib 512 --reps 1 \
        --out "${OUT}/baseline.jsonl" "$@"
    fi
  done
}

measure --workload c --restore plain --restore background --restore plain@100 --restore background@100
measure --workload c --checkpoint-flag=--direct --restore plain+direct --restore background+direct
measure --workload py --restore plain --restore background --restore background@100

{
  echo "$("${RUNSC}" --version | head -n 1)${flags[*]:+, with ${flags[*]#--runsc-flag=}}:"
  echo
  echo '```text'
  python3 "${BENCH}" summary "${OUT}/results.jsonl"
  echo '```'
  if [[ -n "${BASELINE}" ]]; then
    echo
    echo "$("${BASELINE}" --version | head -n 1), measured alongside:"
    echo
    echo '```text'
    python3 "${BENCH}" summary "${OUT}/baseline.jsonl"
    echo '```'
  fi
} >"${OUT}/summary.md"
