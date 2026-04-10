#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARTIFACT_DIR="$ROOT_DIR/artifacts/kernels"

PROFILE="standard"
MODE="symlink"
SOURCE=""
CONFIG=""
NAME=""

usage() {
  cat <<'EOF'
Usage:
  tools/prepare-kernel.sh [options]

Options:
  --profile standard|virtiofs   Kernel profile to validate (default: standard)
  --source PATH                 Kernel image to pin into the repo
  --config PATH                 Kernel config to validate against
  --name NAME                   Output artifact name under artifacts/kernels/
  --mode symlink|copy           How to materialize the artifact (default: symlink)
  -h, --help                    Show this help

Examples:
  ./tools/prepare-kernel.sh
  ./tools/prepare-kernel.sh --profile virtiofs
  ./tools/prepare-kernel.sh --source /boot/vmlinuz-$(uname -r) --name my-kernel
EOF
}

fail() {
  echo "error: $*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)
      PROFILE="${2:-}"
      shift 2
      ;;
    --source)
      SOURCE="${2:-}"
      shift 2
      ;;
    --config)
      CONFIG="${2:-}"
      shift 2
      ;;
    --name)
      NAME="${2:-}"
      shift 2
      ;;
    --mode)
      MODE="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

case "$PROFILE" in
  standard|virtiofs) ;;
  *)
    fail "invalid --profile '$PROFILE' (expected standard or virtiofs)"
    ;;
esac

case "$MODE" in
  symlink|copy) ;;
  *)
    fail "invalid --mode '$MODE' (expected symlink or copy)"
    ;;
esac

if [[ -z "$SOURCE" ]]; then
  release="$(uname -r)"
  for candidate in "/boot/vmlinuz-$release" "/boot/vmlinuz" "/boot/bzImage-$release" "/boot/bzImage"; do
    if [[ -f "$candidate" ]]; then
      SOURCE="$candidate"
      break
    fi
  done
fi

[[ -n "$SOURCE" ]] || fail "no kernel image found; pass --source explicitly"
[[ -f "$SOURCE" ]] || fail "kernel image not found: $SOURCE"

if [[ -z "$CONFIG" ]]; then
  if [[ "$SOURCE" =~ /boot/vmlinuz-([^/]+)$ ]]; then
    candidate="/boot/config-${BASH_REMATCH[1]}"
    [[ -f "$candidate" ]] && CONFIG="$candidate"
  elif [[ -f "/boot/config-$(uname -r)" ]]; then
    CONFIG="/boot/config-$(uname -r)"
  fi
fi

require_config_flag() {
  local key="$1"
  local expected="$2"
  [[ -n "$CONFIG" ]] || fail "kernel config is required to validate '$key'; pass --config explicitly"
  [[ -f "$CONFIG" ]] || fail "kernel config not found: $CONFIG"
  local line
  line="$(rg -n "^${key}=${expected}$" "$CONFIG" || true)"
  [[ -n "$line" ]] || fail "kernel config $CONFIG is missing ${key}=${expected}"
}

require_config_flag_one_of() {
  local key="$1"
  shift
  [[ -n "$CONFIG" ]] || fail "kernel config is required to validate '$key'; pass --config explicitly"
  [[ -f "$CONFIG" ]] || fail "kernel config not found: $CONFIG"
  local value
  for value in "$@"; do
    if rg -q "^${key}=${value}$" "$CONFIG"; then
      return 0
    fi
  done
  fail "kernel config $CONFIG is missing ${key} in {$*}"
}

if [[ "$PROFILE" == "virtiofs" ]]; then
  require_config_flag "CONFIG_ACPI" "y"
  require_config_flag "CONFIG_PCI" "y"
  require_config_flag_one_of "CONFIG_FUSE_FS" "y" "m"
  require_config_flag_one_of "CONFIG_VIRTIO_FS" "y" "m"

  if [[ "$SOURCE" =~ /boot/vmlinuz-([^/]+)$ ]]; then
    release="${BASH_REMATCH[1]}"
    module_glob="/lib/modules/$release/kernel/fs/fuse/virtiofs.ko*"
    compgen -G "$module_glob" >/dev/null || fail "virtiofs module not found for release $release under /lib/modules"
  fi
fi

mkdir -p "$ARTIFACT_DIR"

if [[ -z "$NAME" ]]; then
  case "$PROFILE" in
    standard)
      NAME="host-current-vmlinuz"
      ;;
    virtiofs)
      NAME="host-current-virtiofs-vmlinuz"
      ;;
  esac
fi

TARGET="$ARTIFACT_DIR/$NAME"
rm -f "$TARGET"

if [[ "$MODE" == "copy" ]]; then
  cp -Lf "$SOURCE" "$TARGET"
else
  ln -sfn "$SOURCE" "$TARGET"
fi

echo "Prepared kernel artifact:"
echo "  profile: $PROFILE"
echo "  source:  $SOURCE"
[[ -n "$CONFIG" ]] && echo "  config:  $CONFIG"
echo "  target:  $TARGET"

source_base="$(basename "$SOURCE")"
if [[ "$source_base" == vmlinuz* || "$source_base" == bzImage* ]]; then
  echo >&2
  echo "warning: pinned host kernel looks like a distro bzImage/vmlinuz artifact." >&2
  echo "warning: gocracker will normalize it to the embedded ELF vmlinux at runtime when it can read the file." >&2
  echo "warning: prefer ./tools/build-guest-kernel.sh and use ./artifacts/kernels/gocracker-guest-*-vmlinux for the validated cross-host baseline." >&2
fi
