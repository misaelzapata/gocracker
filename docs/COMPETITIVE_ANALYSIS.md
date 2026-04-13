# How gocracker Fits In

gocracker started as a hobby project to learn KVM internals and Go systems
programming. Along the way it grew into something that actually works -- you can
pull any Docker image and boot it as a real Linux VM in about a second. This page
explains where gocracker sits relative to the excellent projects that inspired it,
and where those projects are clearly better.

We are not trying to replace any of them. We just wanted one binary that does
`docker run` but with real VMs, and we learned a ton of low-level Linux along
the way.

## The Landscape

There are broadly three categories of projects in this space:

**Low-level VMMs** (Firecracker, Cloud Hypervisor, crosvm) -- these give you the
KVM plumbing. You bring your own kernel, disk image, and networking. They are
production-grade and battle-tested at massive scale.

**Container-VM runtimes** (Kata Containers, krunvm, Ignite) -- these bridge the
gap between containers and VMs. They plug into existing container ecosystems
(containerd, CRI, Podman) and add VM isolation underneath.

**Developer tools** (Lima, Colima, Podman machine) -- these run a full Linux VM
on macOS/Windows so you can use Docker or containerd inside it.

gocracker sits somewhere between the first two: it is both the VMM and the
container runtime in one binary. This is simpler to set up but means we
implement everything ourselves, which is both the fun part and the risky part.

## Honest Comparison

| | gocracker | Firecracker | Kata Containers | Cloud Hypervisor | gVisor |
|---|---|---|---|---|---|
| **What it is** | Hobby VMM + container runtime | Production VMM (AWS) | K8s VM runtime | Production VMM (Intel) | Userspace kernel |
| Written in | Go | Rust | Rust/Go | Rust | Go |
| One binary, no deps | Yes | Yes | No (needs containerd + VMM) | Yes | Yes (runsc) |
| Pull OCI images | Yes | No | Via CRI | No | Via runsc |
| Build Dockerfiles | Yes | No | No | No | No |
| Docker Compose | Yes | No | Partial | No | Via Docker |
| Snapshots | Yes | Yes (production-grade) | No | Yes | Checkpoint |
| ARM64 | Yes (tested) | Yes (production) | Yes | Yes | Yes |
| Stars | We just got here | 33.6k | 7.7k | 5.5k | 18.1k |
| Production use | No | AWS Lambda, Fly.io | Telcos, cloud providers | Azure, Kata | GKE Sandbox |

## What gocracker Brings to the Table

None of these features are unique on their own -- what is unusual is having all
of them in one self-contained binary:

1. **Pure Go, single binary, direct KVM** -- no QEMU, no Firecracker, no external VMM dependency
2. **Native OCI image pulling** -- not via containerd shim or CRI
3. **Native Dockerfile builds** -- not delegated to Docker or Buildah
4. **Native git repo builds** -- clone, detect Dockerfile, build, boot
5. **Native Docker Compose** -- each service is a real VM, not a container
6. **Firecracker-compatible REST API** -- drop-in for existing FC tooling
7. **Snapshot/restore + live migration** -- full VM state capture and transfer
8. **ARM64 + x86-64** -- irqfd, GICv2/v3, tested on AWS a1.metal bare metal (Graviton 1)
9. **328 real-world projects validated** -- not just hello-world demos

The closest historical project was **Ignite** (Weaveworks), which ran OCI images
as Firecracker VMs but required Firecracker + containerd as dependencies. It was
archived in December 2023. gocracker fills that gap with zero external
dependencies.

## What We Learned From Each

### Firecracker

Firecracker is the biggest inspiration. We studied its device model (virtio MMIO),
jailer architecture, seccomp profiles, and ARM64 implementation in detail. Our
`internal/kvm/` package started as "what would Firecracker's KVM bindings look
like in Go?" The irqfd interrupt delivery, GIC setup, and PSCI boot flow were all
implemented by reading Firecracker's Rust source side by side.

**Where Firecracker is better:** Production hardening. Millions of VMs in
production. Demand-paged snapshot loading (UFFD). CPU templates. Years of
security auditing. We are nowhere near that level.

**What we add:** You can type `gocracker run --image postgres:16` and get a
running PostgreSQL VM. With Firecracker you would need to separately pull the
image, extract it, build a disk, generate an initrd, configure networking, and
write a JSON API call.

