# gocracker Windows + macOS Portability — Decision

Status: **Skip the port for now.** Re-evaluate when an Apple Silicon
user shows up with a real measurement workload that Lima/OrbStack
can't serve.

This doc is the consolidated decision; the per-platform design notes
in [hvf-backend.md](hvf-backend.md) and [whp-backend.md](whp-backend.md)
spell out *how* a port would work if we ever build one.

## Why skip

A multi-agent audit of the codebase (see `feat/slirp-net-and-atomic-disk-meta`
session notes) flagged three things the per-platform docs underplay:

1. **The KVM coupling is deeper than "just swap the backend".** The
   `machineArchBackend` interface in [pkg/vmm/arch.go](../../pkg/vmm/arch.go)
   is an *arch* split, not a *hypervisor* split — every method signature
   takes `*kvm.VCPU` / `*kvm.VM` / `*kvm.System` directly, and the
   persisted `VCPUState` struct in [pkg/vmm/vmm.go](../../pkg/vmm/vmm.go)
   embeds `kvm.Regs` / `kvm.Sregs` / `kvm.MPState` as JSON-serialised
   fields. So the on-disk snapshot format is already KVM-shaped. Any
   second hypervisor needs (a) a new abstraction layer above
   `internal/kvm/`, (b) a snapshot v2 format with versioned vCPU
   state, and (c) a translation table between hypervisor-native
   register enums.

2. **Eventfd-everywhere.** Virtio device IRQs in
   [pkg/vmm/arch_arm64.go](../../pkg/vmm/arch_arm64.go) and
   [internal/virtio/fs.go](../../internal/virtio/fs.go) plumb host
   eventfds straight into `KVM_IRQFD`. Neither WHP nor HVF has an
   eventfd-equivalent: WHP uses `WHvRequestInterrupt` (synchronous
   interrupt injection), HVF uses `hv_vcpu_set_pending_interrupt`.
   Replacing eventfd with a function-pointer interrupt-injection model
   touches ~7 sites in `pkg/vmm/arch_arm64.go` plus device internals.
   It's not a 1-day job.

3. **The features that differentiate gocracker don't port cleanly.**
   - **Snapshot/restore** (KVM-shaped on-disk format).
   - **virtio-fs** (vhost-user — Linux-helper-process protocol that
     assumes shared memfd; macOS especially has no clean replacement).
   - **vsock** (Linux's AF_VSOCK; Windows has Hyper-V Sockets with a
     different addressing scheme; macOS has nothing native).
   - **Jailer** (`internal/jailer/jailer.go` is `//go:build linux` and
     uses `unshare(CLONE_NEWNS|CLONE_NEWPID)`, `pivot_root`, `setns`).

A port without these is roughly Firecracker's first-cut feature set,
on platforms where Firecracker doesn't ship either — limited demand
for that intersection.

## What users actually have today

- **Windows dev laptops**: WSL2 already gives them KVM under MSHV.
  Performance is within ~10% of bare metal on most workloads. The
  honest answer is "use WSL2"; that's what most Windows developers
  doing container-class work already do.

- **Apple Silicon dev laptops**: Lima, OrbStack, UTM, Tart all run a
  Linux VM via macOS's `Virtualization.framework` and gocracker works
  inside. There's a nesting overhead but for dev ergonomics it's
  adequate. macOS users running `gocracker run --warm node:20-alpine`
  inside Lima see TTI in the same envelope as bare-metal Linux, plus
  a one-time ~2 s vmnet handshake on Lima boot.

- **macOS CI for VM workloads**: rare. GitHub Actions macOS runners
  can't nest hypervisors easily; most teams that need VM-class
  isolation in CI run Linux runners.

- **Production**: zero demand for non-Linux. Production deployments
  of gocracker are on Linux servers with hardware KVM. Firecracker /
  Kata / kvmtool all made the same Linux-only call and don't apologise
  for it.

## When to revisit

Two concrete signals would make us reconsider, in order:

1. **A specific Apple Silicon user shows up wanting native HVF for
   a measurement workload** that Lima's nested-VM overhead invalidates
   — e.g. publishing benchmark numbers on M-series silicon, or
   shipping an SDK that runs end-to-end on a dev laptop without Lima
   in the loop. The HVF audit suggested ~5–7 weeks for the boot+exec
   parity v1 (no snapshot, no vsock, no virtio-fs). That's a credible
   investment if there's a single committed user.

2. **Windows production demand from a corporate caller**. So far the
   only Windows users we've seen route through WSL2; if a customer
   shows up insisting on native Windows, the WHP backend is ~6–9
   weeks for boot+exec parity, much longer for snapshot. Revisit only
   with that demand signal.

## Why now, not later

This decision is recorded NOW (rather than letting users discover the
absence) because:

- The per-feature design work in
  [hvf-backend.md](hvf-backend.md) and [whp-backend.md](whp-backend.md)
  was previously labelled "Planning" without a clear veto. That left
  the door ambiguously open and would have invited "is X done?"
  questions for years.
- The Pieza B work just landed (commit `cdbcfc7`) and adds another
  Linux-specific surface (the in-guest node REPL runner uses
  `/run/gocracker/warm-node.sock` — Unix domain sockets — and is
  spawned by the toolbox agent on PID 1's behalf). Each Linux-specific
  primitive added makes the future port more expensive. A clear
  "Linux-only, by design" stance lets us keep adding those primitives
  without owing a Win/Mac equivalent.

## Implementation guidance for users

When asked "does gocracker run on macOS / Windows?", the canonical
answer is:

- **macOS (Apple Silicon)**: install [Lima](https://lima-vm.io/) or
  [OrbStack](https://orbstack.dev/), boot a Linux VM with KVM,
  install gocracker inside. Performance is within Lima's nested-VM
  overhead (~5–10 % on most workloads, larger on snapshot-heavy
  paths).

- **Windows**: install [WSL2](https://learn.microsoft.com/en-us/windows/wsl/install)
  with a recent kernel (5.15+), `wsl --install -d Ubuntu`, install
  gocracker in the WSL distro. KVM is real (WSL2 runs under MSHV
  with KVM acceleration on top). Performance matches bare-metal
  Linux closely.

Both routes give users gocracker's full feature set including
snapshot/restore, virtio-fs, vsock, the jailer, and the new
`base-node-warm` warm-runtime path. No code change required —
the user's host OS is irrelevant; what matters is `/dev/kvm` being
reachable from the gocracker process.
