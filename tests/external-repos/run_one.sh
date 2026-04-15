#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

RUNNER=("${GOCRACKER_SWEEP_BIN:-go}" "run" "./cmd/gocracker-sweep")

if [[ "${1:-}" == "--list" ]]; then
  exec "${RUNNER[@]}" --manifest tests/external-repos/manifest.tsv --list
fi

if [[ $# -lt 1 ]]; then
  echo "usage: tests/external-repos/run_one.sh <repo-id>" >&2
  exit 1
fi

exec "${RUNNER[@]}" \
  --manifest tests/external-repos/manifest.tsv \
  --id "$1"
