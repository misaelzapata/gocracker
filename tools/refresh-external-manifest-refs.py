#!/usr/bin/env python3

from __future__ import annotations

import argparse
import csv
import subprocess
import sys
from pathlib import Path


def head_sha(url: str) -> str:
    out = subprocess.check_output(
        ["git", "ls-remote", url, "HEAD"],
        text=True,
        stderr=subprocess.DEVNULL,
        timeout=30,
    ).strip()
    if not out:
        raise RuntimeError(f"no HEAD ref returned for {url}")
    return out.splitlines()[0].split()[0]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Refresh the pinned ref column in external-repo manifest rows using remote HEAD."
    )
    parser.add_argument("manifest", type=Path, help="manifest TSV to refresh")
    parser.add_argument(
        "--output",
        type=Path,
        default=None,
        help="write refreshed rows to this path instead of stdout",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    rows: list[list[str]] = []
    failures: list[tuple[str, str]] = []

    with args.manifest.open(newline="", encoding="utf-8") as fh:
      reader = csv.reader(fh, delimiter="\t")
      for raw in reader:
        if not raw:
          continue
        if raw[0].startswith("#"):
          rows.append(raw)
          continue
        if len(raw) != 13:
          failures.append((raw[0] if raw else "?", f"expected 13 fields, got {len(raw)}"))
          continue
        ident = raw[0]
        url = raw[2]
        try:
          raw[3] = head_sha(url)
        except Exception as exc:  # noqa: BLE001
          failures.append((ident, str(exc)))
          continue
        rows.append(raw)

    out = sys.stdout
    close_out = False
    if args.output is not None:
      args.output.parent.mkdir(parents=True, exist_ok=True)
      out = args.output.open("w", newline="", encoding="utf-8")
      close_out = True

    try:
      writer = csv.writer(out, delimiter="\t", lineterminator="\n")
      writer.writerows(rows)
    finally:
      if close_out:
        out.close()

    for ident, detail in failures:
      print(f"FAIL\t{ident}\t{detail}", file=sys.stderr)

    print(f"refreshed {len(rows)} row(s), failed {len(failures)}", file=sys.stderr)
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
