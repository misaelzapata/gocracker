#!/usr/bin/env python3

from __future__ import annotations

import csv
import sys
from collections import Counter
from datetime import datetime
from pathlib import Path


START_MARKER = "<!-- EXTERNAL_SWEEP_REPORT:START -->"
END_MARKER = "<!-- EXTERNAL_SWEEP_REPORT:END -->"


def latest_results_path(base: Path) -> Path:
    candidates = [p for p in base.iterdir() if p.is_dir() and p.name != "cache"]
    if not candidates:
        raise SystemExit(f"no external sweep directories found under {base}")
    latest = max(candidates, key=lambda p: p.stat().st_mtime)
    results = latest / "results.tsv"
    if not results.is_file():
        raise SystemExit(f"results.tsv not found in {latest}")
    return results


def load_rows(results_path: Path) -> list[dict[str, str]]:
    with results_path.open("r", encoding="utf-8", newline="") as f:
        reader = csv.DictReader(f, delimiter="\t")
        return list(reader)


def make_report(results_path: Path, rows: list[dict[str, str]]) -> str:
    counts = Counter(row["status"] for row in rows)
    failure_classes = Counter(row.get("failure_class", "") for row in rows if row["status"] == "FAIL")
    run_dir = results_path.parent
    ts = datetime.fromtimestamp(run_dir.stat().st_mtime).strftime("%Y-%m-%d %H:%M:%S")
    manifest_path = Path(__file__).resolve().parents[1] / "tests" / "external-repos" / "manifest.tsv"
    manifest_total = sum(
        1
        for line in manifest_path.read_text(encoding="utf-8").splitlines()
        if line.strip() and not line.lstrip().startswith("#")
    )

    lines: list[str] = []
    lines.append("## External Repo Sweep")
    lines.append("")
    lines.append(f"Latest recorded sweep snapshot: `{run_dir}`")
    lines.append("")
    lines.append(f"- Updated from `results.tsv` on {ts}")
    lines.append(f"- Tested so far: `{len(rows)}/{manifest_total}`")
    lines.append(f"- `PASS`: `{counts.get('PASS', 0)}`")
    lines.append(f"- `FAIL`: `{counts.get('FAIL', 0)}`")
    if failure_classes:
        top_failures = ", ".join(
            f"`{name}`={count}" for name, count in failure_classes.most_common(5) if name
        )
        if top_failures:
            lines.append(f"- Top failure classes: {top_failures}")
    lines.append("")
    lines.append("Current tested repos:")
    lines.append("")
    lines.append("| ID | Kind | Stack | Status | Ref | Failure | Notes |")
    lines.append("|----|------|-------|--------|-----|---------|-------|")
    for row in rows:
        failure = row.get("failure_class", "")
        lines.append(
            f"| `{row['id']}` | `{row['kind']}` | `{row['stack']}` | `{row['status']}` | `{row['resolved_ref'][:12]}` | `{failure}` | {row['notes']} |"
        )
    lines.append("")
    lines.append(
        "This section is generated from `tests/external-repos` output. Re-run "
        "`./tools/update-external-sweep-report.py` after the sweep advances or finishes."
    )
    return "\n".join(lines)


def replace_section(readme_path: Path, report: str) -> None:
    content = readme_path.read_text(encoding="utf-8")
    if START_MARKER not in content or END_MARKER not in content:
        raise SystemExit(f"missing report markers in {readme_path}")
    start = content.index(START_MARKER) + len(START_MARKER)
    end = content.index(END_MARKER)
    updated = content[:start] + "\n\n" + report + "\n\n" + content[end:]
    readme_path.write_text(updated, encoding="utf-8")


def main(argv: list[str]) -> int:
    repo_root = Path(__file__).resolve().parents[1]
    results_path = Path(argv[1]).resolve() if len(argv) > 1 else latest_results_path(Path("/tmp/gocracker-external-repos"))
    readme_path = repo_root / "README.md"
    rows = load_rows(results_path)
    report = make_report(results_path, rows)
    replace_section(readme_path, report)
    print(f"updated {readme_path} from {results_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
