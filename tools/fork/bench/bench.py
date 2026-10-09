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

"""The checkpoint/restore bench: run a workload in a runsc sandbox, checkpoint it,
restore it, and measure what the workload and the host see.

Run as root, with python3's standard library only, on a host with systemd and Docker.

    bench.py prepare [--image python:3.12-slim]
    bench.py run --runsc PATH --mib 512 [options]      (see `bench.py run --help`)
    bench.py summary RESULTS.jsonl...
    bench.py clean [--all]

Everything lives under $BENCH_WORK (default /tmp/checkpoint-bench):
    rootfs/        the shared root filesystem (python:3.12-slim + /bench/dirtier{,.py})
    state/         runsc --root
    runs/<cid>/    bundle, stdin FIFOs, stdout logs, runsc stderr, checkpoint image

Each sandbox runs in a transient systemd scope (MemoryMax=--memory-max), so its
memory.peak is measured and it cannot push the live stack out of memory. A restore
from a throttled source runs in a scope with IOReadBandwidthMax on the image's disk,
after the image has been evicted from the page cache (cold restore).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import time
import uuid
from pathlib import Path

BENCH = Path(__file__).resolve().parent
WORK = Path(os.environ.get("BENCH_WORK", "/tmp/checkpoint-bench"))
ROOTFS = WORK / "rootfs"
STATE = WORK / "state"
RUNS = WORK / "runs"


def log(*a: object) -> None:
    print(*a, file=sys.stderr, flush=True)


def sh(*cmd: str, **kw) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=True, text=True, capture_output=True, **kw)


# ---------------------------------------------------------------------------- prepare


def prepare(args: argparse.Namespace) -> None:
    """Export a container image as the shared rootfs and add the workloads to it."""
    if not ROOTFS.exists():
        had = subprocess.run(["docker", "image", "inspect", args.image], capture_output=True).returncode == 0
        if not had:
            sh("docker", "pull", "-q", args.image)
        cid = sh("docker", "create", args.image).stdout.strip()
        ROOTFS.mkdir(parents=True)
        try:
            export = subprocess.Popen(["docker", "export", cid], stdout=subprocess.PIPE)
            subprocess.run(["tar", "-x", "-C", str(ROOTFS)], stdin=export.stdout, check=True)
            export.wait()
        finally:
            sh("docker", "rm", cid)
            if not had:  # best effort: someone else may have started using it meanwhile
                subprocess.run(["docker", "rmi", args.image], capture_output=True)
    (ROOTFS / "bench").mkdir(exist_ok=True)
    sh("gcc", "-O2", "-static", "-pthread", "-o", str(ROOTFS / "bench/dirtier"), str(BENCH / "dirtier.c"))
    shutil.copy(BENCH / "dirtier.py", ROOTFS / "bench/dirtier.py")
    log(f"rootfs ready: {ROOTFS} ({sh('du', '-sh', str(ROOTFS)).stdout.split()[0]})")


# ---------------------------------------------------------------------------- sandbox


class Tail:
    """Follows a workload's stdout log, line by line, timestamping each line on arrival."""

    def __init__(self, path: Path):
        self.path, self.pos, self.partial, self.lines = path, 0, b"", []  # (host monotonic s, text)

    def poll(self) -> None:
        with open(self.path, "rb") as f:
            f.seek(self.pos)
            data = f.read()
        self.pos += len(data)
        now = time.monotonic()
        *done, self.partial = (self.partial + data).split(b"\n")
        self.lines += [(now, line.decode(errors="replace")) for line in done]

    def wait(self, prefix: str, timeout: float, start: int = 0) -> tuple[float, str]:
        deadline = time.monotonic() + timeout
        while True:
            self.poll()
            for t, line in self.lines[start:]:
                if line.startswith(prefix):
                    return t, line
            if time.monotonic() > deadline:
                raise TimeoutError(f"no {prefix!r} in {self.path} within {timeout} s")
            time.sleep(0.002)


