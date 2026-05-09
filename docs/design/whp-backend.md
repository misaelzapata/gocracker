# Windows Hypervisor Platform (WHP) Backend — Design

Status: **Planning**. Not implemented. node-vmm proved the model in C++
([`native/whp/backend.cc`](https://github.com/misaelzapata/node-vmm),
~4800 lines); this doc plans the Go shape so we can decide whether to
invest.

## TL;DR recommendation

Don't build this yet. The pure-Go cost is high, the audience is narrow
(gocracker users on Windows hosts), and the alternative — pointing
Windows users at WSL2's KVM — already works. Revisit when there's
a concrete user pulling on it.

If we *do* build it, the path below is realistic. The work is large but
not technically risky: WHP is a stable public API and node-vmm's C++
implementation is a reference.

## What WHP is

Microsoft's hypervisor platform shipped as a public API on Windows 10+
Pro/Server with Hyper-V's hypervisor enabled. It is to Windows what KVM
is to Linux: a thin syscall surface for partitions (VMs), virtual
processors, and memory mapping. There is no built-in device emulation —
that is the VMM's job.

API headers: `WinHvPlatform.h`, `WinHvEmulation.h`. Functions are in
`WinHvPlatform.dll`.

## Surface in Go

Build tags split it cleanly:

```
//go:build windows
internal/whp/whp_windows.go    — syscall.LazyDLL bindings to WinHvPlatform
internal/whp/partition.go      — Partition handle + property setup
internal/whp/vcpu.go           — VirtualProcessor handle + RUN loop
internal/whp/memory.go         — gpa range mapping
pkg/vmm/arch_whp_windows.go    — VMM glue (mirrors arch_x86.go)
```

DLL binding via `syscall.NewLazyDLL("WinHvPlatform.dll")` — no cgo. The
partition handle is a `windows.Handle`. Each WHP function takes the
handle plus a property/struct pointer; tedious but mechanical.

## Component plan

### 1. Partition + memory mapping (~400 LoC)

- `WHvCreatePartition` → handle.
- `WHvSetPartitionProperty(WHvPartitionPropertyCodeProcessorCount, n)`
  for vCPUs.
- `WHvSetupPartition` to commit.
- For each memory region: `WHvMapGpaRange(handle, hostVA, gpa, size,
  access)`. Host VA comes from `windows.VirtualAlloc`.
- Dirty tracking: `WHvQueryGpaRangeDirtyBitmap` — same shape as KVM's
  `KVM_GET_DIRTY_LOG`, plug into the existing `pkg/vmm` dirty
  infrastructure.

### 2. Virtual processor + run loop (~600 LoC)

- `WHvCreateVirtualProcessor(handle, vcpuIdx, flags)`.
- `WHvSetVirtualProcessorRegisters` for CPUID, MSRs, segments — the
  shape mirrors KVM's `KVM_SET_*` calls; we already have working
  reference values in `pkg/vmm/arch_x86.go`.
- `WHvRunVirtualProcessor` returns an `WHV_RUN_VP_EXIT_CONTEXT`. The
  exit reasons we have to handle:
  - `MemoryAccess` — MMIO. Dispatch to virtio MMIO handlers.
  - `X64IoPortAccess` — port I/O. Dispatch to UART/i8042/PIC.
  - `UnrecoverableException`, `InvalidVpRegisterValue` — fatal.
  - `Halt` — cooperative halt; resume on virtio interrupt.
  - `Cpuid` — emulate against our CPUID table.
- Multi-vCPU: one goroutine per vCPU running its own
  `WHvRunVirtualProcessor`. The IRQ delivery path uses
  `WHvRequestInterrupt` which is thread-safe.

### 3. Device emulation reuse (~0 LoC new code)

This is the win — `internal/virtio/*`, `internal/uart`, `internal/i8042`,
`internal/loader` are all platform-neutral. Plug them into the WHP
exit-handler dispatch and they work as-is. virtio-net needs a backend;
on Windows, that backend is `internal/slirp` (TAP doesn't exist in the
Windows world).

### 4. Boot path (~300 LoC)

WHP doesn't load kernels — we do, same as we do for KVM. Reuse
`internal/loader` for the bzImage / ELF entry. The 64-bit transition
table (PML4/PDPT/PD) and IDT setup are identical to the KVM path. The
ACPI tables already live in `internal/acpi`.

### 5. Disk path

`internal/virtio/blk` works as-is. Backed by a regular Windows file
opened with `windows.CreateFile`. No special concerns.

### 6. Console

UART 16550A `internal/uart` works on Windows; the I/O handler routes the
serial bytes to a `*os.File` (stdin/stdout) just like Linux. No PTY
dependency since we don't have one on Windows.

## Effort estimate

- Partition + memory: ~1 week
- vCPU run loop + register state: ~2 weeks
- Boot path + ACPI plumbing: ~1 week
- Wire-up to `pkg/vmm`: ~1 week
- Tests + bring-up + boot Alpine: ~1 week

So ~6 weeks for one engineer to a "boots Alpine to a shell on Windows"
demo. Production-grade (rate limiters, balloon, vsock, snapshots) is
another 2–3 months.

## Snapshot/restore

Out of scope for the first cut. WHP supports VP register dump/restore
identical to KVM, but the snapshot file format in `pkg/vmm` assumes KVM
register layout. A pluggable backend interface around vCPU state would
be the right place to abstract.

## Networking

WHP has no native TAP equivalent. The slirp backend (this branch) is the
intended Windows NIC path.

## Why this is "do later"

1. The audience is small. gocracker's positioning is Linux/KVM-first.
2. The carrying cost is real — every change in `internal/virtio` etc.
   would need to keep a Windows path working in CI.
3. node-vmm exists and works on Windows; users with that need have a
   functioning option until gocracker is ready.

When the time comes, this doc plus node-vmm's `native/whp/backend.cc` is
enough to start. Open a tracking issue with this doc linked.
