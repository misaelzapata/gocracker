# Troubleshooting

Common issues and solutions when running gocracker.

---

## "KVM not available" or "cannot open /dev/kvm"

KVM requires hardware virtualization support.

1. Check that `/dev/kvm` exists:
   ```bash
   ls -l /dev/kvm
   ```
2. If it does not exist, enable Intel VT-x or AMD-V in your BIOS/UEFI settings.
3. Load the KVM module:
   ```bash
   sudo modprobe kvm_intel   # Intel
   sudo modprobe kvm_amd     # AMD
   ```
4. Add your user to the `kvm` group:
   ```bash
   sudo usermod -aG kvm $USER
   ```
   Log out and back in for the group change to take effect.

---

## "Permission denied"

gocracker needs root or equivalent capabilities for KVM access and network
setup (TAP interfaces, iptables rules).

- Run with `sudo`, or
- Grant `CAP_NET_ADMIN` and ensure `/dev/kvm` is accessible to your user.

When using the jailer (`--jailer on`, the default), the process requires root
to set up chroot, mount namespaces, and PID namespaces before dropping
privileges.

---

## "iptables: not found"

gocracker uses iptables for guest NAT and port forwarding. Install the
appropriate package for your distribution:

```bash
# Debian/Ubuntu
sudo apt install iptables

# Fedora/RHEL
sudo dnf install iptables-nft

# Arch
sudo pacman -S iptables-nft
```

---

## "Disk full" / "no space left on device"

The default disk size is 2048 MiB (4096 MiB for compose). If your application
needs more space:

```bash
gocracker run --disk 8192 ...
```

To clean the OCI/build cache:

```bash
rm -rf /tmp/gocracker/cache
```

Snapshot bundles also consume disk space (they include a full RAM dump and disk
image). Clean old snapshots when no longer needed.

---

## "Boot hangs" (no kernel output on console)

The guest kernel must be built with these options enabled:

- `CONFIG_VIRTIO=y` -- virtio core
- `CONFIG_VIRTIO_MMIO=y` -- MMIO transport (gocracker does not use PCI)
- `CONFIG_VIRTIO_BLK=y` -- virtio block device for root disk
- `CONFIG_VIRTIO_NET=y` -- virtio network (if using `--net auto`)
- `CONFIG_EXT4_FS=y` -- ext4 filesystem for root disk
- `CONFIG_SERIAL_8250=y` and `CONFIG_SERIAL_8250_CONSOLE=y` -- serial console
- Kernel command line must include `console=ttyS0`

If the kernel boots but the serial console shows nothing, verify the boot args
include `console=ttyS0`.

---

## "Network unreachable" inside the guest

1. Ensure IP forwarding is enabled on the host:
   ```bash
   sudo sysctl -w net.ipv4.ip_forward=1
   ```
2. Check that gocracker detected the correct upstream network interface. If
   your host has multiple interfaces, gocracker picks the default route
   interface. Verify with:
   ```bash
   ip route show default
   ```
3. Ensure the TAP interface is up and has an IP assigned. With `--net auto`,
   gocracker handles this automatically.

---

## "exec agent connection timed out"

The exec feature uses virtio-vsock to communicate with an agent inside the
guest. The guest kernel must have vsock support built in (not as a module):

```
CONFIG_VIRTIO_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS_COMMON=y
```

If vsock is built as a module, the agent cannot connect during early boot.
Rebuild the kernel with these as built-in (`=y`).

---

## "NO-CARRIER on eth0"

Older versions of gocracker had a race condition where the virtio-net link
status was not updated before the guest driver finished initialization. This
has been fixed: the configuration change interrupt is now sent on `DRIVER_OK`.

Update to the latest version of gocracker to resolve this issue.

---

## ARM64: "Bad system call"

The seccomp BPF filter may block system calls that differ between ARM64 and
x86-64. To diagnose, disable seccomp temporarily:

```bash
export GOCRACKER_SECCOMP=0
sudo gocracker run ...
```

If this resolves the issue, file a bug with the specific syscall that was
blocked (check `dmesg` or `journalctl` for seccomp audit messages).

---

## General Debugging Tips

- Use `--wait` to keep the VM running and see full console output.
- Use `--tty force` to attach an interactive console.
- Retrieve UART logs via the API: `GET /vms/{id}/logs`.
- Check VM events: `GET /vms/{id}/events` for boot progress and errors.
- Stream events in real time: `curl -sN http://localhost:8080/vms/{id}/events/stream`.

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [Security Policy](../SECURITY.md)
| [Security Policy](../SECURITY.md)