class Sandbox:
    """One container (and sandbox) under a runsc binary, in its own systemd scope."""

    def __init__(self, a: argparse.Namespace, cid: str, run_dir: Path):
        self.a, self.cid, self.dir = a, cid, run_dir
        self.bundle = run_dir / "bundle"
        self.n = 0  # incarnation: 0 = run, 1.. = restores
        self.fifo_fd: int | None = None
        self.unit: str | None = None

    def runsc(self, *args: str) -> list[str]:
        flags = [f"--root={STATE}", "--ignore-cgroups", "--network=none", "--overlay2=root:memory"]
        if self.a.debug:
            flags += ["--debug", f"--debug-log={self.dir}/debug/"]
        return [self.a.runsc, *flags, *self.a.runsc_flag, *args]

    def make_bundle(self) -> None:
        self.bundle.mkdir(parents=True)
        if self.a.workload == "c":
            argv = ["/bench/dirtier"]
        else:
            argv = ["/usr/local/bin/python3", "-u", "/bench/dirtier.py"]
        argv += [str(self.a.mib), str(self.a.dirty_pct), str(self.a.burst_hz), str(self.a.gap_ms)]
        sh(self.a.runsc, "spec", f"--bundle={self.bundle}", "--", *argv)
        spec = json.loads((self.bundle / "config.json").read_text())
        spec["root"] = {"path": str(ROOTFS), "readonly": False}
        spec["process"]["terminal"] = False
        (self.bundle / "config.json").write_text(json.dumps(spec, indent=1))

    def _start(self, verb: list[str], props: list[str]) -> tuple[subprocess.Popen, Tail]:
        """Starts `runsc <verb...>` with a fresh stdin FIFO and stdout log, in a new scope."""
        if self.fifo_fd is not None:
            os.close(self.fifo_fd)
        fifo = self.dir / f"in-{self.n}.fifo"
        os.mkfifo(fifo)
        self.fifo_fd = os.open(fifo, os.O_RDWR)  # held open: the workload never sees EOF
        out = self.dir / f"out-{self.n}.log"
        self.unit = f"checkpoint-bench-{self.cid}-{self.n}"
        cmd = ["systemd-run", "--quiet", "--scope", f"--unit={self.unit}",
               f"-pMemoryMax={self.a.memory_max}", "-pMemorySwapMax=0", *[f"-p{p}" for p in props],
               "--", *self.runsc(*verb)]
        # O_APPEND: a restored workload keeps writing at its old stdout offset otherwise.
        with open(out, "ab") as o, open(self.dir / f"runsc-{self.n}.err", "ab") as e:
            proc = subprocess.Popen(cmd, stdin=self.fifo_fd, stdout=o, stderr=e)
        return proc, Tail(out)

    def send(self, line: str) -> None:
        os.write(self.fifo_fd, (line + "\n").encode())

    def memory_peak_mib(self) -> float | None:
        cg = sh("systemctl", "show", "-P", "ControlGroup", f"{self.unit}.scope").stdout.strip()
        try:
            return int(Path(f"/sys/fs/cgroup{cg}/memory.peak").read_text()) / 2**20
        except (OSError, ValueError):
            return None

    def run(self) -> tuple[Tail, float]:
        self.make_bundle()
        t0 = time.monotonic()
        proc, tail = self._start(["run", "--detach", f"--bundle={self.bundle}", self.cid], [])
        if proc.wait() != 0:
            raise RuntimeError(f"runsc run failed: {(self.dir / 'runsc-0.err').read_text()[-2000:]}")
        t, _ = tail.wait("READY", self.a.timeout)
        return tail, t - t0

    def restore(self, image: Path, flags: list[str], props: list[str]) -> tuple[subprocess.Popen, Tail, float]:
        self.n += 1
        t0 = time.monotonic()
        proc, tail = self._start(
            ["restore", "--detach", f"--bundle={self.bundle}", f"--image-path={image}", *flags, self.cid], props)
        return proc, tail, t0

    def destroy(self) -> None:
        subprocess.run(self.runsc("kill", self.cid, "KILL"), capture_output=True)
        subprocess.run(self.runsc("delete", "--force", self.cid), capture_output=True)
        if self.unit:  # the scope ends with its last process; make sure it has
            for _ in range(500):
                r = subprocess.run(["systemctl", "is-active", "-q", f"{self.unit}.scope"])
                if r.returncode != 0:
                    break
                time.sleep(0.01)
            subprocess.run(["systemctl", "stop", f"{self.unit}.scope"], capture_output=True)


