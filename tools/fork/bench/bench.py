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

    bench.py prepare [--image IMAGE]
    bench.py run --runsc PATH [options]          a checkpoint, then restores of it
    bench.py incremental --runsc PATH [options]  a full checkpoint, then a delta, restored
    bench.py working-set --runsc PATH [options]  restores of an image with a working set
    bench.py seaweedfs start|stop                the S3 store `run --s3` checkpoints to
    bench.py summary RESULTS.jsonl [--baseline BASELINE.jsonl]
    bench.py clean [--all]

(see `bench.py COMMAND --help`). The workload, dirtier.c or dirtier.py, holds --mib
MiB, writes --dirty-pct % of it per second, and reports from inside the sandbox
every stall it sees ("GAP"). Each measurement also checks the workload: that it
answers after every restore, and that every page holds the last write the
workload made to it ("verify"); a failed check fails the measurement.

run: a checkpoint (--leave-running by default, so that the pause is seen from
inside), then each --restore variant: plain|background[+direct][@MiB/s], the
read throttled to MiB/s. --tracking=wp runs the sandbox with --dirty-tracking=wp;
--write-mib-per-s throttles the sandbox's writes, so its checkpoint's, as for a
remote store; with --s3, the image is kept in the store of `bench.py seaweedfs`.

incremental: with --tracking=wp, a full checkpoint, --dirty-seconds of dirtying,
then a delta checkpoint (--parent-image-path), restored as a chain (--layer-path).
The delta's pages are compared with what the workload wrote in between (its rate
times the time between the two checkpoints, from the Sentry's log), and the cost
of the workload's first writes after the first checkpoint with the same run
without tracking (--tracking=off, where the second checkpoint is full).

