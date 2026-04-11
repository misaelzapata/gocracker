# Gocracker Performance Bottlenecks Analysis

## Critical Bottlenecks

### 1. Single VM Mutex (pkg/vmm/vmm.go:236)
- `VM.mu sync.Mutex` protects all state mutations
- Affects state reads, device updates, balloon operations
- `Pause()` runs 10ms polling with ~200 lock acquisitions
- **Fix:** Use RWMutex, atomic values for reads, separate device locks

### 2. Virtio Queue Lock During I/O (internal/virtio/virtio.go:132)
- `Queue.mu` held during entire descriptor iteration
- Forces serial processing of requests from same queue
- Blocks multi-queue parallelization
- **Fix:** Use finer-grained locking, consider lock-free structures

### 3. Memory Allocation Hotspots
- **Block I/O:** `buf := make([]byte, desc.Len)` per descriptor in blk.go:155
- **Network TX:** Fragment-level allocations + append copies in net.go:134
- **Network RX:** 65536 byte fixed allocation per packet, lock during delivery
- **Fix:** Implement sync.Pool for buffer recycling

### 4. Sequential Device Setup
- 8+ devices initialized sequentially during VM creation
- Each requires Transport creation, IRQ setup, device-specific init
- **Fix:** Parallelize independent device setup

### 5. Pause/Resume Polling (pkg/vmm/vmm.go:450)
- 10ms fixed polling interval in timeout loop
- 200 acquisitions of VM.mu during typical pause
- No event signaling - busy waiting
- **Fix:** Use condition variables/WaitGroup

## VM Startup Sequence

1. KVM system open
2. VM creation with RAM pre-allocation
3. Memory hotplug setup
4. Sequential device setup (RNG→Balloon→Net→vSock→Block→SharedFS)
5. Kernel loading (includes ACPI generation for all devices/vCPUs)
6. Sequential vCPU creation and setup

## I/O Flow Lock Points

**Block Device:**
1. HandleQueue (lock acquired)
2. IterAvail processes all descriptors (lock held)
3. Per-descriptor: allocate buffer, disk I/O, guest memory copy
4. PushUsedLocked (still locked)

**Network TX:**
1. HandleQueue (lock for entire transmit)
2. WalkChain (locked)
3. Fragment-level allocations + copies (locked)
4. TAP write (locked)

**Network RX:**
1. Dedicated rxPump goroutine polls TAP
2. rxQ.mu.Lock in deliverRXPacket
3. Single packet delivery at a time

## Optimization Priorities

**High (Latency/Throughput):**
- Buffer pooling in I/O paths
- Fine-grained queue locking
- Event-driven pause mechanism
- Atomic state reads

**Medium (Startup):**
- Parallel device initialization
- Pre-computed ACPI tables
- Cached rate limiter configs

**Low (Architectural):**
- Per-queue worker goroutines
- Separate device locks in VM struct
- Network TSO/GSO support

## Risks & Mitigations for Proposed Optimizations

### 1. `mmap MAP_PRIVATE` (Copy-on-Write) for Snapshots
- **Host OOM (Out of Memory) Risk:** With CoW, multiple VMs might appear to use very little RAM initially. If their memory states diverge heavily over time, physical RAM usage spikes unpredictably, potentially triggering the Linux OOM killer.
- **Page Fault Latency:** While boot time drops, the initial runtime performance may suffer slightly ("warm-up" time) because the guest will trigger host-side page faults the first time it touches unmapped memory regions.
- **Mitigation:** Implement strict memory limits, ballooning metrics, and robust host resource monitoring.

### 2. Parallel Device and vCPU Initialization
- **Data Races & Deadlocks:** KVM ioctls are mostly thread-safe, but the internal `VM` struct state must be safely locked. Since `VM.mu` is a single giant lock, parallelizing without refactoring the locking model first will cause deadlocks or panics.
- **Hidden Dependencies:** Some devices might implicitly rely on the initialization order (e.g., assigning available MMIO addresses or IRQ lines sequentially). Parallelizing requires deterministic pre-allocation of these resources.
- **Mitigation:** Pre-allocate MMIO bases and IRQs outside the initialization routines, adopt fine-grained locks or lock-free designs inside the `VM` struct, and heavily test with `go test -race`.

### 3. `sync.Pool` for I/O Buffers
- **Data Corruption:** The biggest risk with `sync.Pool` is putting a byte slice back into the pool while a background goroutine (like a network TAP write) is still reading it. This leads to data corruption in the network or disk streams.
- **Mitigation:** Ensure buffer lifecycle ownership is strictly enforced and thoroughly documented. Never pass pooled slices to decoupled asynchronous workers without copying or tracking references.