# ---------------------------------------------------------------------------- run


def evict(image: Path) -> None:
    """Drops the image's pages from the page cache, so that a restore reads the disk."""
    for f in image.iterdir():
        fd = os.open(f, os.O_RDONLY)
        try:
            os.fsync(fd)
            os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
        finally:
            os.close(fd)


def disk_of(path: Path) -> str:
    """The whole-disk device holding `path` (io.max applies to whole disks)."""
    src = sh("findmnt", "-nvo", "SOURCE", "--target", str(path)).stdout.strip()
    parent = sh("lsblk", "-no", "PKNAME", src).stdout.strip()
    return f"/dev/{parent}" if parent else src


def gaps(lines: list[tuple[float, str]]) -> list[float]:
    return [float(l.split()[1]) for _, l in lines if l.startswith("GAP ")]


def stats(lines: list[tuple[float, str]]) -> dict:
    s = [list(map(float, l.split()[1:])) for _, l in lines if l.startswith("STAT ")]
    return {"seconds": len(s), "burst_p50_us_median": statistics.median(x[2] for x in s) if s else None,
            "burst_max_us": max((x[3] for x in s), default=None)}


def parse_restore(spec: str) -> tuple[str, list[str], int | None]:
    """'background+direct@100' -> (name, ['--background', '--direct'], 100 MiB/s)."""
    m = re.fullmatch(r"([a-z+]+)(?:@(\d+))?", spec)
    if not m:
        raise ValueError(spec)
    words = m.group(1).split("+")
    flags = [f"--{w}" for w in words if w != "plain"]
    for f in flags:
        if f not in ("--background", "--direct"):
            raise ValueError(f"unknown restore option {f} in {spec}")
    return spec, flags, int(m.group(2)) if m.group(2) else None


def one_rep(a: argparse.Namespace, rep: int) -> dict:
    cid = f"{a.workload}{a.mib}-{uuid.uuid4().hex[:6]}"
    run_dir = RUNS / cid
    run_dir.mkdir(parents=True)
    image = run_dir / "image"
    sb = Sandbox(a, cid, run_dir)
    r: dict = {"rep": rep, "cid": cid, "runsc": a.runsc, "workload": a.workload, "mib": a.mib,
               "dirty_pct_per_s": a.dirty_pct, "checkpoint_flags": a.checkpoint_flag,
               "loadavg_start": os.getloadavg()[0]}
    try:
        tail, r["start_to_ready_s"] = sb.run()
        time.sleep(a.warmup)
        n = len(tail.lines)
        t0 = time.monotonic(); sb.send("ping 0"); t1, _ = tail.wait("PONG 0", 30, n)
        r["answer_before_s"] = t1 - t0

        # Checkpoint (by default --leave-running, so the pause is seen from inside).
        n = len(tail.lines)
        flags = (["--leave-running"] if a.leave_running else []) + a.checkpoint_flag
        t0 = time.monotonic()
        ck = subprocess.run(sb.runsc("checkpoint", f"--image-path={image}", *flags, cid), capture_output=True, text=True)
        r["checkpoint_s"] = time.monotonic() - t0
        if ck.returncode != 0:
            raise RuntimeError(f"checkpoint failed: {ck.stderr[-2000:]}")
        r["image_bytes"] = {f.name: f.stat().st_size for f in image.iterdir()}
        r["image_total_mib"] = sum(r["image_bytes"].values()) / 2**20
        if a.leave_running:
            time.sleep(a.observe)
            tail.poll()
            g = gaps(tail.lines[n:])
            r["checkpoint_pause_ms"] = max(g, default=0.0)
            r["checkpoint_gaps_ms"] = g
        r["memory_peak_run_mib"] = sb.memory_peak_mib()
        sb.destroy()

        r["restores"] = []
        for spec in a.restore:
            name, rflags, mibps = parse_restore(spec)
            evict(image)
            props = [f"IOReadBandwidthMax={a.throttle_dev or disk_of(image)} {mibps}M"] if mibps else []
            proc, tail, t0 = sb.restore(image, rflags, props)
            sb.send("ping 1")  # waits in the FIFO until the restored workload reads it
            while (rc := proc.poll()) is None:  # the answer may come before the command returns
                tail.poll()
                time.sleep(0.002)
            t_ret = time.monotonic()
            if rc != 0:
                raise RuntimeError(f"restore {spec} failed: {(run_dir / f'runsc-{sb.n}.err').read_text()[-2000:]}")
            t_ans, _ = tail.wait("PONG 1", a.timeout)
            n = len(tail.lines)
            t2 = time.monotonic(); sb.send("touch"); t3, line = tail.wait("TOUCHED", a.timeout, n)
            time.sleep(a.observe)
            tail.poll()
            g = gaps(tail.lines)
            rr = {"restore": name, "flags": rflags, "read_mib_per_s": mibps,
                  "command_s": t_ret - t0, "first_answer_s": t_ans - t0,
                  "touch_all_s": t3 - t2, "touch_all_inside_ms": float(line.split()[1]),
                  # The first gap spans checkpoint-to-restore; the rest are stalls after it.
                  "downtime_gap_ms": g[0] if g else None, "later_gaps_ms": g[1:],
                  "stats": stats(tail.lines), "memory_peak_mib": sb.memory_peak_mib()}
            r["restores"].append(rr)
            log(f"  rep {rep} restore {name}: command {rr['command_s']:.3f} s, "
                f"first answer {rr['first_answer_s']:.3f} s, touch {rr['touch_all_s']:.3f} s")
            sb.destroy()
        log(f"rep {rep}: ready {r['start_to_ready_s']:.2f} s, checkpoint {r['checkpoint_s']:.3f} s, "
            f"pause {r.get('checkpoint_pause_ms')} ms, image {r['image_total_mib']:.0f} MiB")
    finally:
        sb.destroy()
        if sb.fifo_fd is not None:
            os.close(sb.fifo_fd)
        if not a.keep:
            shutil.rmtree(run_dir, ignore_errors=True)
    return r


