#!/usr/bin/env python3
"""Cookbook 6/10: full file surface — upload + list + download + mkdir + rename + chmod + delete.

Exercises every file endpoint on the toolbox agent (handleListFiles,
handleDownloadFile, handleUploadFile, handleDeleteFile, handleMkdir,
handleRename, handleChmod).

Usage:
  sudo python3 files_full.py [KERNEL_PATH]
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

    sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel)
    print(f"sandbox id={sb.id}")

    try:
        tb = sb.toolbox()

        # mkdir with parents
        tb.mkdir("/tmp/demo/nested/deep", parents=True)
        print("mkdir -p /tmp/demo/nested/deep ok")

        # upload
        content = b"line 1\nline 2\nline 3\n"
        tb.upload("/tmp/demo/hello.txt", content)
        print(f"uploaded /tmp/demo/hello.txt ({len(content)} bytes)")

        # list
        entries = tb.list_files("/tmp/demo")
        names = sorted(e.name for e in entries)
        print(f"list /tmp/demo: {names}")

        # download + verify round-trip
        downloaded = tb.download("/tmp/demo/hello.txt")
        assert downloaded == content, f"download mismatch: {downloaded!r}"
        print(f"downloaded {len(downloaded)} bytes, matches upload ✓")

        # chmod
        tb.chmod("/tmp/demo/hello.txt", 0o755)
        print("chmod 0755 ok")

        # rename
        tb.rename("/tmp/demo/hello.txt", "/tmp/demo/goodbye.txt")
        print("rename hello.txt → goodbye.txt ok")
        entries_after = sorted(e.name for e in tb.list_files("/tmp/demo"))
        print(f"list after rename: {entries_after}")

        # delete
        tb.delete_file("/tmp/demo/goodbye.txt")
        print("delete goodbye.txt ok")

        # Final list to confirm cleanup
        final = sorted(e.name for e in tb.list_files("/tmp/demo"))
        print(f"final /tmp/demo: {final}")
    finally:
        sb.delete()

    return 0


if __name__ == "__main__":
    sys.exit(main())
