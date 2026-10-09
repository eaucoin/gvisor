#!/usr/bin/env python3

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

"""Puts the results of Go benchmarks run on several builds side by side.

    gobench.py NAME=OUTPUT.log [NAME=OUTPUT.log...]

Each OUTPUT.log is the output of `go test -test.bench` (any number of runs,
as with -test.count); this prints, as a Markdown table, the median time per
operation of each benchmark on each build, in seconds, and exits with 1 if a
build ran none.
"""

from __future__ import annotations

import re
import statistics
import sys

RESULT = re.compile(r"^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op")


def main() -> None:
    builds = [arg.split("=", 1) for arg in sys.argv[1:]]
    if not builds or any(len(b) != 2 for b in builds):
        sys.exit(__doc__)
    found: dict[str, dict[str, list[float]]] = {}
    for name, path in builds:
        with open(path) as f:
            for line in f:
                if m := RESULT.match(line):
                    found.setdefault(m[1], {}).setdefault(name, []).append(float(m[2]) / 1e9)
    print("| benchmark (s/op, median of runs) | " + " | ".join(name for name, _ in builds) + " |")
    print("|---" * (len(builds) + 1) + "|")
    for bench, runs in found.items():
        cells = [f"{statistics.median(runs[name]):.3f} (n={len(runs[name])})" if name in runs else "–"
                 for name, _ in builds]
        print(f"| {bench} | " + " | ".join(cells) + " |")
    missing = [name for name, _ in builds if not any(name in runs for runs in found.values())]
    if missing:
        sys.exit(f"no benchmark results for {', '.join(missing)}")


if __name__ == "__main__":
    main()
