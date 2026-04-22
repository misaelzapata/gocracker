#!/usr/bin/env python3
"""Cookbook 7/10: git clone inside the guest + inspect status.

Requires network_mode=auto for DNS/egress. Clones a small public
repo, runs git status to verify clean tree.

Usage:
  sudo python3 git_clone.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from pathlib import Path

repo_root = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(repo_root / "sdk" / "python"))

from gocracker import Client  # noqa: E402


def main() -> int:
    kernel = sys.argv[1] if len(sys.argv) > 1 else "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux"
    client = Client("http://127.0.0.1:9091")

    # Use an alpine image that has git pre-installed. alpine:3.20 doesn't;
    # alpine/git:latest does.
    sb = client.create_sandbox(
        image="alpine/git:latest",
        kernel_path=kernel,
        network_mode="auto",
    )
    print(f"sandbox id={sb.id} guest_ip={sb.guest_ip}")

    try:
        tb = sb.toolbox()
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
