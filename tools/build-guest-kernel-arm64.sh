#!/usr/bin/env bash
# Build an ARM64 guest kernel for gocracker microVMs.
#
# Must be run on an ARM64 host (or with a cross-compiler).
# Uses the host's Ubuntu AWS kernel config as a base, then merges the
# gocracker ARM64 fragment to ensure virtio, vsock, etc. are built-in.
#
# Usage:
#   tools/build-guest-kernel-arm64.sh                         # use host config
#   tools/build-guest-kernel-arm64.sh --base-config /path     # custom base
#   tools/build-guest-kernel-arm64.sh --source-dir /path      # custom source
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS_DIR="$ROOT_DIR/tools/kernel"
ARTIFACT_KERNELS_DIR="$ROOT_DIR/artifacts/kernels"
ARTIFACT_BUILD_DIR="$ROOT_DIR/artifacts/kernel-build"
ARTIFACT_SOURCE_DIR="$ROOT_DIR/artifacts/linux-src"
ARTIFACT_ARCHIVE_DIR="$ROOT_DIR/artifacts/linux-archives"

PROFILE="standard"
SOURCE_DIR=""
BASE_CONFIG=""
JOBS="$(nproc)"
KERNEL_NAME=""
BUILD_DIR=""
CROSS_COMPILE=""

usage() {
  cat <<'EOF'
Usage:
  tools/build-guest-kernel-arm64.sh [options]

Options:
  --profile standard|minimal
                          Guest kernel profile (default: standard)
                          standard: full Ubuntu AWS arm64 base + common fragment
                          minimal:  saved minimal arm64 config + minimal fragment
                                    (~5 MB Image vs ~55 MB standard)
  --source-dir PATH       Linux source tree to build from
  --base-config PATH      Base .config override (default: profile-specific)
  --build-dir PATH        Out-of-tree build directory
  --name NAME             Artifact basename (default: gocracker-guest-<profile>-arm64)
  --cross-compile PREFIX  Cross-compiler prefix (e.g. aarch64-linux-gnu-)
  -j, --jobs N            Parallel jobs (default: nproc)
  -h, --help              Show help

Examples:
  # Standard build on ARM64 host (a1.metal, Graviton):
  ./tools/build-guest-kernel-arm64.sh

  # Minimal build (smaller Image, faster cold boot):
  ./tools/build-guest-kernel-arm64.sh --profile minimal

  # Cross-compile from x86:
  ./tools/build-guest-kernel-arm64.sh --cross-compile aarch64-linux-gnu-
EOF
}

fail() {
  echo "error: $*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)      PROFILE="${2:-}"; shift 2 ;;
    --source-dir)   SOURCE_DIR="${2:-}"; shift 2 ;;
    --base-config)  BASE_CONFIG="${2:-}"; shift 2 ;;
    --build-dir)    BUILD_DIR="${2:-}"; shift 2 ;;
    --name)         KERNEL_NAME="${2:-}"; shift 2 ;;
    --cross-compile) CROSS_COMPILE="${2:-}"; shift 2 ;;
    -j|--jobs)      JOBS="${2:-}"; shift 2 ;;
    -h|--help)      usage; exit 0 ;;
    *)              fail "unknown argument: $1" ;;
  esac
done

case "$PROFILE" in
  standard)
    ARM64_FRAGMENT="$TOOLS_DIR/guest-common-arm64.fragment"
    ;;
  minimal)
    ARM64_FRAGMENT="$TOOLS_DIR/guest-minimal-arm64.fragment"
    # For minimal we default the base to the saved minimal config so the
    # trim survives across builds. User can still override with --base-config.
    if [[ -z "$BASE_CONFIG" ]]; then
      BASE_CONFIG="$ARTIFACT_KERNELS_DIR/gocracker-guest-minimal-arm64.config"
    fi
    ;;
  *)
    fail "invalid --profile '$PROFILE' (expected standard or minimal)"
    ;;
esac

if [[ -z "$KERNEL_NAME" ]]; then
  KERNEL_NAME="gocracker-guest-${PROFILE}-arm64"
fi

[[ -f "$ARM64_FRAGMENT" ]] || fail "missing ARM64 fragment: $ARM64_FRAGMENT"

# Detect architecture
HOST_ARCH="$(uname -m)"
if [[ "$HOST_ARCH" == "aarch64" ]]; then
  ARCH="arm64"
elif [[ -n "$CROSS_COMPILE" ]]; then
  ARCH="arm64"
else
  fail "not on ARM64 host and no --cross-compile given"
fi

# Find base config
if [[ -z "$BASE_CONFIG" ]]; then
  if [[ -f "/boot/config-$(uname -r)" ]]; then
    BASE_CONFIG="/boot/config-$(uname -r)"
  else
    fail "no base config found; pass --base-config explicitly"
  fi
fi
[[ -f "$BASE_CONFIG" ]] || fail "base config not found: $BASE_CONFIG"

