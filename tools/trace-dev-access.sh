#!/usr/bin/env bash

set -euo pipefail

if [[ $# -eq 0 ]]; then
  echo "usage: $0 -- <command> [args...]" >&2
  exit 2
fi

if [[ "$1" == "--" ]]; then
  shift
fi

command -v strace >/dev/null 2>&1 || {
  echo "error: strace is required" >&2
  exit 1
}

exec strace -ff -tt -s 256 \
  -e trace=%file,mknod,mknodat,mount,umount2,rename,renameat,renameat2,unlink,unlinkat,truncate,ftruncate \
  -P /dev \
  "$@"
