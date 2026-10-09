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

Each measurement is the median of its repetitions: the checkpoint's duration
and the pause the workload saw, and for each restore variant the time until
the command returned, until the workload's first answer, and to touch every
page. It prints a Markdown table and exits with 1 when a measurement of the
new run is worse than the baseline's by more than the threshold (a fraction)
and by more than the floor (seconds), which keeps a few milliseconds of noise
on short measurements from counting as regressions.
"""

from __future__ import annotations

import argparse
import json
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
            config = (f"{r['workload']} {r['mib']} MiB, dirty {r['dirty_pct_per_s']}%/s, "
                      f"checkpoint {' '.join(r['checkpoint_flags']) or '(defaults)'}")
            found = {"checkpoint (s)": r["checkpoint_s"]}
            if "checkpoint_pause_ms" in r:
                found["pause inside (s)"] = r["checkpoint_pause_ms"] / 1e3
            for rr in r.get("restores", []):
                name = rr["restore"]
                found[f"restore {name}: command (s)"] = rr["command_s"]
                found[f"restore {name}: first answer (s)"] = rr["first_answer_s"]
                found[f"restore {name}: touch all (s)"] = rr["touch_all_s"]
            for key, value in found.items():
                values.setdefault((config, key), []).append(value)
    return {k: statistics.median(v) for k, v in values.items()}


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
        worse = n - b > a.floor and change > a.threshold
        regressions += worse
        mark = " **regression**" if worse else ""
        print(f"| {config} | {name} | {b:.3f} | {n:.3f} | {change:+.0%}{mark} |")
    sys.exit(1 if regressions else 0)


if __name__ == "__main__":
    main()
