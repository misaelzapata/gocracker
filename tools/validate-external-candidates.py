#!/usr/bin/env python3

from __future__ import annotations

import argparse
import csv
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path


def head_sha(url: str) -> str:
    out = subprocess.check_output(
        ["git", "ls-remote", url, "HEAD"],
        text=True,
        stderr=subprocess.DEVNULL,
        timeout=20,
    ).strip()
    if not out:
        raise RuntimeError(f"no HEAD ref returned for {url}")
    return out.splitlines()[0].split()[0]


def raw_github_url(repo_url: str, sha: str, path: str) -> str:
    if not repo_url.startswith("https://github.com/"):
        raise ValueError(f"unsupported repo url: {repo_url}")
    base = repo_url.removesuffix(".git").replace(
        "https://github.com/", "https://raw.githubusercontent.com/", 1
    )
    return f"{base}/{sha}/{path}"


def raw_exists(url: str) -> bool:
    req = urllib.request.Request(url, method="HEAD")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return 200 <= resp.status < 400
    except urllib.error.HTTPError:
        return False
    except urllib.error.URLError:
        return False


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Resolve HEAD refs and validate Dockerfile/Compose paths for external sweep candidates."
    )
    p.add_argument("input_tsv", type=Path, help="candidate TSV without ref column resolution")
    p.add_argument(
        "--output",
        type=Path,
        default=None,
        help="write validated rows to this TSV file instead of stdout",
    )
    return p.parse_args()


def main() -> int:
    args = parse_args()
    rows = []
    failures = []

    with args.input_tsv.open(newline="", encoding="utf-8") as fh:
        reader = csv.reader(fh, delimiter="\t")
        for raw in reader:
            if not raw:
                continue
            if raw[0].startswith("#"):
                continue
            if len(raw) != 12:
                failures.append((raw[0] if raw else "?", "manifest", f"expected 12 fields, got {len(raw)}"))
                continue
            rows.append(raw)

    validated = []
    for raw in rows:
        (
            ident,
            kind,
            url,
            path,
            stack,
            mode,
            probe_type,
            probe_target,
            probe_expect,
            mem_mb,
            disk_mb,
            notes,
        ) = raw
        try:
            sha = head_sha(url)
        except Exception as exc:  # noqa: BLE001
            failures.append((ident, "head", str(exc)))
            continue
        try:
            raw_url = raw_github_url(url, sha, path)
        except Exception as exc:  # noqa: BLE001
            failures.append((ident, "url", str(exc)))
            continue
        if not raw_exists(raw_url):
            failures.append((ident, "path", f"missing {path} at {sha[:12]}"))
            continue
        validated.append(
            [
                ident,
                kind,
                url,
                sha,
                path,
                stack,
                mode,
                probe_type,
                probe_target,
                probe_expect,
                mem_mb,
                disk_mb,
                notes,
            ]
        )

    out = sys.stdout
    close_out = False
    if args.output is not None:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        out = args.output.open("w", newline="", encoding="utf-8")
        close_out = True

    try:
        writer = csv.writer(out, delimiter="\t", lineterminator="\n")
        writer.writerow(
            [
                "# id",
                "kind",
                "url",
                "ref",
                "path",
                "stack",
                "mode",
                "probe_type",
                "probe_target",
                "probe_expect",
                "mem_mb",
                "disk_mb",
                "notes",
            ]
        )
        writer.writerows(validated)
    finally:
        if close_out:
            out.close()

    for ident, stage, detail in failures:
        print(f"FAIL\t{ident}\t{stage}\t{detail}", file=sys.stderr)

    print(
        f"validated {len(validated)} candidate(s), failed {len(failures)}",
        file=sys.stderr,
    )
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
