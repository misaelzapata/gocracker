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