# Security Policy

## Security Model Overview

gocracker isolates guest workloads through multiple defense-in-depth layers.
Each VM runs inside its own KVM virtual machine with hardware-enforced memory
isolation, complemented by host-side sandboxing.

## Isolation Layers

### KVM Hardware Isolation

Each guest runs in a separate KVM virtual machine. Guest memory is isolated by
hardware (Intel VT-x / AMD-V / ARM VHE). The guest cannot access host memory
or other VMs.

### Seccomp BPF Syscall Filtering

The VMM process applies a seccomp BPF filter that restricts the set of system
calls available to the host-side VMM code, limiting the attack surface if a
guest-triggered bug causes unintended host behavior.

### Jailer (chroot + mount namespace + PID namespace)

When `--jailer on` (the default), each VM worker runs inside:

- A **chroot** jail with only the files needed by that VM.
- A **mount namespace** so the worker cannot see or modify the host filesystem.
- A **PID namespace** so the worker cannot signal other host processes.

### Privilege Drop (UID/GID)

After setting up namespaces, the jailer drops privileges to a non-root
UID/GID. The `--uid` and `--gid` flags on `gocracker serve` control the
target identity.

### Cgroups v2 Resource Limits

When running under the jailer, the VMM process is placed in a cgroup v2 scope
that enforces memory and CPU limits, preventing a single VM from exhausting
host resources.

## Reporting Vulnerabilities

Please do **not** file public GitHub issues for suspected vulnerabilities.

Report security issues privately to: **security@gocracker.dev**

Include:

- Affected version or commit hash
- Host OS / kernel version
- Reproduction steps
- Expected vs. actual behavior
- Impact assessment (if known)

**Expected response time:** acknowledgment within 48 hours, triage within
7 business days. We will coordinate disclosure timing with the reporter.

## Scope

Security-sensitive areas include:

- **Guest memory and vCPU isolation** -- KVM ioctl usage, memory slot setup
- **Virtio device emulation** -- virtio-blk, virtio-net, virtio-rng, virtio-vsock, virtio-balloon; any guest-controlled input parsed by the host
- **OCI extraction and build isolation** -- layer unpacking, Dockerfile builds
- **Jailer and namespace setup** -- chroot, mount namespace, PID namespace, cgroup configuration
- **API authentication and path validation** -- auth token enforcement, trusted directory checks, path traversal prevention
- **Snapshot and migration artifacts** -- snapshot bundles contain raw memory; ensure they are stored securely and not served to untrusted parties

## Supported Versions

Security fixes are applied to the **latest release** only. There is no
long-term support for older versions at this time.

| Version | Supported |
|---------|-----------|
| Latest | Yes |
| Older | No |
