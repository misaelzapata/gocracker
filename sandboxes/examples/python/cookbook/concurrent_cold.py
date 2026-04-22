#!/usr/bin/env python3
"""Cookbook 10/10: N concurrent cold-boots (stress test for the lock path).

Exercises the artifact_lock + concurrent container.Run work
we stabilized in PR #12. No pool — every sandbox cold-boots from
scratch. Useful for reproducing any regression in the cold-boot
concurrency path.

Usage:
  sudo python3 concurrent_cold.py [N] [KERNEL_PATH]
"""
from __future__ import annotations

import concurrent.futures
import statistics
import sys
import time
from pathlib import Path

repo_root = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(repo_root / "sdk" / "python"))

from gocracker import Client, SandboxError  # noqa: E402


def main() -> int:
    n = int(sys.argv[1]) if len(sys.argv) > 1 else 10
    kernel = sys.argv[2] if len(sys.argv) > 2 else "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux"

    client = Client("http://127.0.0.1:9091", timeout=300.0)

    def create_one(_i: int):
        t0 = time.perf_counter()
        try:
            sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel)
            elapsed_ms = (time.perf_counter() - t0) * 1000.0
            # Verify exec works right away.
            tb = sb.toolbox()
            tb.exec(["echo", "ok"], timeout=5.0)
            client.delete(sb.id)
            return elapsed_ms, None
        except Exception as e:
            return (time.perf_counter() - t0) * 1000.0, e

    print(f"firing {n} concurrent cold-boots...")
    start = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=n) as ex:
        results = list(ex.map(create_one, range(n)))
    total_ms = (time.perf_counter() - start) * 1000.0

    oks = [ms for ms, err in results if err is None]
    fails = [(ms, err) for ms, err in results if err is not None]

    if oks:
        oks.sort()
        print()
        print("=" * 64)
        print(f"burst-{n} cold-boot ({len(oks)} ok / {len(fails)} fail, total {total_ms:.0f} ms)")
        print("-" * 64)
        print(f"  min   {oks[0]:7.1f} ms")
        print(f"  p50   {oks[len(oks)//2]:7.1f} ms")
        print(f"  mean  {statistics.mean(oks):7.1f} ms")
        if len(oks) > 1:
            print(f"  p95   {oks[int(len(oks)*0.95)]:7.1f} ms")
        print(f"  max   {oks[-1]:7.1f} ms")
        print("=" * 64)

    if fails:
        print(f"\n{len(fails)} failures:")
        for ms, err in fails[:3]:
            print(f"  {ms:.0f} ms: {err}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
