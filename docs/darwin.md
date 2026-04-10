# macOS (Darwin) Support

gocracker runs on macOS using Apple's [Virtualization.framework](https://developer.apple.com/documentation/virtualization) via the [Code-Hex/vz](https://github.com/Code-Hex/vz) Go library.

## Requirements

| Requirement | Details |
|-------------|---------|
| macOS | 13 (Ventura) or later |
| Architecture | Apple Silicon (arm64) or Intel (x86_64) |
| Go | 1.22+ with CGO enabled |
| Entitlement | `com.apple.security.virtualization` (required) |
| Entitlement | `com.apple.vm.networking` (optional, for compose inter-VM networking) |

## Quick start

```bash
# Build all binaries (ad-hoc signed with basic entitlements)
make build-darwin

# Boot Alpine as a microVM
./gocracker run \
  --image alpine:3.20 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-arm64-Image \
  --cmd 'echo hello && uname -a' \
  --wait --tty off --net auto
```

## Building

```bash
# Standard build (ad-hoc signing, basic entitlements)
make build-darwin

# Build with full entitlements (requires Developer ID certificate)
make build-darwin-e2e DARWIN_SIGN_IDENTITY="Developer ID Application: Your Name"

# Install to /usr/local/bin
sudo make install
```

The darwin build requires `CGO_ENABLED=1` because vz uses Objective-C bridging via cgo. The Makefile handles this automatically.

## Entitlements

Two entitlement profiles are provided:

| File | Entitlements | Signing |
|------|-------------|---------|
| `entitlements.local.plist` | `com.apple.security.virtualization` | Ad-hoc (`-s -`) |
| `entitlements.plist` | `com.apple.security.virtualization` + `com.apple.vm.networking` | Developer ID required |

`com.apple.vm.networking` is a **restricted entitlement**. Apple's AMFI (AppleMobileFileIntegrity) rejects ad-hoc signed binaries that carry it. You need a valid Apple Developer ID certificate to sign binaries with this entitlement.

## Feature parity with Linux

### Fully supported

| Feature | Notes |
|---------|-------|
| `gocracker run --image` | OCI images from any registry |
| `gocracker run --dockerfile` | Multi-stage builds, all Dockerfile instructions |
| `gocracker repo` | Clone + build + boot from git repos |
| `gocracker build` | Build ext4 disk images without booting |
| `gocracker serve` | REST API server (Firecracker-compatible + extended) |
| `gocracker compose` (single service) | One VM per service with NAT networking |
| Interactive console (`--tty force`) | Raw PTY, Ctrl-] to detach, Ctrl-C forwarded to guest |
| Networking (`--net auto`) | DHCP via vz NAT, full internet access |
| Shared volumes (virtio-fs) | Native vz shared directories, no virtiofsd |
| Exec into VM | Via virtio-vsock, same as Linux |
| Multi-vCPU | Configurable via `--cpus` |
| Memory balloon | Basic support (stats not available) |
| Entropy (virtio-rng) | Native vz entropy device |
| Snapshot save | Via Virtualization.framework `SaveMachineState` (macOS 14+) |
| Disk caching | Cached mode + no-sync for fast I/O |

### Requires Developer ID signing

| Feature | Entitlement needed | Notes |
|---------|-------------------|-------|
| Compose inter-VM networking | `com.apple.vm.networking` | Uses vmnet shared mode for a bridge network where services resolve each other by name |
| Port forwarding (host → guest) | `com.apple.vm.networking` | TCP/UDP forwarding for published compose ports |

### Not supported (Virtualization.framework limitations)

| Feature | Reason |
|---------|--------|
| Snapshot restore (cross-process) | vz requires the same VM object; `gocracker restore` CLI cannot recreate the exact configuration |
| Live migration | No dirty page tracking in vz |
| Memory hotplug | No memory slot API in vz |
| TAP networking | Use NAT mode instead (`--net auto`) |
| Rate limiters | vz abstracts device transport |
| Seccomp/jailer | macOS sandbox (`sandbox-exec`) available but not equivalent |

## Architecture differences

### Networking

On **Linux**, gocracker creates TAP devices, bridges, and network namespaces for compose stacks. Each VM gets a TAP interface in a shared bridge, with iptables NAT for internet access.

On **macOS**, two networking modes are available:

1. **NAT** (default, no special entitlement): Each VM gets an independent NAT network via `vz.NATNetworkDeviceAttachment`. The VM receives an IP via DHCP from the vz NAT gateway (typically `192.168.64.x`). VMs can reach the internet but cannot see each other.

2. **vmnet shared** (requires `com.apple.vm.networking`): Compose stacks use `vz.VmnetNetwork` with `VmnetModeShared` to create a shared subnet. All VMs in the stack share the same virtual network and can communicate by IP. Port forwarding from the host to guest VMs uses userspace TCP/UDP proxies.

### Console

On **Linux**, the guest console uses a 16550A UART device (I/O ports on x86, MMIO on ARM64) connected to a PTY.

On **macOS**, the guest console uses a virtio-console device connected via `os.Pipe()` pairs. The kernel cmdline uses `console=hvc0` instead of `console=ttyS0`.

### Guest kernel

The guest kernel must be compiled for the host architecture (ARM64 on Apple Silicon, x86_64 on Intel Mac). The kernel must include:

```
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_CONSOLE=y
CONFIG_VIRTIO_FS=y
CONFIG_VIRTIO_VSOCK=y
CONFIG_PCI=y
CONFIG_PCI_HOST_GENERIC=y
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_EXT4_FS=y
```

Pre-built kernels are included in `artifacts/kernels/`. Decompress with:

```bash
gzip -dk artifacts/kernels/gocracker-guest-standard-arm64-Image.gz
```

### Guest init

The gocracker guest init handles platform differences transparently:

- Sets `PATH` for DHCP client discovery (`udhcpc`, `dhclient`)
- Auto-detects the first non-loopback NIC (handles both `eth0` and `enp0sN`)
- Waits for link carrier before DHCP
- Uses `LINUX_REBOOT_CMD_POWER_OFF` on macOS (via `gc.shutdown=poweroff` cmdline) instead of `LINUX_REBOOT_CMD_RESTART` to ensure clean VM shutdown

### Shutdown

On **Linux** with KVM, the guest calls `reboot()` which triggers a KVM exit that gocracker catches. The `reboot=k` cmdline arg routes this through the i8042 keyboard controller reset.

On **macOS** with vz, the guest calls `reboot(POWER_OFF)` which triggers a PSCI power-off. Virtualization.framework reports this as `VirtualMachineStateStopped`, and gocracker's `watchStateChanges` goroutine detects it.

## Troubleshooting

### "create vmnet network: failure"

The binary needs `com.apple.vm.networking` entitlement with a proper Developer ID signing identity. Ad-hoc signed binaries with this entitlement are rejected by AMFI.

```bash
make build-darwin-e2e DARWIN_SIGN_IDENTITY="Developer ID Application: Your Name"
```

### VM boots but no console output

Ensure the kernel cmdline includes `console=hvc0` (not `console=ttyS0`). The darwin backend sets this automatically via `runtimecfg.DarwinKernelArgs()`.

### VM reboots in a loop

Ensure the guest init binary includes the `gc.shutdown=poweroff` support. Rebuild with `go generate ./internal/guest/`.

### DHCP fails (no IP assigned)

Ensure the guest init binary has `PATH` set. The init sets `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` at startup so `udhcpc` can be found.
