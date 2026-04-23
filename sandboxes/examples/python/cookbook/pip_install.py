#!/usr/bin/env python3
"""Cookbook: pip install at runtime + use the freshly installed package.

Demonstrates that a vanilla `python:3.12-alpine` sandbox can pull a
package from PyPI and import it. Runs end-to-end: install -> import ->
use -> verify. A follow-up pattern is to snapshot the sandbox AFTER
install (see custom_template.py / base-python with Readiness probe)
so the install tax is paid once, not per lease.

Usage:
  sudo python3 pip_install.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
import time
from _common import resolve_kernel, sandboxd_url

from gocracker import Client, ProcessExitError  # noqa: E402


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url(), timeout=300)

    try:
        sb = client.create_sandbox(template="base-python", network_mode="auto")
        print("(using base-python warm template)")
    except Exception:
        sb = client.create_sandbox(image="python:3.12-alpine", kernel_path=kernel, network_mode="auto")

    print(f"sandbox id={sb.id}")
    try:
        t0 = time.perf_counter()
        try:
            r = sb.process.exec("pip install --quiet --break-system-packages 'httpx==0.28.1'", timeout=120)
            print(f"  pip install httpx ok in {round((time.perf_counter()-t0)*1000)}ms")
        except ProcessExitError as e:
            print(f"  pip install FAILED exit={e.exit_code}")
            print(f"  stderr: {e.stderr.strip()[:300]}")
            return 1

        r = sb.process.exec("python3 -c 'import httpx; print(httpx.__version__)'", timeout=10)
        print(f"  httpx version reported by guest: {r.stdout_text.strip()}")

        r = sb.process.exec("python3 -c 'import httpx; r=httpx.get(\"https://httpbin.org/uuid\", timeout=10); print(r.status_code, r.text.strip())'", timeout=15)
        print(f"  real HTTP call from guest: {r.stdout_text.strip()[:120]}")
    finally:
        sb.delete()
    return 0


if __name__ == "__main__":
    sys.exit(main())