working-set: a checkpoint P; P restored in the background, answering, and
checkpointed again (C) with recording on (--working-set-window); C compacted
with `runsc image compact --working-set-first` (C'); then C and C' restored with
--working-set-prefetch=auto and =off, each --restore variant.

Everything lives under $BENCH_WORK (default /tmp/checkpoint-bench):
    rootfs/        the shared root filesystem (Python 3.12's image + /bench/dirtier{,.py})
    state/         runsc --root
    runs/<cid>/    bundle, stdin FIFOs, stdout logs, runsc logs, checkpoint images
    seaweedfs/     the S3 store's data

Each sandbox runs in a transient systemd scope (MemoryMax=--memory-max), so its
memory.peak is measured and it cannot push the live stack out of memory. A restore
from a throttled source runs in a scope with IOReadBandwidthMax on the image's disk,
after the image has been evicted from the page cache (cold restore). Times that
runsc logs (the Sentry's pause, the time to load every page in a background
restore, pre-copy rounds) come from its log, at its default level.

Each repetition appends one JSON line to --out: its section and configuration, its
"values" (medians of which `summary` and compare.py report; units in their names),
and the details they come from.
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
import urllib.request
import uuid
import xml.etree.ElementTree as ET
from pathlib import Path

BENCH = Path(__file__).resolve().parent
WORK = Path(os.environ.get("BENCH_WORK", "/tmp/checkpoint-bench"))
ROOTFS = WORK / "rootfs"
STATE = WORK / "state"
RUNS = WORK / "runs"
WEED = WORK / "seaweedfs"

# The images come from Google's mirror of Docker Hub, which does not limit the
# rate of pulls from shared addresses, as CI runners' are.
ROOTFS_IMAGE = ("mirror.gcr.io/library/python:3.12-slim"
                "@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1")
# The S3 store: SeaweedFS at the version alasio pins, one signed identity.
WEED_IMAGE = ("mirror.gcr.io/chrislusf/seaweedfs:4.47"
              "@sha256:ce9e796f1fe6f06968f4c04bdaf8f678dad9c8acdfef3d244133d71bfa6bf882")
WEED_CONTAINER = "checkpoint-bench-seaweedfs"
S3 = "http://127.0.0.1:18333"
S3_BUCKET = "checkpoint-bench"
S3_KEY, S3_SECRET = "bench", "bench-secret"

MIB = 2**20


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


# ---------------------------------------------------------------------------- S3 store


def seaweedfs(a: argparse.Namespace) -> None:
    """Starts or stops the S3 store: a SeaweedFS container on 127.0.0.1, its data in WEED."""
    subprocess.run(["docker", "rm", "-f", WEED_CONTAINER], capture_output=True)
    shutil.rmtree(WEED, ignore_errors=True)
    if a.action == "stop":
        return
    (WEED / "data").mkdir(parents=True)
    # The signed identity checkpoints use, and an anonymous one for the bench's own
    # requests (creating the bucket, listing and deleting objects).
    (WEED / "s3.json").write_text(json.dumps({"identities": [
        {"name": "bench", "credentials": [{"accessKey": S3_KEY, "secretKey": S3_SECRET}],
         "actions": ["Admin", "Read", "Write", "List", "Tagging"]},
        {"name": "anonymous", "actions": ["Admin"]}]}))
    port = S3.rsplit(":", 1)[1]
    sh("docker", "run", "-d", "--name", WEED_CONTAINER, "--memory=1g", "-e", "GOMEMLIMIT=512MiB",
       "-p", f"127.0.0.1:{port}:8333", "-v", f"{WEED}/data:/data", "-v", f"{WEED}/s3.json:/etc/seaweedfs/s3.json:ro",
       WEED_IMAGE, "server", "-dir=/data", "-ip.bind=0.0.0.0", "-volume.max=16", "-master.volumeSizeLimitMB=1024",
       "-master.defaultReplication=000", "-master.telemetry=false", "-filer", "-s3",
       "-s3.config=/etc/seaweedfs/s3.json")
    # The S3 gateway answers before the filer behind it is ready: retry the bucket.
    deadline = time.monotonic() + 120
    while True:
        try:
            s3_request("PUT", f"/{S3_BUCKET}")
            break
        except OSError as e:
            if time.monotonic() > deadline:
                logs = subprocess.run(["docker", "logs", "--tail=20", WEED_CONTAINER], capture_output=True, text=True)
                raise RuntimeError(f"SeaweedFS did not start: {e}\n{logs.stdout}{logs.stderr}") from e
            time.sleep(1)
    log(f"SeaweedFS {WEED_IMAGE.split('@')[0]} serving {S3}/{S3_BUCKET}")


def s3_request(method: str, path: str) -> bytes:
    with urllib.request.urlopen(urllib.request.Request(S3 + path, method=method), timeout=30) as r:
        return r.read()


def s3_objects(prefix: str) -> dict[str, int]:
    """Maps the names of the store's objects under prefix (without it) to their sizes."""
    ns = "{http://s3.amazonaws.com/doc/2006-03-01/}"
    root = ET.fromstring(s3_request("GET", f"/{S3_BUCKET}?list-type=2&prefix={prefix}"))
    return {c.find(ns + "Key").text[len(prefix):]: int(c.find(ns + "Size").text) for c in root.iter(ns + "Contents")}


def s3_delete(prefix: str) -> None:
    for name in s3_objects(prefix):
        s3_request("DELETE", f"/{S3_BUCKET}/{prefix}{name}")
    # Reclaim the deleted objects' space.
    subprocess.run(["docker", "exec", WEED_CONTAINER, "wget", "-qO-",
                    "http://localhost:9333/vol/vacuum?garbageThreshold=0.0001"], capture_output=True)


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

    def ask(self, sb: Sandbox, command: str, prefix: str, timeout: float) -> tuple[float, str]:
        """Sends command and waits for its answer: (seconds it took, the answer)."""
        n = len(self.lines)
        t0 = time.monotonic()
        sb.send(command)
        t, line = self.wait(prefix, timeout, n)
        return t - t0, line


class Sandbox:
    """One container (and sandbox) under a runsc binary, in its own systemd scope."""

    def __init__(self, a: argparse.Namespace, cid: str, run_dir: Path):
        self.a, self.cid, self.dir = a, cid, run_dir
        self.bundle = run_dir / "bundle"
        self.logs = run_dir / "logs"
        self.logs.mkdir(parents=True)
        self.n = 0  # incarnation: 0 = run, 1.. = restores
        self.fifo_fd: int | None = None
        self.unit: str | None = None

    def runsc(self, *args: str, extra: list[str] | tuple[str, ...] = ()) -> list[str]:
        flags = [f"--root={STATE}", "--ignore-cgroups", "--network=none", "--overlay2=root:memory",
                 f"--debug-log={self.logs}/"]
        if self.a.debug:
            flags.append("--debug")
        if self.a.tracking != "off":
            flags.append(f"--dirty-tracking={self.a.tracking}")
        return [self.a.runsc, *flags, *self.a.runsc_flag, *extra, *args]

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

    def _start(self, verb: list[str], props: list[str], extra: list[str]) -> tuple[subprocess.Popen, Tail]:
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
               "--", *self.runsc(*verb, extra=extra)]
        # O_APPEND: a restored workload keeps writing at its old stdout offset otherwise.
        with open(out, "ab") as o, open(self.dir / f"runsc-{self.n}.err", "ab") as e:
            proc = subprocess.Popen(cmd, stdin=self.fifo_fd, stdout=o, stderr=e)
        return proc, Tail(out)

    def send(self, line: str) -> None:
        os.write(self.fifo_fd, (line + "\n").encode())

    def boot_log(self) -> list[str]:
        """The lines that the sandbox's processes (runsc boot, the Sentry) logged so far,
        those of earlier incarnations first."""
        lines = []
        for f in sorted(self.logs.glob("*boot*")):
            lines += f.read_text(errors="replace").splitlines()
        return lines

    def memory_peak_mib(self) -> float | None:
        cg = sh("systemctl", "show", "-P", "ControlGroup", f"{self.unit}.scope").stdout.strip()
        try:
            return int(Path(f"/sys/fs/cgroup{cg}/memory.peak").read_text()) / MIB
        except (OSError, ValueError):
            return None

    def run(self, props: list[str]) -> Tail:
        self.make_bundle()
        t0 = time.monotonic()
        proc, tail = self._start(["run", "--detach", f"--bundle={self.bundle}", self.cid], props, [])
        if proc.wait() != 0:
            raise RuntimeError(f"runsc run failed: {(self.dir / 'runsc-0.err').read_text()[-2000:]}")
        t, _ = tail.wait("READY", self.a.timeout)
        log(f"  ready in {t - t0:.2f} s")
        return tail

    def restore(self, image: Path, flags: list[str], props: list[str],
                extra: list[str]) -> tuple[subprocess.Popen, Tail, float]:
        self.n += 1
        t0 = time.monotonic()
        proc, tail = self._start(
            ["restore", "--detach", f"--bundle={self.bundle}", f"--image-path={image}", *flags, self.cid],
            props, extra)
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

    def close(self) -> None:
        self.destroy()
        if self.fifo_fd is not None:
            os.close(self.fifo_fd)
            self.fifo_fd = None


# ---------------------------------------------------------------------------- measuring


def evict(image: Path) -> None:
    """Drops the image's pages from the page cache, so that a restore reads the disk
    (or, for an image in the S3 store, so that the store does)."""
    root = WEED / "data" if (image / "s3_opts.json").exists() else image
    subprocess.run(["sync"], check=True)
    for d, _, files in os.walk(root):
        for name in files:
            fd = os.open(os.path.join(d, name), os.O_RDONLY)
            try:
                os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
            finally:
                os.close(fd)


def disk_of(path: Path) -> str:
    """The whole-disk device holding `path` (io.max applies to whole disks)."""
    src = sh("findmnt", "-nvo", "SOURCE", "--target", str(path)).stdout.strip()
    parent = sh("lsblk", "-no", "PKNAME", src).stdout.strip()
    return f"/dev/{parent}" if parent else src


def image_bytes(image: Path) -> dict[str, int]:
    """The sizes of the image's files, in its directory or in the S3 store."""
    opts = image / "s3_opts.json"
    if opts.exists():
        return s3_objects(json.loads(opts.read_text())["object_prefix"])
    return {f.name: f.stat().st_size for f in image.iterdir() if f.is_file()}


def gaps(lines: list[tuple[float, str]]) -> list[float]:
    return [float(l.split()[1]) for _, l in lines if l.startswith("GAP ")]


def stamp(line: str) -> float | None:
    """The time of day, in seconds, at which runsc logged line."""
    m = re.match(r"[IWDE]\d{4} (\d\d):(\d\d):(\d\d\.\d+)", line)
    return int(m[1]) * 3600 + int(m[2]) * 60 + float(m[3]) if m else None


def go_duration(s: str) -> float:
    """Seconds in a Go duration's string, such as 1m2.5s, 812ms or 40µs."""
    units = {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "us": 1e-6, "µs": 1e-6, "ns": 1e-9}
    return sum(float(v) * units[u] for v, u in re.findall(r"([\d.]+)(h|ms|m|s|us|µs|ns)", s))


def save_log(lines: list[str]) -> dict:
    """What the Sentry logged of a checkpoint: its pause, from pausing to resuming
    the tasks (with --leave-running), and its pre-copy rounds."""
    r: dict = {}
    for l in lines:
        if "pausing all tasks" in l:
            r["paused_at"] = stamp(l)
        elif "Tasks resumed after save" in l:
            r["resumed_at"] = stamp(l)
        elif m := re.search(r"Pre-copy round (\d+): wrote (\d+) bytes in (\S+); (\d+) bytes dirty since", l):
            r.setdefault("precopy_rounds", []).append(
                {"round": int(m[1]), "bytes": int(m[2]), "s": go_duration(m[3]), "pending": int(m[4])})
        elif m := re.search(r"Pre-copy done after (\d+) rounds \((.*)\): (\d+) bytes in (\S+)", l):
            r["precopy"] = {"rounds": int(m[1]), "stop": m[2], "bytes": int(m[3]), "s": go_duration(m[4])}
    if "paused_at" in r and "resumed_at" in r:
        r["sentry_pause_s"] = (r["resumed_at"] - r["paused_at"]) % 86400
    return r


def checkpoint(a: argparse.Namespace, sb: Sandbox, tail: Tail, image: Path, flags: list[str]) -> dict:
    """Checkpoints sb to image with flags; with --leave-running among them, watches
    the workload's stalls for --observe seconds after."""
    mark = len(sb.boot_log())
    n = len(tail.lines)
    t0 = time.monotonic()
    ck = subprocess.run(sb.runsc("checkpoint", f"--image-path={image}", *flags, sb.cid), capture_output=True, text=True)
    c: dict = {"flags": flags, "s": time.monotonic() - t0, "started": t0}
    if ck.returncode != 0:
        raise RuntimeError(f"checkpoint {' '.join(flags)} failed: {ck.stderr[-2000:]}")
    if "--leave-running" in flags:
        time.sleep(a.observe)
        tail.poll()
        c["gaps_ms"] = gaps(tail.lines[n:])
        c["pause_inside_s"] = max(c["gaps_ms"], default=0.0) / 1e3
    c.update(save_log(sb.boot_log()[mark:]))
    c["image_bytes"] = image_bytes(image)
    return c


def checkpoint_values(prefix: str, c: dict) -> dict[str, float]:
    v = {f"{prefix} (s)": c["s"], f"{prefix}: image (MiB)": sum(c["image_bytes"].values()) / MIB}
    if "pause_inside_s" in c:
        v[f"{prefix}: pause inside (s)"] = c["pause_inside_s"]
    if "sentry_pause_s" in c:
        v[f"{prefix}: pause logged (s)"] = c["sentry_pause_s"]
    if "precopy" in c:
        v[f"{prefix}: pre-copy rounds"] = c["precopy"]["rounds"]
        v[f"{prefix}: pre-copy written (MiB)"] = c["precopy"]["bytes"] / MIB
    return v


LOADED_RE = re.compile(r"Async page loading completed in (\S+) \((\d+) bytes.*?(\d+) waiters waited (\S+)~(\S+) for (\d+) bytes")
PREFETCH_RES = {
    "working_set": re.compile(r"Working set: (\d+) bytes in (\d+) extents touched in (\S+)"),
    "prefetch_read": re.compile(r"Async page loading read (\d+) of (\d+) bytes of working sets first"),
    "prefetch_skipped": re.compile(r"Async page loading: not reading working sets first"),
}


def parse_restore(spec: str) -> tuple[list[str], int | None]:
    """'background+direct@100' -> (['--background', '--direct'], 100 MiB/s)."""
    m = re.fullmatch(r"([a-z+]+)(?:@(\d+))?", spec)
    if not m:
        raise ValueError(spec)
    flags = [f"--{w}" for w in m.group(1).split("+") if w != "plain"]
    for f in flags:
        if f not in ("--background", "--direct"):
            raise ValueError(f"unknown restore option {f} in {spec}")
    return flags, int(m.group(2)) if m.group(2) else None


def restore(a: argparse.Namespace, sb: Sandbox, image: Path, spec: str, layers: list[Path] = (),
            extra: list[str] = ()) -> dict:
    """Restores image (cold, as its layers) as spec says, with global runsc flags extra;
    times the first answer, and in the background, loading every page; checks the
    workload's memory; then destroys the sandbox."""
    rflags, mibps = parse_restore(spec)
    for d in (image, *layers):
        evict(d)
    props = []
    if mibps:
        if (image / "s3_opts.json").exists():
            raise ValueError("an image in the S3 store is read at the store's speed")
        props = [f"IOReadBandwidthMax={a.throttle_dev or disk_of(image)} {mibps}M"]
    mark = len(sb.boot_log())
    proc, tail, t0 = sb.restore(image, [*rflags, *(f"--layer-path={d}" for d in layers)], props, list(extra))
    sb.send("ping 1")  # waits in the FIFO until the restored workload reads it
    while (rc := proc.poll()) is None:  # the answer may come before the command returns
        tail.poll()
        time.sleep(0.002)
    t_ret = time.monotonic()
    if rc != 0:
        raise RuntimeError(f"restore {spec} failed: {(sb.dir / f'runsc-{sb.n}.err').read_text()[-2000:]}")
    t_ans, _ = tail.wait("PONG 1", a.timeout)
    rr: dict = {"restore": spec, "flags": [*rflags, *extra], "layers": [str(d) for d in layers], "read_mib_per_s": mibps,
                "command_s": t_ret - t0, "first_answer_s": t_ans - t0}
    if "--background" in rflags:
        # Wait for every page before touching them, so that the touches do not
        # hurry the loading being timed.
        deadline = time.monotonic() + a.timeout
        while not (m := next((m for l in sb.boot_log()[mark:] if (m := LOADED_RE.search(l))), None)):
            if time.monotonic() > deadline:
                raise TimeoutError(f"restore {spec}: loading did not complete within {a.timeout} s")
            time.sleep(0.05)
        rr.update(loaded_s=go_duration(m[1]), loaded_bytes=int(m[2]), waiters=int(m[3]),
                  waited_s=go_duration(m[5]), waited_bytes=int(m[6]))
    rr["touch_all_s"], line = tail.ask(sb, "touch", "TOUCHED", a.timeout)
    rr["touch_all_inside_ms"] = float(line.split()[1])
    _, line = tail.ask(sb, "verify", "VERIFIED", a.timeout)
    rr["verified_pages"], rr["wrong_pages"] = map(int, line.split()[1:])
    if rr["wrong_pages"]:
        raise RuntimeError(f"restore {spec}: {rr['wrong_pages']} of {rr['verified_pages']} pages lost their last write")
    time.sleep(a.observe)
    tail.poll()
    g = gaps(tail.lines)
    # The first gap spans checkpoint-to-restore; the rest are stalls after it.
    rr.update(downtime_gap_ms=g[0] if g else None, later_gaps_ms=g[1:], memory_peak_mib=sb.memory_peak_mib())
    lines = sb.boot_log()[mark:]
    for k, rx in PREFETCH_RES.items():
        rr[k] = next((m.group(0) for l in lines if (m := rx.search(l))), None)
    sb.destroy()
    log(f"  restore {spec} {' '.join(extra)}: command {rr['command_s']:.3f} s, first answer {rr['first_answer_s']:.3f} s"
        + (f", all loaded {rr['loaded_s']:.3f} s" if "loaded_s" in rr else "")
        + f", {rr['verified_pages']} pages verified")
    return rr


def restore_values(prefix: str, rr: dict) -> dict[str, float]:
    v = {f"{prefix}: command (s)": rr["command_s"], f"{prefix}: first answer (s)": rr["first_answer_s"],
         f"{prefix}: touch all (s)": rr["touch_all_s"],
         f"{prefix}: later stalls max (s)": max(rr["later_gaps_ms"], default=0) / 1e3}
    if "loaded_s" in rr:
        v[f"{prefix}: all loaded (s)"] = rr["loaded_s"]
    if rr["prefetch_read"]:
        read = int(PREFETCH_RES["prefetch_read"].search(rr["prefetch_read"])[1])
        v[f"{prefix}: working set read first (MiB)"] = read / MIB
    return v


def describe(a: argparse.Namespace, *more: str) -> str:
    """The configuration of a measurement, which results are grouped and compared by."""
    words = [f"{'C' if a.workload == 'c' else 'Python'} {a.mib} MiB, dirty {a.dirty_pct:g}%/s"]
    if a.tracking != "off":
        words.append(f"--dirty-tracking={a.tracking}")
    return ", ".join([*words, *more])


class Rep:
    """One repetition's sandbox and run directory, cleaned up when it ends."""

    def __init__(self, a: argparse.Namespace, kind: str):
        self.cid = f"{kind}-{a.workload}{a.mib}-{uuid.uuid4().hex[:6]}"
        self.dir = RUNS / self.cid
        self.a = a

    def __enter__(self) -> Sandbox:
        self.sb = Sandbox(self.a, self.cid, self.dir)
        return self.sb

    def __exit__(self, *exc) -> None:
        self.sb.close()
        if self.a.keep:
            return
        shutil.rmtree(self.dir, ignore_errors=True)
        if self.a.s3:
            s3_delete(f"{self.cid}/")


def start(a: argparse.Namespace, sb: Sandbox, props: list[str] = ()) -> Tail:
    tail = sb.run(list(props))
    time.sleep(a.warmup)
    tail.ask(sb, "ping 0", "PONG 0", 30)
    return tail


# ---------------------------------------------------------------------------- run


def run_rep(a: argparse.Namespace) -> dict:
    with Rep(a, "run") as sb:
        props = []
        if a.write_mib_per_s:
            props = [f"IOWriteBandwidthMax={a.throttle_dev or disk_of(RUNS)} {a.write_mib_per_s}M"]
        tail = start(a, sb, props)
        image = sb.dir / "image"
        if a.s3:
            image.mkdir()
            # Readable by its owner only, since it holds credentials: the
            # checkpoint gofer refuses it otherwise.
            fd = os.open(image / "s3_opts.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w") as f:
                json.dump({
                    "endpoint": S3, "bucket": S3_BUCKET, "object_prefix": f"{sb.cid}/",
                    "credentials": {"access_key_id": S3_KEY, "secret_access_key": S3_SECRET}}, f)
        flags = (["--leave-running"] if a.leave_running else []) + a.checkpoint_flag
        ck = checkpoint(a, sb, tail, image, flags)
        ck["memory_peak_mib"] = sb.memory_peak_mib()
        sb.destroy()
        values = checkpoint_values("checkpoint", ck)
        restores = []
        for spec in a.restore:
            rr = restore(a, sb, image, spec)
            restores.append(rr)
            values.update(restore_values(f"restore {spec}", rr))
        notes = {}
        if "precopy" in ck:
            notes["pre-copy stop"] = ck["precopy"]["stop"]
        log(f"checkpoint {ck['s']:.3f} s, pause inside {ck.get('pause_inside_s')} s, "
            f"image {values['checkpoint: image (MiB)']:.0f} MiB {notes}")
        more = [f"checkpoint {' '.join(a.checkpoint_flag) or '(defaults)'}"]
        if a.s3:
            more.append("image in S3")
        if a.write_mib_per_s:
            more.append(f"writes at {a.write_mib_per_s} MiB/s")
        section = ("S3" if a.s3 else "throttled-writes" if a.write_mib_per_s else "restore")
        return {"section": section, "config": describe(a, *more), "values": values, "notes": notes,
                "checkpoint": ck, "restores": restores}


# ---------------------------------------------------------------------------- incremental


def stat_lines(tail: Tail, t0: float, t1: float) -> list[list[float]]:
    """The workload's STAT lines printed between host times t0 and t1."""
    return [list(map(float, l.split()[1:])) for t, l in tail.lines if l.startswith("STAT ") and t0 < t <= t1]


def incremental_rep(a: argparse.Namespace) -> dict:
    with Rep(a, "incremental") as sb:
        tail = start(a, sb)
        first, second = sb.dir / "first", sb.dir / "second"
        ck1 = checkpoint(a, sb, tail, first, ["--leave-running"])
        delta = a.tracking != "off"
        time.sleep(max(0.0, a.dirty_seconds - a.observe))
        ck2 = checkpoint(a, sb, tail, second, ["--leave-running"] + ([f"--parent-image-path={first}"] if delta else []))
        sb.destroy()
        # What the workload wrote between the two checkpoints: its rate times the
        # time its tasks ran, from resuming after the first to pausing for the second.
        pages_per_s = int(a.mib * MIB // 4096 * a.dirty_pct / 100 / a.burst_hz) * a.burst_hz
        ran_s = (ck2["paused_at"] - ck1["resumed_at"]) % 86400
        dirtied = min(pages_per_s * ran_s, a.mib * MIB // 4096) * 4096
        # The workload's bursts after the first checkpoint (the first second
        # aside, which the checkpoint's pause may have split), all first writes
        # since it: the cost of tracking them.
        stats = stat_lines(tail, ck1["started"] + ck1["s"] + 1, ck2["started"])
        per_page_us = statistics.median(s[2] / (s[1] / s[0]) for s in stats) if stats else None
        values = {**checkpoint_values("first checkpoint", ck1), **checkpoint_values("second checkpoint", ck2)}
        if delta:
            values["dirtied in between (MiB)"] = dirtied / MIB
            values["delta's pages / dirtied (ratio)"] = ck2["image_bytes"]["pages.img"] / dirtied
        if per_page_us is not None:
            values["write after a checkpoint, per page (µs)"] = per_page_us
        restores = []
        for spec in a.restore:
            rr = restore(a, sb, second, spec, [first] if delta else [])
            restores.append(rr)
            values.update(restore_values(f"restore {spec}", rr))
        log(f"second checkpoint: {values['second checkpoint: image (MiB)']:.1f} MiB, "
            f"dirtied {dirtied / MIB:.1f} MiB in {ran_s:.2f} s; write per page {per_page_us} µs")
        more = ["second checkpoint a delta" if delta else "both checkpoints full", f"{a.dirty_seconds:g} s apart"]
        return {"section": "incremental", "config": describe(a, *more), "values": values,
                "checkpoints": [ck1, ck2], "ran_s": ran_s, "stats": stats, "restores": restores}


# ---------------------------------------------------------------------------- working sets


def working_set_rep(a: argparse.Namespace) -> dict:
    with Rep(a, "working-set") as sb:
        tail = start(a, sb)
        p, c, compact = sb.dir / "p", sb.dir / "c", sb.dir / "c-compact"
        checkpoint(a, sb, tail, p, [])
        sb.destroy()
        # Restore P, have it answer, and checkpoint it with what it touched since.
        mark = len(sb.boot_log())
        proc, tail, _ = sb.restore(p, ["--background"], [], [f"--working-set-window={a.window}"])
        sb.send("ping 1")
        if proc.wait() != 0:
            raise RuntimeError((sb.dir / f"runsc-{sb.n}.err").read_text()[-2000:])
        tail.wait("PONG 1", a.timeout)
        checkpoint(a, sb, tail, c, [])
        sb.destroy()
        m = next((m for l in sb.boot_log()[mark:] if (m := PREFETCH_RES["working_set"].search(l))), None)
        if not m:
            raise RuntimeError("the restored sandbox recorded no working set")
        values = {"recorded working set (MiB)": int(m[1]) / MIB}
        t0 = time.monotonic()
        sh(a.runsc, "image", "compact", "--working-set-first", f"--output={compact}", str(c))
        values["compact --working-set-first (s)"] = time.monotonic() - t0
        restores = []
        for name, image in (("C", c), ("C compacted", compact)):
            for prefetch in ("auto", "off"):
                for spec in a.restore:
                    rr = restore(a, sb, image, spec, extra=[f"--working-set-prefetch={prefetch}"])
                    rr.update(image=name, prefetch=prefetch)
                    restores.append(rr)
                    values.update(restore_values(f"{name}, restore {spec}, prefetch {prefetch}", rr))
        log(f"working set {m.group(0)}")
        return {"section": "working-set", "config": describe(a, f"recorded for {a.window}"), "values": values,
                "recorded": m.group(0), "restores": restores}


# ---------------------------------------------------------------------------- driver


def measure(a: argparse.Namespace) -> None:
    if not (ROOTFS / "bench/dirtier").exists():
        sys.exit("run `bench.py prepare` first")
    version = sh(a.runsc, "--version").stdout.splitlines()[0]
    out = open(a.out, "a") if a.out else None
    results = []
    for rep in range(a.reps):
        r = {"rep": rep, "runsc": a.runsc, "runsc_version": version, "runsc_flags": a.runsc_flag,
             "loadavg_start": os.getloadavg()[0], **a.rep(a)}
        results.append(r)
        if out:
            out.write(json.dumps(r) + "\n")
            out.flush()
    print(markdown(results, []))


# ---------------------------------------------------------------------------- summary

SECTIONS = {
    "restore": ("Checkpoint and restore",
                "A checkpoint with --leave-running (the pause is the longest stall the workload saw), then "
                "cold restores of it: plainly, in the background, with direct I/O, from local disk or read "
                "at @MiB/s. *First answer* is the workload's answer to a command sent with the restore; "
                "*all loaded*, the time runsc logged to load every page in the background."),
    "incremental": ("Incremental checkpoints",
                    "A full checkpoint, seconds of dirtying, then a delta of it (--parent-image-path) restored "
                    "as a chain (--layer-path), against the same run without tracking, where the second "
                    "checkpoint is full. The delta's pages should match what the workload dirtied in "
                    "between (ratio near 1); the write per page is the median burst's time per page in "
                    "the seconds after the first checkpoint, all first writes."),
    "throttled-writes": ("Checkpoints to a store throttled for writes",
                         "The sandbox's writes are throttled (cgroup io.max), as for a remote store: "
                         "stop-the-world against pre-copy (--precopy=on), at a dirtying rate below and "
                         "above half the store's speed, where the halving rule must stop the rounds."),
    "S3": ("Checkpoints in an S3 store",
           "Checkpoints to, and background restores from, a SeaweedFS store on the runner, through the "
           "checkpoint gofer (s3_opts.json)."),
    "working-set": ("Working sets",
                    "An image whose working set was recorded after a restore, and the same image compacted "
                    "with --working-set-first, restored with --working-set-prefetch=auto and off."),
}


def load(path: str) -> list[dict]:
    with open(path) as f:
        return [json.loads(l) for l in f if l.strip()]


def medians(results: list[dict]) -> dict[tuple[str, str, str], tuple[float, list[float]]]:
    """Maps (section, configuration, measurement) to its median and its repetitions."""
    found: dict = {}
    for r in results:
        for name, v in r["values"].items():
            found.setdefault((r["section"], r["config"], name), []).append(v)
    return {k: (statistics.median(v), v) for k, v in found.items()}


def fmt(x: float) -> str:
    return f"{x:.0f}" if abs(x) >= 100 or x == int(x) else f"{x:.3f}"


def markdown(results: list[dict], baseline: list[dict]) -> str:
    new, base = medians(results), medians(baseline)
    notes: dict = {}
    for r in results:
        for k, v in r.get("notes", {}).items():
            notes.setdefault((r["section"], r["config"]), {}).setdefault(k, []).append(v)
    out = []
    for section in dict.fromkeys(k[0] for k in new):
        title, about = SECTIONS[section]
        out += [f"### {title}", "", about, ""]
        for config in dict.fromkeys(k[1] for k in new if k[0] == section):
            out += [f"#### {config}", ""]
            out += ["| measurement | this build: median (range) | baseline |", "|---|---|---|"]
            for (s, c, name), (m, reps) in new.items():
                if (s, c) != (section, config):
                    continue
                b = base.get((s, c, name))
                out.append(f"| {name} | {fmt(m)} ({fmt(min(reps))}–{fmt(max(reps))}) | {fmt(b[0]) if b else '–'} |")
            for k, vs in notes.get((section, config), {}).items():
                out += ["", f"{k}: " + ", ".join(f"{v} ({vs.count(v)} of {len(vs)})" for v in dict.fromkeys(vs))]
            out.append("")
    return "\n".join(out)


def summary(a: argparse.Namespace) -> None:
    print(markdown(load(a.results), load(a.baseline) if a.baseline else []))


# ---------------------------------------------------------------------------- clean


def clean(a: argparse.Namespace) -> None:
    """Kills every sandbox under our --root, stops our scopes and store, removes our files."""
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
    subprocess.run(["docker", "rm", "-f", WEED_CONTAINER], capture_output=True)
    for p in (STATE, RUNS, WEED):
        shutil.rmtree(p, ignore_errors=True)
    if a.all:
        shutil.rmtree(ROOTFS, ignore_errors=True)
    log("clean")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    pp = sub.add_parser("prepare")
    pp.add_argument("--image", default=ROOTFS_IMAGE)
    pp.set_defaults(func=prepare)

    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--runsc", default="/usr/local/bin/runsc", help="runsc binary (its gvisor-bin/ next to it)")
    common.add_argument("--runsc-flag", action="append", default=[],
                        help="extra global runsc flag (repeatable), not part of the configuration")
    common.add_argument("--tracking", choices=["off", "wp"], default="off", help="the sandbox's --dirty-tracking")
    common.add_argument("--workload", choices=["c", "py"], default="c")
    common.add_argument("--mib", type=int, default=512, help="working set (MiB)")
    common.add_argument("--dirty-pct", type=float, default=5, help="share of the working set written per second (%%)")
    common.add_argument("--burst-hz", type=int, default=10)
    common.add_argument("--gap-ms", type=float, default=10, help="smallest stall the workload reports")
    common.add_argument("--restore", action="append", default=[],
                        help="restore variant, repeatable: plain|background[+direct][@MiB/s]")
    common.add_argument("--throttle-dev", help="device for IO{Read,Write}BandwidthMax (default: the image's disk)")
    common.add_argument("--memory-max", default="1536M", help="MemoryMax of each sandbox's scope")
    common.add_argument("--warmup", type=float, default=3, help="seconds of dirtying before the first checkpoint")
    common.add_argument("--observe", type=float, default=2, help="seconds watched after checkpoint/restore")
    common.add_argument("--reps", type=int, default=3)
    common.add_argument("--timeout", type=float, default=120, help="seconds to wait for any answer")
    common.add_argument("--out", help="append one JSON line per repetition here")
    common.add_argument("--keep", action="store_true", help="keep runs/<cid>/ (logs, images)")
    common.add_argument("--debug", action="store_true", help="runsc --debug")

    pr = sub.add_parser("run", parents=[common], help="a checkpoint, then restores of it")
    pr.add_argument("--checkpoint-flag", action="append", default=[],
                    help="e.g. --checkpoint-flag=--direct --checkpoint-flag=--precopy=on")
    pr.add_argument("--no-leave-running", dest="leave_running", action="store_false")
    pr.add_argument("--write-mib-per-s", type=int, help="throttle the sandbox's writes (its checkpoint's)")
    pr.add_argument("--s3", action="store_true", help="keep the image in the store of `bench.py seaweedfs start`")
    pr.set_defaults(func=measure, rep=run_rep, default_restore="plain")

    pi = sub.add_parser("incremental", parents=[common], help="a full checkpoint, then a delta, restored")
    pi.add_argument("--dirty-seconds", type=float, default=8, help="seconds between the two checkpoints")
    pi.set_defaults(func=measure, rep=incremental_rep, s3=False, default_restore="plain")

    pw = sub.add_parser("working-set", parents=[common], help="restores of an image with a working set")
    pw.add_argument("--window", default="3s", help="--working-set-window of the restore that records it")
    pw.set_defaults(func=measure, rep=working_set_rep, s3=False, default_restore="background@100")

    pz = sub.add_parser("seaweedfs", help="start or stop the S3 store")
    pz.add_argument("action", choices=["start", "stop"])
    pz.set_defaults(func=seaweedfs)

    ps = sub.add_parser("summary", help="the results' medians as Markdown")
    ps.add_argument("results")
    ps.add_argument("--baseline", help="results of a baseline measured alongside")
    ps.set_defaults(func=summary)

    pc = sub.add_parser("clean")
    pc.add_argument("--runsc", default="/usr/local/bin/runsc")
    pc.add_argument("--all", action="store_true", help="also the rootfs")
    pc.set_defaults(func=clean)

    a = p.parse_args()
    if getattr(a, "rep", None) and not a.restore:
        a.restore = [a.default_restore]
    a.func(a)


if __name__ == "__main__":
    main()
