#!/usr/bin/env python3
"""Cookbook 1/5: create a sandbox + exec a command.

Usage:
  python hello_world.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url())

    if not client.healthz():
        print("sandboxd not reachable at 127.0.0.1:9091", file=sys.stderr)
        return 1

    print(f"creating sandbox (alpine:3.20, kernel={kernel})...")
    sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel)
    print(f"  id={sb.id} guest_ip={sb.guest_ip} uds={sb.uds_path}")

    try:
        result = sb.toolbox().exec(["echo", "hello from gocracker"])
        print(f"exit={result.exit_code}")
        print(f"stdout: {result.stdout_text.rstrip()}")
        if result.stderr:
            print(f"stderr: {result.stderr_text.rstrip()}")
    finally:
        sb.delete()
        print(f"deleted id={sb.id}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
