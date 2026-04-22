#!/usr/bin/env python3
"""Cookbook 3/5: upload a file, read it back, list the directory.

NB: files endpoints on the toolbox agent aren't implemented yet in
this repo (see internal/toolbox/agent) — this example demonstrates
the SDK shape; enabling it end-to-end waits on the agent files
feature (toolbox v2 had it; current repo hasn't ported yet).

Usage:
  python files.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from pathlib import Path

repo_root = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(repo_root / "sdk" / "python"))

from gocracker import Client  # noqa: E402
from gocracker.toolbox import ToolboxError  # noqa: E402


def main() -> int:
    kernel = sys.argv[1] if len(sys.argv) > 1 else "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux"
    client = Client("http://127.0.0.1:9091")

    sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel)
    print(f"sandbox id={sb.id}")

    try:
        tb = sb.toolbox()
        content = b"hello from the host\nthis is line 2\n"
        try:
            tb.upload("/tmp/hello.txt", content)
            print(f"uploaded /tmp/hello.txt ({len(content)} bytes)")

            echoed = tb.download("/tmp/hello.txt")
            print(f"downloaded {len(echoed)} bytes: {echoed!r}")
            assert echoed == content, "downloaded content != uploaded"

            entries = tb.list_files("/tmp")
            print(f"/tmp has {len(entries)} entries")
            for e in entries[:5]:
                print(f"  {e.name:20} size={e.size} dir={e.is_dir}")
        except ToolboxError as e:
            print(f"files API unavailable in this agent build: {e}", file=sys.stderr)
            print("(files endpoints are a follow-up; see internal/toolbox/agent)", file=sys.stderr)
            return 2
    finally:
        sb.delete()

    return 0


if __name__ == "__main__":
    sys.exit(main())