def run(a: argparse.Namespace) -> None:
    if not (ROOTFS / "bench/dirtier").exists():
        sys.exit("run `bench.py prepare` first")
    version = sh(a.runsc, "--version").stdout.splitlines()[0]
    out = open(a.out, "a") if a.out else None
    results = []
    for rep in range(a.reps):
        r = one_rep(a, rep)
        r["runsc_version"] = version
        results.append(r)
        if out:
            out.write(json.dumps(r) + "\n")
            out.flush()
    summarize(results)


# ---------------------------------------------------------------------------- summary


def med(xs: list) -> str:
    xs = [x for x in xs if x is not None]
    if not xs:
        return "-"
    return f"{statistics.median(xs):.3f} [{', '.join(f'{x:.3f}' for x in xs)}]"


def summarize(results: list[dict]) -> None:
    groups: dict = {}
    for r in results:
        groups.setdefault((r["runsc"], r["workload"], r["mib"], r["dirty_pct_per_s"], " ".join(r["checkpoint_flags"])), []).append(r)
    for (runsc, wl, mib, pct, cf), rs in groups.items():
        print(f"\n## {wl} {mib} MiB, dirty {pct}%/s, checkpoint {cf or '(defaults)'}, {runsc}, n={len(rs)}")
        print(f"start to ready (s)      {med([r['start_to_ready_s'] for r in rs])}")
        print(f"checkpoint (s)          {med([r['checkpoint_s'] for r in rs])}")
        print(f"pause inside (s)        {med([r.get('checkpoint_pause_ms', 0) / 1e3 for r in rs])}")
        print(f"image (MiB)             {med([r['image_total_mib'] for r in rs])}")
        print(f"memory peak (MiB)       {med([r['memory_peak_run_mib'] for r in rs])}")
        names = [x["restore"] for x in rs[0].get("restores", [])]
        for name in names:
            rr = [x for r in rs for x in r["restores"] if x["restore"] == name]
            print(f"restore {name}:")
            print(f"  command returns (s)   {med([x['command_s'] for x in rr])}")
            print(f"  first answer (s)      {med([x['first_answer_s'] for x in rr])}")
            print(f"  touch all pages (s)   {med([x['touch_all_s'] for x in rr])}")
            print(f"  later stalls max (s)  {med([max(x['later_gaps_ms'], default=0) / 1e3 for x in rr])}")


