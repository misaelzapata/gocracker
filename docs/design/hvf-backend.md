# macOS Hypervisor.framework (HVF) Backend — Design

Status: **Planning**. Not implemented. node-vmm proved the model in C++
([`native/hvf/backend.cc`](https://github.com/misaelzapata/node-vmm),
~3900 lines, ARM64-focused on Apple Silicon). This doc plans the Go
shape so we can decide whether to invest.

## TL;DR recommendation

Don't build this yet. Same reasoning as
[whp-backend.md](whp-backend.md): narrow audience, large carrying cost.
Users on macOS who need this today have node-vmm. Revisit when there's
a gocracker user who specifically needs the Go runtime on Apple
Silicon.

If we do build it, the Apple Silicon (ARM64) variant is the correct
priority — Intel macOS is already legacy and the install base is
shrinking. The ARM64 path slots cleanly next to gocracker's existing
Linux/KVM ARM64 work in `pkg/vmm/arch_arm64.go`.

## What HVF is

Apple's hypervisor framework, public since macOS 10.10 (Intel) and 11.0
(ARM64). It exposes:

- VMs
- vCPUs (called "virtual CPUs" on Intel, "vCPU" on ARM64)
- Memory mapping (host VA → guest PA)
- Run-loop with structured exits

The ARM64 API surface lives in `<Hypervisor/Hypervisor.h>` and uses C
calling conventions. No syscalls — it's a userspace library, dynamically
linked.

## Surface in Go

Unlike WHP (which is `syscall.LazyDLL`-friendly), HVF has no
`LoadLibrary` shim — it's `dlopen` on a system framework. Two options:

### Option A: cgo

```go
//go:build darwin && arm64
package hvf

/*
#cgo LDFLAGS: -framework Hypervisor
#include <Hypervisor/Hypervisor.h>
*/
import "C"
```

Pros: type-safe wrapper for `hv_*` calls; matches how Apple expects
embedders to use the API; node-vmm's reference implementation is
directly portable.

Cons: cgo. Breaks gocracker's "single static binary" claim for macOS
builds. Cross-compiling from Linux to darwin/arm64 needs a darwin
toolchain.

### Option B: pure-Go via `syscall.Syscall`

The `hv_*` functions are exported C symbols in
`/System/Library/Frameworks/Hypervisor.framework`. They could be looked
up at runtime via `purego` (or hand-rolled). This is what
`fyne.io/fyne` and `gioui.org` do for AppKit; precedent exists.

Pros: no cgo, single static binary, cross-compilable.

Cons: more bring-up work, opaque to symbolic debuggers, fragile across
macOS releases.

**Recommendation:** start with cgo. Production gocracker on macOS is
not the priority — the goal is "does it work?". Optimize binary
shape later.

```
//go:build darwin && arm64
internal/hvf/hvf_darwin_arm64.go    — cgo bindings
internal/hvf/vm.go                  — VM handle + memory map
internal/hvf/vcpu.go                — vCPU run loop
pkg/vmm/arch_hvf_darwin_arm64.go    — VMM glue
```

## Component plan (ARM64 only)

### 1. VM + memory mapping (~300 LoC)

- `hv_vm_create(NULL)` — single global VM context per process
  (HVF restriction).
- `hv_vm_map(hostVA, gpa, size, flags)` for each memory region.
  Flags are R/W/X, maps to `HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC`.
- Dirty tracking: HVF doesn't expose a clean dirty bitmap on
  ARM64. Workaround: `mprotect` the host VA to read-only, take page
  faults in `hv_vcpu_run` exits and mark dirty manually. Slow but
  correct. Snapshot/migrate users are not the macOS audience anyway.

### 2. vCPU run loop (~500 LoC)

- `hv_vcpu_create(&vcpu, NULL)` — one per goroutine.
- ARM64 register init: `hv_vcpu_set_reg`, `hv_vcpu_set_sys_reg`. CPU
  feature register (ID_AA64*) values come back from `hv_vcpu_get_*`;
  filter to the subset we want the guest to see.
- Boot: PC = kernel entry, X0 = FDT physical address. Identical to
  what `pkg/vmm/arch_arm64.go` already does for KVM.
- `hv_vcpu_run(vcpu)` returns and you read `hv_vcpu_exit_t`. Exit
  reasons:
  - `HV_EXIT_REASON_EXCEPTION` — most cases. Decode the ESR, dispatch
    to MMIO/SMC/HVC handlers.
  - `HV_EXIT_REASON_VTIMER_ACTIVATED` — timer interrupt; assert IRQ.
  - `HV_EXIT_REASON_CANCELED` — graceful exit.

### 3. Device emulation reuse

Same as WHP: virtio MMIO devices in `internal/virtio` are
platform-neutral and slot in directly.

vsock on macOS is the interesting question. KVM's vsock uses
`/dev/vhost-vsock`; HVF has nothing equivalent. Either port the vhost
shim to be hostable in process (we already have `internal/vhostuser`)
or skip vsock for the macOS first cut and use serial-console-driven
exec.

### 4. Boot path

ARM64 boot is `internal/loader.LoadKernelARM64` plus FDT generation in
`internal/fdt`. Both already work — they target the guest physical
layout, not the host hypervisor. Same vmlinux that boots under KVM
boots under HVF.

### 5. Networking

Slirp (this branch) is the macOS NIC path. Apple's `vmnet` framework
exists for TAP-equivalent functionality but requires the
`com.apple.vm.networking` entitlement and an Apple Developer-signed
binary. Out of scope for an open-source project's default install.
node-vmm wires `vmnet` behind a flag; we can do the same eventually.

### 6. Console

UART 16550A doesn't make sense on ARM64 — guest expects PL011 (or
something else). Reuse `internal/pl011` from the existing ARM64 KVM
path.

## Effort estimate

- VM + memory: ~1 week (HVF API is well-documented and small)
- vCPU run loop + ESR decode: ~2 weeks
- ARM64 boot wire-up + FDT: ~3 days (mostly reuse)
- Tests + bring-up + boot Alpine ARM64: ~1 week
- VMM glue + integration: ~1 week

So ~5 weeks for "boots Alpine to a shell on Apple Silicon". Snapshots
and vsock are another month each.

## What to skip in v1

- Intel macOS (HVF on x86 has different APIs; small audience).
- Snapshots (HVF doesn't expose vCPU register state in a stable
  snapshot-able shape; would need pause + manual register harvest).
- Live migration.
- vmnet networking (entitlement + signing dance).

## When this is worth doing

If a meaningful fraction of gocracker's user base develops on Apple
Silicon and wants to run microVMs natively (not via Lima/UTM/Tart). The
demand signal hasn't appeared yet; revisit when it does.
