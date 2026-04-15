#!/usr/bin/env python3
"""Parallel external-repo sweep for gocracker."""
from __future__ import annotations
import argparse, csv, os, queue, re, signal, subprocess, sys, threading, time
from dataclasses import dataclass
from pathlib import Path

REPO_ROOT = Path(os.environ.get("REPO_ROOT", "/home/misael/Desktop/projects/gocracker"))
GC_BIN = REPO_ROOT / "gocracker"
KERNEL = REPO_ROOT / "artifacts/kernels/gocracker-guest-standard-vmlinux"
MANIFEST = REPO_ROOT / "tests/external-repos/manifest.tsv"
SHARED_CACHE = Path("/tmp/gocracker-shared-cache")

BOOT_RE = re.compile(r"VM .* is running|started id=")
RATELIMIT_RE = re.compile(r"TOOMANYREQUESTS|rate limit")


@dataclass
class Case:
    id: str
    kind: str = ""
    url: str = ""
    ref: str = ""
    subdir: str = "."
    mem_mb: int = 256
    disk_mb: int = 4096
    dockerfile: str = ""  # optional: non-canonical Dockerfile name (col 14)


@dataclass
class Result:
    id: str
    status: str
    duration_s: int
    reason: str = ""


def load_manifest() -> dict:
    cases = {}
    for line in open(MANIFEST):
        line = line.rstrip("\n")
        if not line or line.startswith("#"):
            continue
        parts = line.split("\t")
        if len(parts) < 12:
            continue
        cases[parts[0]] = Case(
            id=parts[0], kind=parts[1], url=parts[2], ref=parts[3],
            subdir=parts[4] or ".",
            mem_mb=int(parts[10] or 256), disk_mb=int(parts[11] or 4096),
            dockerfile=(parts[13].strip() if len(parts) > 13 else ""),
        )
    return cases


def read_ids(path: Path) -> list:
    return [ln.strip() for ln in open(path) if ln.strip() and not ln.startswith("#")]


def run_case(case: Case, log_dir: Path, boot_timeout: int, service_window: int) -> Result:
    SHARED_CACHE.mkdir(parents=True, exist_ok=True)
    log_path = log_dir / f"{case.id}.log"
    cmd = [
        "sudo", "-E", "env", f"PATH={os.environ.get('PATH', '')}",
        str(GC_BIN), "repo",
        "--url", case.url, "--ref", case.ref, "--subdir", case.subdir,
        "--kernel", str(KERNEL),
        "--mem", str(case.mem_mb), "--disk", str(case.disk_mb),
        "--tty", "off", "--jailer", "off",
        "--cache-dir", str(SHARED_CACHE),
    ]
    if case.dockerfile:
        cmd.extend(["--dockerfile", case.dockerfile])
    t0 = time.time()
    with open(log_path, "w") as lf:
        proc = subprocess.Popen(cmd, stdout=lf, stderr=subprocess.STDOUT, start_new_session=True)

    booted = False
    deadline = t0 + boot_timeout
    try:
        while time.time() < deadline:
            if proc.poll() is not None:
                break
            try:
                with open(log_path, errors="replace") as lf:
                    if BOOT_RE.search(lf.read()):
                        booted = True
                        break
            except FileNotFoundError:
                pass
            time.sleep(1)
        if not booted:
            try:
                with open(log_path, errors="replace") as lf:
                    if BOOT_RE.search(lf.read()):
                        booted = True
            except FileNotFoundError:
                pass
        if booted:
            time.sleep(service_window)
        if proc.poll() is None:
            try:
                os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
            except (PermissionError, ProcessLookupError):
                subprocess.run(["sudo", "kill", "-9", str(proc.pid)], check=False)
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            subprocess.run(["sudo", "kill", "-9", str(proc.pid)], check=False)
            proc.wait(timeout=5)
    finally:
        runs = SHARED_CACHE / "artifacts"
        if runs.exists():
            for artifact in runs.iterdir():
                rd = artifact / "runs"
                if rd.exists():
                    subprocess.run(["sudo", "rm", "-rf", str(rd)], check=False)

    duration = int(time.time() - t0)
    if booted:
        return Result(id=case.id, status="PASS", duration_s=duration)
    reason = ""
    try:
        text = open(log_path, errors="replace").read()
        lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
        if lines:
            reason = lines[-1][:200]
        if RATELIMIT_RE.search(text):
            reason = "RATELIMIT: " + reason
    except FileNotFoundError:
        reason = "no log"
    return Result(id=case.id, status="FAIL", duration_s=duration, reason=reason)


