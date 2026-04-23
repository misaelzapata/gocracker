"""Shared helpers for all cookbook examples.

Every example resolves the guest kernel path the same way:

    1. Command-line arg 1 (`python hello_world.py /abs/kernel`)
    2. Environment variable `GOCRACKER_KERNEL`
    3. Repo-relative default `artifacts/kernels/gocracker-guest-standard-vmlinux`
       (works when running from a checked-out repo without installation)
    4. Error with a clear message pointing the user at (1)–(3)

This keeps examples portable across machines: no absolute paths baked
into scripts that only work on the original author's laptop.
"""
from __future__ import annotations

import os
import sys
from pathlib import Path

_REPO_ROOT = Path(__file__).resolve().parents[3].parent  # .../gocracker/
_DEFAULT_KERNEL = _REPO_ROOT / "artifacts" / "kernels" / "gocracker-guest-standard-vmlinux"

# Make the SDK importable without a pip install when running straight
# from the repo. parents[3] is sandboxes/, so sandboxes/sdk/python is
# where the package lives.
_SDK = Path(__file__).resolve().parents[3] / "sdk" / "python"
if str(_SDK) not in sys.path:
    sys.path.insert(0, str(_SDK))


def resolve_kernel(argv_index: int = 1) -> str:
    """Return the guest kernel path to use, or exit(2) with a clear
    message if none is configured.

    Precedence:
      sys.argv[argv_index] → $GOCRACKER_KERNEL → repo-relative default
    """
    if len(sys.argv) > argv_index and sys.argv[argv_index]:
        return sys.argv[argv_index]
    env = os.environ.get("GOCRACKER_KERNEL", "").strip()
    if env:
        return env
    if _DEFAULT_KERNEL.exists():
        return str(_DEFAULT_KERNEL)
    print(
        f"error: no guest kernel configured.\n"
        f"  pass one as arg 1, or set $GOCRACKER_KERNEL,\n"
        f"  or build the default at {_DEFAULT_KERNEL}",
        file=sys.stderr,
    )
    sys.exit(2)


def sandboxd_url() -> str:
    """Return the sandboxd HTTP URL. Honours $GOCRACKER_SANDBOXD or
    defaults to http://127.0.0.1:9091."""
    return os.environ.get("GOCRACKER_SANDBOXD", "http://127.0.0.1:9091")