### 4. Virtio-fs + OverlayFS vs. Raw Ext4
- **Compatibility:** Docker layers use specific file system features (like whiteout files for deletions). `virtio-fs` bridging an `overlayfs` mount can sometimes have weird permission mapping or xattr behaviors compared to a raw `ext4` block device.
- **Metadata Performance:** While booting is faster, operations with heavy metadata I/O (e.g., `npm install` modifying thousands of files) can sometimes be slower over `virtio-fs` than to an in-guest `ext4` disk.
- **Mitigation:** Fall back to the ext4 compilation pipeline for write-heavy images or if overlayfs xattrs fail, keeping virtio-fs as a high-speed default for read-heavy or well-supported workloads.

### 5. Pre-computed ACPI / FDT Tables
- **Kernel Panics:** If a cached ACPI table is used but the guest is booted with a different topology (e.g., 2 vCPUs instead of 1, or more RAM), the guest kernel will read incorrect memory maps and trigger a kernel panic during boot. The table must perfectly match the VM shape.
- **Mitigation:** Implement a strict cache-key matching algorithm (hashing the exact vCPU count, memory size, and configured devices) to only load pre-computed tables when the configuration is a 1:1 match.

## Advanced Architecture (Beating Firecracker)

To truly exceed Firecracker's speed and security model, the architecture needs to move away from Go userspace bottlenecks and embrace deep Linux kernel integrations.

### 6. Data Plane Offloading (`io_uring` and `vhost-net`)
- **Speed Win:** Currently, `blk.go` and `net.go` use blocking `os.File` operations. Moving to `io_uring` for disk I/O provides batched, asynchronous execution. Integrating `vhost-net` drops the networking data plane entirely into the kernel, bypassing Go scheduling for packets.
- **Risks:** Vhost-net requires kernel modules and complicates the clean "one static binary" deployment. `io_uring` has complex lifecycle states in Go.
- **Mitigation:** Abstract the backend interface. Allow falling back to standard `os.File` reads if `vhost` or `io_uring` capabilities are missing on the host.

### 7. Strict CPU Core Affinity (Pinning)
- **Speed Win:** `runtime.LockOSThread()` stops Go from migrating the vCPU goroutine, but the Linux scheduler can still move the OS thread across physical cores. Using `unix.SchedSetaffinity` to bind the vCPU thread to a dedicated physical core eliminates cache thrashing and context-switch latency.
- **Risks:** Poorly implemented pinning can starve the host OS or create pathological performance if multiple VMs are mistakenly pinned to the same logical core.
- **Mitigation:** Implement a host-level CPU resource manager that tracks physical topology (`/proc/cpuinfo`) and explicitly hands out non-overlapping core masks.

### 8. Profile-Guided Optimization (PGO)
- **Speed Win:** Rebuilding gocracker with Go 1.2+'s Profile-Guided Optimization (`-pgo=auto`) against a CPU profile of a heavy KVM boot/I/O workload will auto-inline hot paths and reorganize the binary registry for extreme CPU efficiency.
- **Risks:** Stale profiles can actually regress performance across versions.
- **Mitigation:** Add an automated profiling step to the CI pipeline (`make pgo-profile`) that runs a heavy I/O benchmark and saves the resulting `default.pgo` before the final release build.

### 9. Per-Thread Seccomp and Linux Landlock
- **Security Win:** Firecracker applies distinct seccomp filters per system thread (the vCPU thread is blocked from opening files, while the block device thread can only read its specific FD). Combining per-thread seccomp filters with modern Linux Landlock (restricting global filesystem trees deeply into the kernel) beats legacy `chroot` jails.
- **Risks:** Unpredictable syscalls in future Go runtime upgrades might trigger seccomp kills. Complex thread-local BPF filtering is notoriously hard to manage in Go because of the M:N scheduler.
- **Mitigation:** Call the seccomp lockouts *after* explicitly calling `runtime.LockOSThread()` on the worker goroutines, and maintain a robust audit-logging mode (`SECCOMP_RET_LOG`) before enforcing strict `SECCOMP_RET_KILL`.

### 10. Rootless KVM Execution
- **Security Win:** Running `sudo gocracker` elevates the blast radius of a KVM escape. Using Linux User Namespaces and pre-allocating/chowning `/dev/kvm` and TAP interfaces before the VMM initializes allows `gocracker` to run completely as an unprivileged user.
- **Risks:** Setting up User Namespaces, unprivileged TAP/Bridge attachments, and resolving file ownership layers can degrade the "one-click" developer experience if the host isn't configured correctly.
- **Mitigation:** Ship a separate `gocracker-setup` binary (or command) that runs once as root to configure `udev` rules and `setcap` capabilities, allowing `gocracker run` to operate entirely unprivileged forever after.