class Sweeper:
    def __init__(self, cases, ids, concurrency, boot_timeout, service_window, log_dir, results_path, summary_path):
        self.cases, self.ids = cases, ids
        self.concurrency, self.boot_timeout, self.service_window = concurrency, boot_timeout, service_window
        self.log_dir, self.results_path, self.summary_path = log_dir, results_path, summary_path
        self.results = []
        self.lock = threading.Lock()
        self.started, self.finished = 0, 0

    def worker(self, q):
        while True:
            try:
                case = q.get_nowait()
            except queue.Empty:
                return
            with self.lock:
                self.started += 1
                idx = self.started
            print(f"[{idx}/{len(self.ids)}] {case.id} STARTED", flush=True)
            try:
                res = run_case(case, self.log_dir, self.boot_timeout, self.service_window)
            except Exception as exc:
                res = Result(id=case.id, status="ERROR", duration_s=0, reason=str(exc))
            with self.lock:
                self.results.append(res)
                self.finished += 1
                n = self.finished
                passes = sum(1 for r in self.results if r.status == "PASS")
                fails = sum(1 for r in self.results if r.status == "FAIL")
                errs = sum(1 for r in self.results if r.status == "ERROR")
                self._write_results()
            msg = f"{res.status} {case.id} in {res.duration_s}s"
            if res.status != "PASS":
                msg += f"  ({res.reason})"
            print(f"    [{n}/{len(self.ids)}] {msg}  (pass={passes} fail={fails} err={errs})", flush=True)
            q.task_done()

    def _write_results(self):
        tmp = self.results_path.with_suffix(self.results_path.suffix + ".tmp")
        with open(tmp, "w", newline="") as f:
            w = csv.writer(f, delimiter="\t")
            w.writerow(["id", "status", "duration_s", "reason"])
            for r in self.results:
                w.writerow([r.id, r.status, r.duration_s, r.reason])
        tmp.replace(self.results_path)

    def run(self):
        q = queue.Queue()
        for cid in self.ids:
            case = self.cases.get(cid)
            if not case:
                print(f"skip {cid}: not in manifest", flush=True)
                continue
            q.put(case)
        threads = [threading.Thread(target=self.worker, args=(q,), daemon=True) for _ in range(self.concurrency)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        passes = sorted(r.id for r in self.results if r.status == "PASS")
        fails = sorted(r.id for r in self.results if r.status == "FAIL")
        errs = sorted(r.id for r in self.results if r.status == "ERROR")
        with open(self.summary_path, "w") as f:
            f.write(f"TOTAL: {len(self.results)}\n")
            f.write(f"PASS:  {len(passes)}\n")
            f.write(f"FAIL:  {len(fails)}\n")
            f.write(f"ERROR: {len(errs)}\n\nPASS IDs:\n")
            for i in passes:
                f.write(f"  {i}\n")
            f.write("\nFAIL IDs:\n")
            for r in sorted((r for r in self.results if r.status == "FAIL"), key=lambda r: r.id):
                f.write(f"  {r.id}  [{r.duration_s}s]  {r.reason}\n")
        print(f"\n== done: PASS={len(passes)} FAIL={len(fails)} ERROR={len(errs)}")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("ids_file")
    p.add_argument("--concurrency", type=int, default=3)
    p.add_argument("--boot-timeout", type=int, default=900)
    p.add_argument("--service-window", type=int, default=6)
    p.add_argument("--log-dir", default="/tmp/sweep")
    p.add_argument("--results", default="/tmp/sweep.tsv")
    p.add_argument("--summary", default="/tmp/sweep.summary.txt")
    args = p.parse_args()
    log_dir = Path(args.log_dir)
    log_dir.mkdir(parents=True, exist_ok=True)
    subprocess.run(["sudo", "pkill", "-9", "-f", "gocracker-vmm"], check=False)
    subprocess.run(["sudo", "pkill", "-9", "-f", "firecracker-v"], check=False)
    cases = load_manifest()
    ids = read_ids(Path(args.ids_file))
    Sweeper(cases, ids, args.concurrency, args.boot_timeout, args.service_window,
            log_dir, Path(args.results), Path(args.summary)).run()


if __name__ == "__main__":
    main()
