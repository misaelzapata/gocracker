#!/usr/bin/env python3
"""Cookbook 7/10: git clone inside the guest + inspect status.

Requires network_mode=auto for DNS/egress. Clones a small public
repo, runs git status to verify clean tree.

Usage:
  sudo python3 git_clone.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url())

    # Plain alpine + apk add git. We can't use alpine/git:latest because
    # its ENTRYPOINT=["git"] runs at boot and exits immediately, which
    # panics the guest kernel (PID 1 exit). Sandboxd doesn't yet surface
    # a way to override image ENTRYPOINT, so we install git at runtime.
    sb = client.create_sandbox(
        image="alpine:3.20",
        kernel_path=kernel,
        network_mode="auto",
    )
    print(f"sandbox id={sb.id} guest_ip={sb.guest_ip}")

    try:
        tb = sb.toolbox()
        print("installing git...")
        r = tb.exec(["apk", "add", "--no-cache", "git"], timeout=60)
        if r.exit_code != 0:
            print(f"apk add failed: exit={r.exit_code} stderr={r.stderr_text[:300]}")
            return 1
        print("cloning https://github.com/octocat/Hello-World.git...")
        resp = tb.git_clone("https://github.com/octocat/Hello-World.git", "/tmp/hello")
        print(f"clone response: {resp}")

        entries = tb.list_files("/tmp/hello")
        print(f"cloned files: {sorted(e.name for e in entries)}")

        status = tb.git_status("/tmp/hello")
        print(f"git status: {status}")
    finally:
        sb.delete()

    return 0


if __name__ == "__main__":
    sys.exit(main())
