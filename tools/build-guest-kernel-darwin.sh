#!/usr/bin/env bash
#
# build-guest-kernel-darwin.sh — Download or cross-compile a Linux guest kernel
# compatible with Apple Virtualization.framework.
#
# The kernel must include at minimum:
#   CONFIG_VIRTIO_PCI=y
#   CONFIG_VIRTIO_BLK=y
#   CONFIG_VIRTIO_NET=y
#   CONFIG_VIRTIO_CONSOLE=y
#   CONFIG_VIRTIO_FS=y
#   CONFIG_VIRTIO_VSOCK=y
#   CONFIG_VIRTIO_RNG=y (hardware_random)
#   CONFIG_PCI=y
#   CONFIG_PCI_HOST_GENERIC=y
#   CONFIG_SERIAL_8250=y
#   CONFIG_SERIAL_8250_CONSOLE=y
#   CONFIG_EXT4_FS=y
#
# On Apple Silicon (arm64), the guest kernel must be ARM64.
# On Intel Mac (x86_64), the guest kernel must be x86_64.
#
# Usage:
#   ./tools/build-guest-kernel-darwin.sh
#
# The output kernel is placed in:
#   ./artifacts/kernels/gocracker-guest-darwin-vmlinuz
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OUTPUT_DIR="$PROJECT_ROOT/artifacts/kernels"
KERNEL_VERSION="${KERNEL_VERSION:-6.12.1}"

HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  arm64|aarch64) LINUX_ARCH="arm64"; CROSS_COMPILE="" ;;
  x86_64)        LINUX_ARCH="x86_64"; CROSS_COMPILE="" ;;
  *)             echo "Unsupported host architecture: $HOST_ARCH"; exit 1 ;;
esac

mkdir -p "$OUTPUT_DIR"

OUTPUT_KERNEL="$OUTPUT_DIR/gocracker-guest-darwin-vmlinuz"

echo "=== gocracker guest kernel builder (macOS) ==="
echo "Host arch:      $HOST_ARCH"
echo "Linux arch:     $LINUX_ARCH"
echo "Kernel version: $KERNEL_VERSION"
echo "Output:         $OUTPUT_KERNEL"
echo ""

# Check if a pre-built kernel already exists
if [ -f "$OUTPUT_KERNEL" ]; then
  echo "Kernel already exists at $OUTPUT_KERNEL"
  echo "Delete it to rebuild."
  exit 0
fi

echo "To build a guest kernel on macOS, you need a Linux cross-compilation toolchain."
echo ""
echo "Option 1: Use a pre-built kernel"
echo "  Download a pre-built vmlinuz from:"
echo "    https://github.com/Code-Hex/vz/releases (example kernels)"
echo "    https://github.com/lima-vm/alpine-lima/releases (Alpine kernels)"
echo "  Place it at: $OUTPUT_KERNEL"
echo ""
echo "Option 2: Cross-compile (requires Docker or a Linux VM)"
echo "  docker run --rm -v $PROJECT_ROOT:/work -w /work debian:bookworm bash -c '"
echo "    apt-get update && apt-get install -y build-essential flex bison libelf-dev bc && "
echo "    ./tools/build-guest-kernel.sh"
echo "  '"
echo "  cp artifacts/kernels/gocracker-guest-standard-vmlinux $OUTPUT_KERNEL"
echo ""
echo "Required kernel configs for Virtualization.framework:"
echo "  CONFIG_PCI=y CONFIG_PCI_HOST_GENERIC=y"
echo "  CONFIG_VIRTIO_PCI=y CONFIG_VIRTIO_BLK=y CONFIG_VIRTIO_NET=y"
echo "  CONFIG_VIRTIO_CONSOLE=y CONFIG_VIRTIO_FS=y CONFIG_VIRTIO_VSOCK=y"
echo "  CONFIG_SERIAL_8250=y CONFIG_SERIAL_8250_CONSOLE=y CONFIG_EXT4_FS=y"

exit 1
