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

"""Compares two runs of bench.py, measurement by measurement.

    compare.py BASELINE.jsonl NEW.jsonl [--threshold 0.2] [--floor 0.02]

Each measurement is the median of its repetitions' values (bench.py's "values":
times, sizes, rounds, the cost of writes; lower is better for all of them). It
prints a Markdown table and exits with 1 when a measurement of the new run is
worse than the baseline's by more than the threshold (a fraction) and by more
than its unit's floor (--floor seconds; 1 MiB; 1 round; 0.5 µs), which keeps
noise on short measurements from counting as regressions. Ratios are shown,
not judged.
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
import sys


def measurements(path: str) -> dict[tuple[str, str], float]:
    """Maps (configuration, measurement) to its median over the repetitions."""
    values: dict[tuple[str, str], list[float]] = {}
    with open(path) as f:
        for line in f:
            if not line.strip():
                continue
            r = json.loads(line)
            for name, value in r["values"].items():
                values.setdefault((r["config"], name), []).append(value)
    return {k: statistics.median(v) for k, v in values.items()}


def floor(name: str, seconds: float) -> float | None:
    """The smallest regression of the measurement that counts, or None if it is not judged."""
    unit = m[1] if (m := re.search(r"\((\w+)\)$", name)) else "rounds"
    return {"s": seconds, "MiB": 1.0, "rounds": 1.0, "µs": 0.5}.get(unit)


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("baseline")
    p.add_argument("new")
    p.add_argument("--threshold", type=float, default=0.2, help="relative regression that fails (fraction)")
    p.add_argument("--floor", type=float, default=0.02, help="absolute regression below which none fails (s)")
    a = p.parse_args()
    base, new = measurements(a.baseline), measurements(a.new)
    regressions = 0
    print("| configuration | measurement | baseline | new | change |")
    print("|---|---|---|---|---|")
    for key in sorted(new):
        config, name = key
        if key not in base:
            print(f"| {config} | {name} | - | {new[key]:.3f} | new |")
            continue
        b, n = base[key], new[key]
        change = (n - b) / b if b else 0.0
        least = floor(name, a.floor)
        worse = least is not None and n - b > least and change > a.threshold
        regressions += worse
        mark = " **regression**" if worse else ""
        print(f"| {config} | {name} | {b:.3f} | {n:.3f} | {change:+.0%}{mark} |")
    sys.exit(1 if regressions else 0)


if __name__ == "__main__":
    main()