### Kata Containers

Kata showed that running containers as VMs is practical for real workloads. Their
CRI shim approach means existing Kubernetes clusters get VM isolation
transparently.

**Where Kata is better:** Kubernetes integration, multiple hypervisor backends
(QEMU, Firecracker, Cloud Hypervisor, Dragonball), confidential computing
(TDX/SEV), IBM Z and Power support.

**What we add:** No dependencies. You do not need containerd, a CRI runtime, or
Kubernetes. One binary, one command.

### Cloud Hypervisor

Cloud Hypervisor has the richest device model of any modern VMM: hot-plug for
CPU/memory/PCI, VFIO passthrough, virtio-mem, MSHV support, and more.

**Where Cloud Hypervisor is better:** Device diversity, hot-plug, Windows guests,
VFIO/GPU passthrough, Microsoft Hypervisor support.

**What we add:** The container workflow. Cloud Hypervisor is a VMM; gocracker
is a VMM plus image pulling, Dockerfile building, and Compose orchestration.

### gVisor

gVisor takes a completely different approach -- it reimplements the Linux kernel
in Go and intercepts syscalls in userspace. No actual VM, no kernel to boot.

**Where gVisor is better:** Sub-millisecond startup, deeper syscall
compatibility tracking, GPU support (CUDA passthrough), GKE Sandbox integration.

**What we add:** True hardware isolation via KVM. A real Linux kernel running
in the guest. The virtio device model.

### Ignite (archived)

Ignite was the project closest to gocracker's vision: OCI images as Firecracker
VMs, written in Go. It was archived in December 2023 when Weaveworks shut down.
Ignite required Firecracker and containerd as external dependencies.

gocracker does not depend on either. Everything -- the VMM, OCI pulling,
Dockerfile building, networking -- is in one Go binary.

## Things We Would Like to Add

These are features we have seen in other projects and would love to implement
someday. They are listed roughly in order of how excited we are about them:

1. **GPU passthrough (VFIO)** -- Cloud Hypervisor, crosvm, and libkrun all
   have this. We wrote a [detailed plan](VFIO_GPU_PASSTHROUGH_PLAN.md).

2. **macOS support** -- Lima and libkrun run on Apple Silicon via
   Hypervisor.framework. We would love to bring gocracker to Mac.

3. **Transparent Socket Impersonation** -- libkrun's networking model where
   guest sockets are transparently forwarded to the host. No TAP, no
   iptables, no bridge. Brilliant idea.

4. **Kubernetes CRI shim** -- Kata's main integration point. Would let
   gocracker be a drop-in runtime for Kubernetes pods.

5. **Demand-paged snapshots (UFFD)** -- Firecracker and Fly.io use this for
   sub-30ms cold starts from snapshots. Currently our restore loads the
   entire RAM file.

6. **Hot-plug** -- Cloud Hypervisor can hot-add CPUs, memory, and PCI
   devices while the VM is running.

## Why Go?

Most VMMs are written in Rust (Firecracker, Cloud Hypervisor, crosvm, libkrun).
We chose Go because:

- This started as a learning project and Go is what we know
- Go's standard library handles OCI/HTTP/JSON/networking out of the box
- `CGO_ENABLED=0` gives us a truly static binary with no libc dependency
- Cross-compilation is trivial (`GOARCH=arm64 go build`)
- The `unsafe` package gives us the raw pointer access we need for KVM ioctls

The trade-off is that Go's garbage collector adds latency compared to Rust's
zero-cost abstractions. For a hobby project focused on developer experience
rather than microsecond-level VM isolation performance, we think that is fine.

## A Note on Maturity

gocracker is alpha software. It works well enough to boot 328 real-world
projects and run Flask+PostgreSQL compose stacks on ARM64 over the internet.
But it has not been audited, load-tested, or deployed at scale. The projects
listed above have years of production hardening that we do not.

If you need production VM isolation today, use Firecracker or Kata Containers.
If you want to play with running containers as VMs, learn how KVM works, or
just enjoy the idea of `gocracker run --image` producing a real Linux VM --
welcome aboard.

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [How gocracker Fits In](COMPETITIVE_ANALYSIS.md) | [Security Policy](../SECURITY.md)

