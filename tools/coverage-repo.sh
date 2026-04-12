#!/usr/bin/env bash
set -euo pipefail

threshold=""
warn_only=0
summary_file=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --threshold)
      threshold="${2:-}"
      shift 2
      ;;
    --warn-only)
      warn_only=1
      shift
      ;;
    --summary-file)
      summary_file="${2:-}"
      shift 2
      ;;
    *)
      echo "usage: $0 [--threshold <percent>] [--warn-only] [--summary-file <path>]" >&2
      exit 2
      ;;
  esac
done

tmpdir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT

covered_total=0
statement_total=0

while IFS= read -r pkg; do
  [[ -z "$pkg" ]] && continue
  profile="$tmpdir/$(echo "$pkg" | tr '/.' '__').out"
  echo "==> $pkg"
  go test "$pkg" -coverprofile="$profile" -covermode=set
  read -r covered statements < <(awk 'NR > 1 { total += $2; if ($3 > 0) covered += $2 } END { print covered + 0, total + 0 }' "$profile")
  covered_total=$((covered_total + covered))
  statement_total=$((statement_total + statements))
done < <(go list ./... | grep -v '/tests/integration')

if [[ "$statement_total" -eq 0 ]]; then
  percent="100.00"
else
  percent="$(awk -v covered="$covered_total" -v total="$statement_total" 'BEGIN { printf "%.2f", (covered * 100) / total }')"
fi

summary="Repo coverage: ${percent}% (${covered_total}/${statement_total} statements)"
echo "$summary"

if [[ -n "$summary_file" ]]; then
  printf '%s\n' "$summary" >"$summary_file"
fi

if [[ -n "$threshold" ]]; then
  if awk -v got="$percent" -v want="$threshold" 'BEGIN { exit !(got + 0 < want + 0) }'; then
    message="$summary is below threshold ${threshold}%"
    if [[ "$warn_only" -eq 1 ]]; then
      echo "warning: $message" >&2
    else
      echo "error: $message" >&2
      exit 1
    fi
  fi
fi