def summary(a: argparse.Namespace) -> None:
    rs = [json.loads(l) for p in a.files for l in open(p) if l.strip()]
    summarize(rs)


# ---------------------------------------------------------------------------- clean


def clean(a: argparse.Namespace) -> None:
    """Kills every sandbox under our --root, stops our scopes, removes our files."""
    for runsc in {a.runsc, "/usr/local/bin/runsc"}:
        if STATE.exists() and Path(runsc).exists():
            ids = subprocess.run([runsc, f"--root={STATE}", "list", "-q"], capture_output=True, text=True).stdout.split()
            for cid in ids:
                subprocess.run([runsc, f"--root={STATE}", "kill", cid, "KILL"], capture_output=True)
                subprocess.run([runsc, f"--root={STATE}", "delete", "--force", cid], capture_output=True)
    units = subprocess.run(["systemctl", "list-units", "--plain", "--no-legend", "checkpoint-bench-*"],
                           capture_output=True, text=True).stdout.split("\n")
    for u in units:
        if u.split():
            subprocess.run(["systemctl", "stop", u.split()[0]], capture_output=True)
    # runsc keeps the null network namespace it shares between sandboxes bind-mounted
    # in its --root (runsc/container: null-netns); unmount it before removing the root.
    if (STATE / "null-netns").exists():
        subprocess.run(["umount", str(STATE / "null-netns")], capture_output=True)
    for p in (STATE, RUNS):
        shutil.rmtree(p, ignore_errors=True)
    if a.all:
        shutil.rmtree(ROOTFS, ignore_errors=True)
    log("clean")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    pp = sub.add_parser("prepare")
    pp.add_argument("--image", default="python:3.12-slim")
    pr = sub.add_parser("run")
    pr.add_argument("--runsc", default="/usr/local/bin/runsc", help="runsc binary (its gvisor-bin/ next to it)")
    pr.add_argument("--runsc-flag", action="append", default=[], help="extra global runsc flag (repeatable)")
    pr.add_argument("--workload", choices=["c", "py"], default="c")
    pr.add_argument("--mib", type=int, default=64, help="working set (MiB)")
    pr.add_argument("--dirty-pct", type=float, default=5, help="share of the working set written per second (%%)")
    pr.add_argument("--burst-hz", type=int, default=10)
    pr.add_argument("--gap-ms", type=float, default=10, help="smallest stall the workload reports")
    pr.add_argument("--checkpoint-flag", action="append", default=[],
                    help="e.g. --checkpoint-flag=--direct --checkpoint-flag=--compression=flate-best-speed")
    pr.add_argument("--no-leave-running", dest="leave_running", action="store_false")
    pr.add_argument("--restore", action="append", default=[],
                    help="restore variant, repeatable: plain|background[+direct][@MiB/s]")
    pr.add_argument("--throttle-dev", help="device for IOReadBandwidthMax (default: the image's disk)")
    pr.add_argument("--memory-max", default="1536M", help="MemoryMax of each sandbox's scope")
    pr.add_argument("--warmup", type=float, default=3, help="seconds of dirtying before the checkpoint")
    pr.add_argument("--observe", type=float, default=2, help="seconds watched after checkpoint/restore")
    pr.add_argument("--reps", type=int, default=3)
    pr.add_argument("--timeout", type=float, default=120, help="seconds to wait for any answer")
    pr.add_argument("--out", help="append one JSON line per repetition here")
    pr.add_argument("--keep", action="store_true", help="keep runs/<cid>/ (logs, image)")
    pr.add_argument("--debug", action="store_true", help="runsc debug logs into runs/<cid>/debug/")
    ps = sub.add_parser("summary")
    ps.add_argument("files", nargs="+")
    pc = sub.add_parser("clean")
    pc.add_argument("--runsc", default="/usr/local/bin/runsc")
    pc.add_argument("--all", action="store_true", help="also the rootfs")
    a = p.parse_args()
    if a.cmd == "run" and not a.restore:
        a.restore = ["plain"]
    {"prepare": prepare, "run": run, "summary": summary, "clean": clean}[a.cmd](a)


if __name__ == "__main__":
    main()
