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

"""The Python REPL variant of dirtier.c, with the same arguments and protocol.

    python3 -u dirtier.py MIB DIRTY_PCT_PER_S [BURST_HZ=10] [GAP_MS=10]

It stands in for a REPL session: its data is a bytearray, its threads share the
GIL, and besides ping/touch/quit it answers "eval EXPR" with "VALUE <repr>", as a
REPL turn would.
"""

import os
import sys
import threading
import time

PAGE = 4096


def out(line: str) -> None:
    """Writes a whole line in one write(2): print() writes the text and its newline
    separately, and lines from different threads would interleave."""
    os.write(1, (line + "\n").encode())


def now_ms() -> float:
    return time.monotonic() * 1e3


def ticker(gap_ms: float) -> None:
    prev = now_ms()
    while True:
        time.sleep(0.001)
        t = now_ms()
        if t - prev > gap_ms:
            out(f"GAP {t - prev:.1f}")
        prev = t


class Position:
    """The dirtier's position: the next page it writes, in pass gen. Its lock is
    held by the dirtier for each burst."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.next, self.gen = 0, 1


def dirtier(buf: bytearray, pos: Position, dirty_pct: float, burst_hz: int) -> None:
    npages = len(buf) // PAGE
    per_burst = int(npages * dirty_pct / 100 / burst_hz)
    if per_burst == 0:
        return
    period = 1000 / burst_hz
    lat, pages = [], 0
    start = now_ms()
    report = start + 1000
    while True:
        with pos.lock:
            t0 = now_ms()
            nxt, gen = pos.next, pos.gen
            for _ in range(per_burst):
                buf[nxt * PAGE + (gen % 64) * 8] = gen % 256
                nxt += 1
                if nxt == npages:
                    nxt, gen = 0, gen + 1
            pos.next, pos.gen = nxt, gen
            t1 = now_ms()
        lat.append((t1 - t0) * 1e3)
        pages += per_burst
        if t1 >= report:
            lat.sort()
            out(f"STAT {len(lat)} {pages} {lat[len(lat) // 2]:.0f} {lat[-1]:.0f}")
            lat, pages = [], 0
            while report <= t1:
                report += 1000
        start += period
        wait = start - now_ms()
        if wait > 0:
            time.sleep(wait / 1e3)
        else:
            start = now_ms()


def verify(buf: bytearray, pos: Position) -> tuple[int, int]:
    """Counts the pages that hold the dirtier's last write to them, and those that
    do not: pages before pos.next were last written in pass pos.gen, the others
    in the pass before (or never, in the first pass)."""
    checked = wrong = 0
    with pos.lock:
        for p in range(len(buf) // PAGE):
            g = pos.gen if p < pos.next else pos.gen - 1
            if g == 0:
                continue
            checked += 1
            wrong += buf[p * PAGE + (g % 64) * 8] != g % 256
    return checked, wrong


def main() -> None:
    mib, dirty_pct = int(sys.argv[1]), float(sys.argv[2])
    burst_hz = int(sys.argv[3]) if len(sys.argv) > 3 else 10
    gap_ms = float(sys.argv[4]) if len(sys.argv) > 4 else 10
    t0 = now_ms()
    # Random contents: no zero pages, and a 1 MiB period, far beyond flate's 32 KiB
    # window, so that compressed checkpoints are not flattered.
    buf = bytearray(os.urandom(1048576)) * mib
    out(f"READY {now_ms() - t0:.0f}")
    threading.Thread(target=ticker, args=(gap_ms,), daemon=True).start()
    pos = Position()
    threading.Thread(target=dirtier, args=(buf, pos, dirty_pct, burst_hz), daemon=True).start()
    env: dict = {"buf": buf}
    for line in sys.stdin:
        if line.startswith("ping"):
            out(f"PONG{line[4:].rstrip()}")
        elif line.startswith("touch"):
            s = now_ms()
            for off in range(PAGE - 1, len(buf), PAGE):
                buf[off] = (buf[off] + 1) & 0xFF
            out(f"TOUCHED {now_ms() - s:.1f}")
        elif line.startswith("verify"):
            out("VERIFIED {} {}".format(*verify(buf, pos)))
        elif line.startswith("eval "):
            out(f"VALUE {eval(line[5:], env)!r}")
        elif line.startswith("quit"):
            break


if __name__ == "__main__":
    main()
