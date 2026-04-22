#!/usr/bin/env python3
"""Cookbook 5/5: 50 concurrent leases against a warm pool, measure p95.

Registers a pool with MinPaused=50, waits for warm fill, fires 50
concurrent lease requests, reports per-call latency stats. Plan §5
target: p95 < 20 ms steady state.

Usage:
  python pool_burst.py [KERNEL_PATH]
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
    kernel = sys.argv[1] if len(sys.argv) > 1 else "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux"
    burst = int(sys.argv[2]) if len(sys.argv) > 2 else 50

    client = Client("http://127.0.0.1:9091", timeout=300.0)
    template_id = "burst-pool"

    print(f"registering pool '{template_id}' MinPaused={burst} MaxPaused={burst}...")
    try:
        client.register_pool(
            template_id=template_id,
            image="alpine:3.20",
            kernel_path=kernel,
            min_paused=burst,
            max_paused=burst,
        )
    except SandboxError as e:
        if "already" not in str(e).lower():
            print(f"register_pool: {e}", file=sys.stderr)
            return 1
        print("(pool already registered, reusing)")

    # Wait for warm fill.
    print(f"warming up to {burst} paused VMs ", end="", flush=True)
    deadline = time.time() + 5 * 60
    while time.time() < deadline:
        pools = client.list_pools()
        for p in pools:
            if p.template_id == template_id:
                count = p.counts.get("paused", 0)
                if count >= burst:
                    print(f" ok ({count} paused)")
                    break
        else:
            time.sleep(0.5)
            print(".", end="", flush=True)
            continue
        break
    else:
        print(f" timeout (paused={count})", file=sys.stderr)
        return 1

    print(f"\nfiring {burst} concurrent leases...")
    leased_ids = []
    latencies_ms = []
    errors = 0

    def lease_one(_idx: int) -> tuple[float, str | None, Exception | None]:
        t0 = time.perf_counter()
        try:
            sb = client.lease_sandbox(template_id, timeout_ns=int(60e9))
            return (time.perf_counter() - t0) * 1000.0, sb.id, None
        except Exception as e:
            return (time.perf_counter() - t0) * 1000.0, None, e

    burst_start = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=burst) as ex:
        for elapsed_ms, sb_id, err in ex.map(lease_one, range(burst)):
            if err is not None:
                errors += 1
                print(f"  ERROR: {err}", file=sys.stderr)
                continue
            leased_ids.append(sb_id)
            latencies_ms.append(elapsed_ms)
    burst_total_ms = (time.perf_counter() - burst_start) * 1000.0

    # Cleanup.
    print(f"releasing {len(leased_ids)} leases...")
    for sb_id in leased_ids:
        try:
            client.delete(sb_id)
        except SandboxError:
            pass

    if not latencies_ms:
        print("no successful leases", file=sys.stderr)
        return 1

    latencies_ms.sort()
    print()
    print("=" * 64)
    print(f"burst-{burst} ({len(latencies_ms)} ok / {errors} fail, total {burst_total_ms:.0f} ms)")
    print("-" * 64)
    print(f"  min   {latencies_ms[0]:6.1f} ms")
    print(f"  p50   {latencies_ms[len(latencies_ms) // 2]:6.1f} ms")
    print(f"  mean  {statistics.mean(latencies_ms):6.1f} ms")
    print(f"  p95   {latencies_ms[int(len(latencies_ms) * 0.95)]:6.1f} ms")
    print(f"  p99   {latencies_ms[int(len(latencies_ms) * 0.99)]:6.1f} ms")
    print(f"  max   {latencies_ms[-1]:6.1f} ms")
    print("=" * 64)

    # Optional: tear down the pool.
    try:
        client.unregister_pool(template_id)
        print(f"\nunregistered pool {template_id}")
    except SandboxError:
        pass

    return 0 if errors == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
