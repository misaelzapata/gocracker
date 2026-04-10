# Competitive Analysis

How gocracker compares to other microVM and container-VM projects.

## Comparison Matrix

| Project | Language | Backend | OCI Images | Dockerfile | Compose | Snapshot | ARM64 | Stars |
|---------|----------|---------|------------|------------|---------|----------|-------|-------|
| **gocracker** | Go | KVM direct | Native | Native | Native | Yes | Yes | -- |
| Firecracker | Rust | KVM direct | No | No | No | Yes | Yes | 33.6k |
| Kata Containers | Rust/Go | QEMU/FC/CH | Via CRI | No | Partial | No | Yes | 7.7k |
| Cloud Hypervisor | Rust | KVM/MSHV | No | No | No | Yes | Yes | 5.5k |
| gVisor | Go | Syscall/KVM | Via runsc | No | Via Docker | Checkpoint | Yes | 18.1k |
| Ignite | Go | Firecracker | Native | No | No | No | No | 3.5k (archived) |
| krunvm | Rust | libkrun | Native | No | No | No | Yes | 1.6k |
| Lima | Go | QEMU/VZ | Via containerd | Via containerd | Via containerd | No | Yes | 20.7k |
| Podman machine | Go | QEMU/HVF | Native | Buildah | podman-compose | No | Yes | 31.3k |
| crosvm | Rust | KVM | No | No | No | Yes | Yes | 1.2k |
| Unikraft | Go/C | QEMU/Xen/FC | OCI packaging | Yes | Yes | No | Yes | 399 |

## What Makes gocracker Unique

No other single project combines all of these:

1. **Pure Go, single binary, direct KVM** -- no QEMU, no Firecracker, no external VMM
2. **Native OCI image pulling** -- not via containerd or CRI shim
3. **Native Dockerfile builds** -- not delegated to Docker or Buildah
4. **Native git repo builds** -- clone, detect Dockerfile, build, boot
5. **Native Docker Compose** -- each service is a real VM
6. **Firecracker-compatible REST API** -- drop-in for FC tooling
7. **Snapshot/restore + live migration**
8. **ARM64 + x86-64** with irqfd, GICv2/v3, tested on real hardware

The closest historical project was **Ignite** (Weaveworks), which ran OCI images as
Firecracker VMs but required Firecracker + containerd as dependencies. It was
archived in December 2023.

## Feature Gaps (opportunities from competitors)

| Feature | Who Has It | Complexity | Priority |
|---------|-----------|------------|----------|
| GPU passthrough (VFIO) | Cloud Hypervisor, crosvm, libkrun | High (~3K LOC) | [Planned](VFIO_GPU_PASSTHROUGH_PLAN.md) |
| macOS (Hypervisor.framework) | Lima, libkrun, Colima | Medium | Planned |
| Kubernetes CRI shim | Kata Containers | Medium | Consider |
| Transparent Socket Impersonation | libkrun/krunvm | Medium | Consider |
| Hot-plug (CPU/memory/PCI) | Cloud Hypervisor | Medium | Consider |
| Demand-paged snapshots (UFFD) | Firecracker, Fly.io | Medium | Consider |
| Confidential computing (TDX/SEV) | Kata, Cloud Hypervisor | High | Future |
| OCI runtime spec (runc drop-in) | crun/krun | Low | Consider |
| GPU virtualization (virtio-gpu) | crosvm, libkrun | High | Future |
| Cloud-init/Ignition metadata | Flintlock, Lima | Low | Consider |

## Detailed Notes

### vs. Firecracker
Firecracker is a bare VMM -- it provides the KVM plumbing but no container
runtime. You must separately pull images, build disks, manage networking.
gocracker builds on the same security model (jailer, seccomp, minimal device
model) and adds the complete developer experience: `gocracker run --image`.

### vs. Kata Containers
Kata is Kubernetes-native via CRI shimv2. It requires containerd + an external
VMM (QEMU, Firecracker, or Cloud Hypervisor). gocracker is a single binary
with no external dependencies. Kata has wider platform support (ppc64le, s390x)
and confidential computing (TDX/SEV).

### vs. gVisor
gVisor is not a VMM -- it intercepts syscalls in userspace. It provides
container-like isolation without hardware virtualization. gocracker provides
true KVM hardware isolation. gVisor has sub-millisecond startup; gocracker
boots a real Linux kernel (~1 second).

### vs. Cloud Hypervisor
Cloud Hypervisor is a feature-rich Rust VMM with hot-plug, VFIO passthrough,
MSHV support, and more device types. It has no container runtime -- it's a
VMM like Firecracker. gocracker is simpler but includes the full container
workflow.
