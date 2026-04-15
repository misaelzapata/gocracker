#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

exec "${GOCRACKER_SWEEP_BIN:-go}" run ./cmd/gocracker-sweep \
  --manifest tests/external-repos/manifest.tsv \
  --tty-manifest tests/external-repos/historical-tty.tsv
