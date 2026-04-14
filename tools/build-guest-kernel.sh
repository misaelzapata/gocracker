#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS_DIR="$ROOT_DIR/tools/kernel"
ARTIFACT_KERNELS_DIR="$ROOT_DIR/artifacts/kernels"
ARTIFACT_BUILD_DIR="$ROOT_DIR/artifacts/kernel-build"
ARTIFACT_SOURCE_DIR="$ROOT_DIR/artifacts/linux-src"
ARTIFACT_ARCHIVE_DIR="$ROOT_DIR/artifacts/linux-archives"
FIRECRACKER_BASE_CONFIG="$TOOLS_DIR/firecracker-guest-x86_64-6.1.config"
FIRECRACKER_KERNEL_VERSION="6.1.102"

PROFILE="standard"
SOURCE_DIR=""
BASE_CONFIG=""
JOBS="$(nproc)"
KERNEL_NAME=""
BUILD_DIR=""
STAGED_SOURCE_DIR=""
EMIT_BZIMAGE=0

usage() {
  cat <<'EOF'
Usage:
  tools/build-guest-kernel.sh [options]

Options:
  --profile standard|virtiofs|minimal
                               Guest kernel profile (default: standard)
  --source-dir PATH             Linux source tree to build from
  --base-config PATH            Base .config to merge from (default: Firecracker 6.1 guest config)
  --build-dir PATH              Out-of-tree build directory
  --name NAME                   Artifact basename under artifacts/kernels/
  --emit-bzimage                Also materialize the matching bzImage artifact
  -j, --jobs N                  Parallel jobs (default: nproc)
  -h, --help                    Show help

Examples:
  ./tools/build-guest-kernel.sh
  ./tools/build-guest-kernel.sh --profile standard
  ./tools/build-guest-kernel.sh --source-dir /path/to/linux --base-config /path/to/.config
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
    --source-dir)
      SOURCE_DIR="${2:-}"
      shift 2
      ;;
    --base-config)
      BASE_CONFIG="${2:-}"
      shift 2
      ;;
    --build-dir)
      BUILD_DIR="${2:-}"
      shift 2
      ;;
    --name)
      KERNEL_NAME="${2:-}"
      shift 2
      ;;
    --emit-bzimage)
      EMIT_BZIMAGE=1
      shift
      ;;
    -j|--jobs)
      JOBS="${2:-}"
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
  standard|virtiofs|minimal) ;;
  *)
    fail "invalid --profile '$PROFILE' (expected standard, virtiofs, or minimal)"
    ;;
esac

detect_source_dir() {
  local release base series
  release="$(uname -r)"
  base="${release%-generic}"
  series="$(printf '%s' "$base" | awk -F'[.-]' '{print $1 "." $2}')"
  for candidate in \
    "/usr/src/linux-hwe-${series}-headers-${base}" \
    "/usr/src/linux-hwe-headers-${base}" \
    "/usr/src/linux-hwe-${base}" \
    "/usr/src/linux-headers-${release}" \
    "/usr/src/linux-${release}" \
    "/usr/src/linux"; do
    if [[ -d "$candidate" && -f "$candidate/Makefile" && -d "$candidate/arch/x86" ]]; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

source_tree_complete() {
  local dir="$1"
  [[ -f "$dir/scripts/Makefile.extrawarn" ]] &&
    [[ -f "$dir/arch/x86/tools/cpufeaturemasks.awk" ]] &&
    [[ -f "$dir/arch/x86/entry/syscalls/syscall_32.tbl" ]]
}

detect_kernel_version() {
  local dir="$1" cfg="$2"
  if [[ -n "$dir" && -f "$dir/Makefile" ]]; then
    if make -s -C "$dir" kernelversion >/dev/null 2>&1; then
      make -s -C "$dir" kernelversion
      return 0
    fi
  fi
  if [[ -n "$cfg" && -f "$cfg" ]]; then
    local from_header
    from_header="$(grep -m1 '^# Linux/x86_64 ' "$cfg" | sed -E 's/^# Linux\/x86_64 ([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
    if [[ -n "$from_header" ]]; then
      printf '%s' "$from_header"
      return 0
    fi
  fi
  printf '%s' "$(uname -r)" | sed -E 's/^([0-9]+\.[0-9]+\.[0-9]+).*/\1/'
}

download_full_source() {
  local version="$1"
  local archive="$ARTIFACT_ARCHIVE_DIR/linux-${version}.tar.xz"
  local extracted="$ARTIFACT_SOURCE_DIR/linux-${version}"
  local url="https://cdn.kernel.org/pub/linux/kernel/v${version%%.*}.x/linux-${version}.tar.xz"
  if [[ ! -f "$archive" ]]; then
    curl -L --fail --output "$archive" "$url"
  fi
  if [[ ! -d "$extracted" ]]; then
    tar -C "$ARTIFACT_SOURCE_DIR" -xf "$archive"
  fi
  [[ -d "$extracted" ]] || fail "downloaded source tree not found: $extracted"
  echo "$extracted"
}

mkdir -p "$ARTIFACT_KERNELS_DIR" "$ARTIFACT_BUILD_DIR" "$ARTIFACT_SOURCE_DIR" "$ARTIFACT_ARCHIVE_DIR"

if [[ -z "$BASE_CONFIG" ]]; then
  if [[ -f "$FIRECRACKER_BASE_CONFIG" ]]; then
    BASE_CONFIG="$FIRECRACKER_BASE_CONFIG"
  elif [[ -f "/boot/config-$(uname -r)" ]]; then
    BASE_CONFIG="/boot/config-$(uname -r)"
  fi
fi
[[ -n "$BASE_CONFIG" ]] || fail "no base config found; pass --base-config explicitly"
[[ -f "$BASE_CONFIG" ]] || fail "base config not found: $BASE_CONFIG"

if [[ -z "$SOURCE_DIR" ]]; then
  if [[ "$BASE_CONFIG" == "$FIRECRACKER_BASE_CONFIG" ]]; then
    SOURCE_DIR="$(download_full_source "$FIRECRACKER_KERNEL_VERSION")"
  else
    SOURCE_DIR="$(detect_source_dir)" || fail "could not find a local Linux source tree; pass --source-dir explicitly"
  fi
fi
[[ -d "$SOURCE_DIR" ]] || fail "source dir not found: $SOURCE_DIR"
[[ -f "$SOURCE_DIR/Makefile" ]] || fail "missing Makefile in source dir: $SOURCE_DIR"
if ! source_tree_complete "$SOURCE_DIR"; then
  SOURCE_DIR="$(download_full_source "$(detect_kernel_version "$SOURCE_DIR" "$BASE_CONFIG")")"
fi
[[ -x "$SOURCE_DIR/scripts/config" ]] || fail "missing scripts/config in source dir: $SOURCE_DIR"
[[ -x "$SOURCE_DIR/scripts/kconfig/merge_config.sh" ]] || fail "missing merge_config.sh in source dir: $SOURCE_DIR"

if [[ -z "$BUILD_DIR" ]]; then
  BUILD_DIR="$ARTIFACT_BUILD_DIR/$PROFILE"
fi
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"

if [[ -z "$KERNEL_NAME" ]]; then
  KERNEL_NAME="gocracker-guest-${PROFILE}"
fi

BZIMAGE_DST="$ARTIFACT_KERNELS_DIR/${KERNEL_NAME}-bzImage"
VMLINUX_DST="$ARTIFACT_KERNELS_DIR/${KERNEL_NAME}-vmlinux"
CONFIG_DST="$ARTIFACT_KERNELS_DIR/${KERNEL_NAME}.config"

COMMON_FRAGMENT="$TOOLS_DIR/guest-common-x86_64.fragment"
PROFILE_FRAGMENT=""
[[ -f "$COMMON_FRAGMENT" ]] || fail "missing common fragment: $COMMON_FRAGMENT"
case "$PROFILE" in
  virtiofs)
    PROFILE_FRAGMENT="$TOOLS_DIR/guest-virtiofs.fragment"
    ;;
  minimal)
    PROFILE_FRAGMENT="$TOOLS_DIR/guest-minimal-x86_64.fragment"
    ;;