# Find or download source
detect_source_dir() {
  local release base
  release="$(uname -r)"
  base="${release%-aws}"
  for candidate in \
    "/usr/src/linux-aws-${release%%.*}-headers-${release%-generic}" \
    "/usr/src/linux-headers-${release}" \
    "/usr/src/linux-${release}"; do
    if [[ -d "$candidate" && -f "$candidate/Makefile" ]]; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

source_tree_complete() {
  local dir="$1"
  # Headers-only packages have arch/arm64/Makefile but lack actual C source.
  # Require kernel/fork.c as proof we have the full tree.
  [[ -f "$dir/scripts/Makefile.extrawarn" ]] &&
    [[ -f "$dir/arch/arm64/Makefile" ]] &&
    [[ -f "$dir/kernel/fork.c" ]]
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
    from_header="$(grep -m1 '^# Linux/' "$cfg" | sed -E 's/^# Linux\/[^ ]+ ([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
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
    echo "Downloading linux-${version} source..." >&2
    mkdir -p "$ARTIFACT_ARCHIVE_DIR"
    curl -L --fail --output "$archive" "$url" >&2
  fi
  if [[ ! -d "$extracted" ]]; then
    echo "Extracting linux-${version}..." >&2
    mkdir -p "$ARTIFACT_SOURCE_DIR"
    tar -C "$ARTIFACT_SOURCE_DIR" -xf "$archive" >&2
  fi
  [[ -d "$extracted" ]] || fail "downloaded source tree not found: $extracted"
  echo "$extracted"
}

mkdir -p "$ARTIFACT_KERNELS_DIR" "$ARTIFACT_BUILD_DIR" "$ARTIFACT_SOURCE_DIR" "$ARTIFACT_ARCHIVE_DIR"

if [[ -z "$SOURCE_DIR" ]]; then
  SOURCE_DIR="$(detect_source_dir 2>/dev/null)" || true
  if [[ -z "$SOURCE_DIR" ]] || ! source_tree_complete "$SOURCE_DIR"; then
    KVER="$(detect_kernel_version "${SOURCE_DIR:-}" "$BASE_CONFIG")"
    echo "Source tree incomplete, downloading linux-${KVER}..."
    SOURCE_DIR="$(download_full_source "$KVER")"
  fi
fi
[[ -d "$SOURCE_DIR" ]] || fail "source dir not found: $SOURCE_DIR"
[[ -f "$SOURCE_DIR/Makefile" ]] || fail "missing Makefile in source dir: $SOURCE_DIR"

if [[ -z "$BUILD_DIR" ]]; then
  BUILD_DIR="$ARTIFACT_BUILD_DIR/arm64-${PROFILE}"
fi
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"

IMAGE_DST="$ARTIFACT_KERNELS_DIR/${KERNEL_NAME}-Image"
CONFIG_DST="$ARTIFACT_KERNELS_DIR/${KERNEL_NAME}.config"

echo "=== Building ARM64 guest kernel ==="
echo "  source:      $SOURCE_DIR"
echo "  base config: $BASE_CONFIG"
echo "  fragment:    $ARM64_FRAGMENT"
echo "  build dir:   $BUILD_DIR"
echo "  jobs:        $JOBS"

# Start with base config
cp "$BASE_CONFIG" "$BUILD_DIR/.config"

# Merge our fragment using -m so merge_config.sh stays in olddefconfig mode
# (works with headers-only source trees).  After the merge we patch any option
# that the base had as =m but we require as =y by overwriting it directly;
# make olddefconfig then validates the result without needing alldefconfig.
if [[ -x "$SOURCE_DIR/scripts/kconfig/merge_config.sh" ]]; then
  ARCH=$ARCH "$SOURCE_DIR/scripts/kconfig/merge_config.sh" -m -O "$BUILD_DIR" "$BUILD_DIR/.config" "$ARM64_FRAGMENT"
else
  cat "$ARM64_FRAGMENT" >> "$BUILD_DIR/.config"
fi

# Force built-in for options that the base may have as =m.
# This handles base configs (Ubuntu/Debian AWS kernels) that ship many
# drivers as modules even when our fragment says =y.
for opt in \
  CONFIG_VSOCKETS \
  CONFIG_VIRTIO_VSOCKETS \
  CONFIG_VIRTIO_VSOCKETS_COMMON \
  CONFIG_OVERLAY_FS \
  CONFIG_HW_RANDOM_VIRTIO \
  CONFIG_RTC_DRV_PL031 \
; do
  sed -i "s|^${opt}=m|${opt}=y|; s|^# ${opt} is not set|${opt}=y|" "$BUILD_DIR/.config"
done

# Build
MAKE_ARGS=(
  -C "$SOURCE_DIR"
  O="$BUILD_DIR"
  ARCH=$ARCH
  -j "$JOBS"
)
if [[ -n "$CROSS_COMPILE" ]]; then
  MAKE_ARGS+=(CROSS_COMPILE="$CROSS_COMPILE")
fi

make "${MAKE_ARGS[@]}" olddefconfig
make "${MAKE_ARGS[@]}" Image

IMAGE_SRC="$BUILD_DIR/arch/arm64/boot/Image"
CONFIG_SRC="$BUILD_DIR/.config"

[[ -f "$IMAGE_SRC" ]] || fail "Image not produced at $IMAGE_SRC"

cp -f "$IMAGE_SRC" "$IMAGE_DST"
cp -f "$CONFIG_SRC" "$CONFIG_DST"

echo ""
echo "=== ARM64 guest kernel built ==="
echo "  Image:  $IMAGE_DST  ($(du -h "$IMAGE_DST" | cut -f1))"
echo "  config: $CONFIG_DST"
echo ""
echo "Verify vsock is built-in:"
grep -E 'CONFIG_VSOCKETS|CONFIG_VIRTIO_VSOCK' "$CONFIG_DST"
echo ""
echo "Usage:"
echo "  sudo gocracker run --image alpine:3.20 --kernel $IMAGE_DST --wait --tty force"
