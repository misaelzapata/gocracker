#!/usr/bin/env python3
"""Cookbook 8/10: toolbox /secrets store — set + list + delete.

The toolbox agent carries a per-sandbox credential store that
persists only within the VM's lifetime. Useful for API keys that
the user process needs without them leaking into command history
or env variables at launch time.

Usage:
  sudo python3 secrets.py [KERNEL_PATH]
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

        # Initial list should be empty.
        initial = tb.list_secrets()
        print(f"initial secrets: {initial}")

        # Set two secrets.
        tb.set_secret("OPENAI_API_KEY", "sk-test-not-a-real-key")
        tb.set_secret("STRIPE_SECRET", "sk_test_123")
        names = sorted(tb.list_secrets())
        print(f"after set: {names}")
        assert "OPENAI_API_KEY" in names and "STRIPE_SECRET" in names

        # Delete one.
        tb.delete_secret("STRIPE_SECRET")
        after_delete = sorted(tb.list_secrets())
        print(f"after delete STRIPE_SECRET: {after_delete}")
        assert "STRIPE_SECRET" not in after_delete
        assert "OPENAI_API_KEY" in after_delete

        print("\nNote: secrets are scoped to this sandbox and discarded when")
        print("the VM is torn down — they never write to disk.")
    finally:
        sb.delete()

    return 0


if __name__ == "__main__":
    sys.exit(main())