esac
if [[ -n "$PROFILE_FRAGMENT" ]]; then
  [[ -f "$PROFILE_FRAGMENT" ]] || fail "missing profile fragment: $PROFILE_FRAGMENT"
fi

STAGED_SOURCE_DIR="$ARTIFACT_SOURCE_DIR/$(basename "$SOURCE_DIR")"
if [[ "$SOURCE_DIR" == "$ARTIFACT_SOURCE_DIR/"* ]]; then
  STAGED_SOURCE_DIR="$SOURCE_DIR"
else
  rm -rf "$STAGED_SOURCE_DIR"
  mkdir -p "$STAGED_SOURCE_DIR"
  rsync -a --delete --exclude '.git' --exclude '.cache.mk' "$SOURCE_DIR"/ "$STAGED_SOURCE_DIR"/
fi
make -C "$STAGED_SOURCE_DIR" mrproper

cp "$BASE_CONFIG" "$BUILD_DIR/.config"
if [[ -n "$PROFILE_FRAGMENT" ]]; then
  "$STAGED_SOURCE_DIR/scripts/kconfig/merge_config.sh" -m -O "$BUILD_DIR" "$BUILD_DIR/.config" "$COMMON_FRAGMENT" "$PROFILE_FRAGMENT"
else
  "$STAGED_SOURCE_DIR/scripts/kconfig/merge_config.sh" -m -O "$BUILD_DIR" "$BUILD_DIR/.config" "$COMMON_FRAGMENT"
fi
make -C "$STAGED_SOURCE_DIR" O="$BUILD_DIR" olddefconfig
make -C "$STAGED_SOURCE_DIR" O="$BUILD_DIR" -j "$JOBS" bzImage

BZIMAGE_SRC="$BUILD_DIR/arch/x86/boot/bzImage"
VMLINUX_SRC="$BUILD_DIR/vmlinux"
CONFIG_SRC="$BUILD_DIR/.config"

[[ -f "$BZIMAGE_SRC" ]] || fail "bzImage not produced at $BZIMAGE_SRC"
[[ -f "$VMLINUX_SRC" ]] || fail "vmlinux not produced at $VMLINUX_SRC"

cp -f "$VMLINUX_SRC" "$VMLINUX_DST"
cp -f "$CONFIG_SRC" "$CONFIG_DST"

if [[ "$EMIT_BZIMAGE" == "1" ]]; then
  cp -f "$BZIMAGE_SRC" "$BZIMAGE_DST"
else
  rm -f "$BZIMAGE_DST"
fi

echo "Built guest kernel:"
echo "  profile:    $PROFILE"
echo "  source dir: $SOURCE_DIR"
echo "  staged dir: $STAGED_SOURCE_DIR"
echo "  base config:$BASE_CONFIG"
echo "  vmlinux:    $VMLINUX_DST"
echo "  config:     $CONFIG_DST"
if [[ "$EMIT_BZIMAGE" == "1" ]]; then
  echo "  bzImage:    $BZIMAGE_DST"
else
  echo "  bzImage:    not emitted (pass --emit-bzimage to materialize it)"
fi
