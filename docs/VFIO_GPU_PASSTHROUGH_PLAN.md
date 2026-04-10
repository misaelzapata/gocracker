# VFIO GPU Passthrough Implementation Plan

## Overview

Add PCI bus emulation and VFIO device passthrough to gocracker, enabling GPU passthrough to microVMs. This allows running CUDA, ML training, and GPU-accelerated workloads inside gocracker VMs.

**Scope:** Full VFIO-PCI passthrough with MSI-X interrupts, DMA mapping, and BAR region mapping. Both x86-64 and ARM64 supported.

**Estimated effort:** ~2,500-3,500 lines of Go across 8-10 new files + modifications to 6 existing files.

**Reference implementations:**
- [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) — Rust VMM with full VFIO support
- [crosvm](https://chromium.googlesource.com/crosvm/crosvm/) — Chrome OS VMM with VFIO
- [Kata Containers + NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-kata.html)
- [Linux VFIO docs](https://docs.kernel.org/driver-api/vfio.html)

---

## Architecture

```
Guest VM
  |
  +-- PCI Config Space Access (ECAM MMIO or CF8/CFC I/O ports)
  |     |
  |     +-- Host Bridge (00:00.0) -- emulated
  |     +-- GPU (00:01.0) -- VFIO passthrough
  |     +-- GPU Audio (00:01.1) -- VFIO passthrough (same IOMMU group)
  |
  +-- GPU BAR0/BAR1/BAR3 -- mmap'd from VFIO device fd directly into guest PA
  |
  +-- MSI-X Table -- VMM-emulated (trapped MMIO writes)
  |     |
  |     +-- eventfd per vector
  |           |
  |           +-- VFIO kernel driver signals eventfd on physical interrupt
  |           +-- KVM injects MSI into guest via irqfd (no VMexit)
  |
  +-- DMA -- GPU reads/writes guest RAM via IOMMU (VFIO_IOMMU_MAP_DMA)
```

**Hot path (zero userspace involvement):**
1. GPU fires MSI-X interrupt -> VFIO kernel driver -> eventfd -> KVM irqfd -> guest IRQ
2. GPU DMA read/write -> IOMMU translates IOVA (=GPA) -> host physical RAM

---

## Phase 1: VFIO Kernel Interface (`internal/vfio/`)

### New file: `internal/vfio/vfio.go`

VFIO container, group, and device management.

#### VFIO Ioctl Constants

```go
// From include/uapi/linux/vfio.h
const (
    VFIO_GET_API_VERSION        = 0x3B64  // _IO(';', 100)
    VFIO_CHECK_EXTENSION        = 0x3B65  // _IO(';', 101)
    VFIO_SET_IOMMU              = 0x3B66  // _IO(';', 102)
    VFIO_GROUP_GET_STATUS       = 0x3B67  // _IO(';', 103)
    VFIO_GROUP_SET_CONTAINER    = 0x3B68  // _IO(';', 104)
    VFIO_GROUP_UNSET_CONTAINER  = 0x3B69  // _IO(';', 105)
    VFIO_GROUP_GET_DEVICE_FD    = 0x3B6A  // _IO(';', 106)
    VFIO_DEVICE_GET_INFO        = 0x3B6B  // _IO(';', 107)
    VFIO_DEVICE_GET_REGION_INFO = 0x3B6C  // _IO(';', 108)
    VFIO_DEVICE_GET_IRQ_INFO   = 0x3B6D  // _IO(';', 109)
    VFIO_DEVICE_SET_IRQS       = 0x3B6E  // _IO(';', 110)
    VFIO_DEVICE_RESET          = 0x3B6F  // _IO(';', 111)
    VFIO_IOMMU_MAP_DMA         = 0x3B71  // _IO(';', 113)
    VFIO_IOMMU_UNMAP_DMA       = 0x3B72  // _IO(';', 114)

    VFIO_TYPE1_IOMMU   = 1
    VFIO_TYPE1v2_IOMMU = 3
    VFIO_API_VERSION   = 0
)
```

#### Data Structures

```go
type GroupStatus struct {
    ArgSz uint32
    Flags uint32 // bit 0 = VIABLE, bit 1 = CONTAINER_SET
}

type DeviceInfo struct {
    ArgSz      uint32
    Flags      uint32 // bit 0 = RESET, bit 1 = PCI
    NumRegions uint32
    NumIRQs    uint32
    CapOffset  uint32
    Pad        uint32
}

type RegionInfo struct {
    ArgSz     uint32
    Flags     uint32 // bit 0 = READ, bit 1 = WRITE, bit 2 = MMAP, bit 3 = CAPS
    Index     uint32
    CapOffset uint32
    Size      uint64
    Offset    uint64 // mmap offset for this region on the device fd
}

type IRQInfo struct {
    ArgSz uint32
    Flags uint32 // bit 0 = EVENTFD, bit 1 = MASKABLE
    Index uint32
    Count uint32
}

type IRQSet struct {
    ArgSz uint32
    Flags uint32 // DATA_EVENTFD(4) | ACTION_TRIGGER(32)
    Index uint32
    Start uint32
    Count uint32
    // followed by int32[] eventfds
}

type DMAMap struct {
    ArgSz uint32
    Flags uint32 // READ(1) | WRITE(2)
    VAddr uint64 // host virtual address
    IOVA  uint64 // I/O virtual address (== guest physical address)
    Size  uint64
}
```

#### PCI Region & IRQ Indices

```go
const (
    PCI_BAR0   = 0
    PCI_BAR1   = 1
    PCI_BAR2   = 2
    PCI_BAR3   = 3
    PCI_BAR4   = 4
    PCI_BAR5   = 5
    PCI_ROM    = 6
    PCI_CONFIG = 7
    PCI_VGA    = 8

    PCI_INTX_IRQ  = 0
    PCI_MSI_IRQ   = 1
    PCI_MSIX_IRQ  = 2
    PCI_ERR_IRQ   = 3
    PCI_REQ_IRQ   = 4
)
```

#### API

```go
// Container wraps /dev/vfio/vfio
type Container struct {
    fd int
}

func NewContainer() (*Container, error)
func (c *Container) SetIOMMU(iommuType int) error
func (c *Container) MapDMA(hostAddr, guestAddr, size uint64) error
func (c *Container) UnmapDMA(guestAddr, size uint64) error
func (c *Container) Close() error

// Group wraps /dev/vfio/<group_id>
type Group struct {
    fd          int
    containerFd int
}

func OpenGroup(groupID int, container *Container) (*Group, error)
func (g *Group) GetDeviceFD(bdf string) (int, error)
func (g *Group) Close() error

// Device wraps a VFIO device fd
type Device struct {
    fd         int
    info       DeviceInfo
    regions    []RegionInfo
    irqs       []IRQInfo
    barMaps    [6][]byte       // mmap'd BAR regions
    bdf        string          // "0000:01:00.0"
}

func NewDevice(group *Group, bdf string) (*Device, error)
func (d *Device) Region(index int) *RegionInfo
func (d *Device) IRQ(index int) *IRQInfo
func (d *Device) MapBAR(index int) ([]byte, error)           // mmap BAR into host
func (d *Device) ReadConfig(offset, size int) (uint32, error) // read PCI config via VFIO
func (d *Device) WriteConfig(offset int, val uint32, size int) error
func (d *Device) SetupMSIX(eventfds []int) error             // VFIO_DEVICE_SET_IRQS
func (d *Device) DisableMSIX() error
func (d *Device) Reset() error
func (d *Device) Close() error
```

#### Initialization Sequence

```
1. container = NewContainer()                        // open /dev/vfio/vfio
2. group = OpenGroup(groupID, container)             // open /dev/vfio/<N>, attach to container
3. container.SetIOMMU(VFIO_TYPE1v2_IOMMU)            // enable IOMMU
4. container.MapDMA(hostVA, guestPA, ramSize)         // map ALL guest RAM for DMA
5. device = NewDevice(group, "0000:01:00.0")         // get device fd, query regions+IRQs
6. device.MapBAR(0)                                  // mmap BAR0 (GPU framebuffer)
7. device.MapBAR(1)                                  // mmap BAR1 (GPU registers)
8. device.MapBAR(3)                                  // mmap BAR3 (GPU I/O)
9. device.Reset()                                    // FLR the device
```

---

## Phase 2: PCI Bus Emulation (`internal/pci/`)

### New file: `internal/pci/config.go`

PCI configuration space (4096 bytes per device/function).

```go
const (
    ConfigSize     = 4096  // PCIe extended config space
    HeaderSize     = 64    // Standard PCI header
    FirstCapOffset = 0x40  // Where capabilities start
    MaxCapOffset   = 192   // Maximum capability chain end

    // Header offsets
    VendorID    = 0x00
    DeviceID    = 0x02
    Command     = 0x04
    Status      = 0x06
    ClassCode   = 0x08
    HeaderType  = 0x0E
    BAR0        = 0x10  // BAR0-BAR5: 0x10, 0x14, 0x18, 0x1C, 0x20, 0x24
    CapPtr      = 0x34
    IntLine     = 0x3C
    IntPin      = 0x3D

    // Command register bits
    CmdIOSpace     = 1 << 0
    CmdMemSpace    = 1 << 1
    CmdBusMaster   = 1 << 2
    CmdIntxDisable = 1 << 10

    // Status register bits
    StatusCapList = 1 << 4
)

type ConfigSpace struct {
    data         [ConfigSize]byte
    writeMask    [ConfigSize]byte  // which bits are writable
    barSizes     [6]uint64         // actual BAR sizes (for sizing protocol)
    barTypes     [6]uint8          // 0=unused, 1=mem32, 2=mem64, 3=io
}

func NewConfigSpace(vendorID, deviceID uint16, classCode uint32) *ConfigSpace
func (c *ConfigSpace) Read(offset, size int) uint32
func (c *ConfigSpace) Write(offset int, val uint32, size int)
func (c *ConfigSpace) AddCapability(capID byte, data []byte) int  // returns offset
func (c *ConfigSpace) SetBAR(index int, size uint64, barType uint8, prefetchable bool)
```

**BAR sizing protocol:** When guest writes 0xFFFFFFFF to a BAR, the VMM must return `~(size-1)` masked with type bits. On next write, the guest assigns the actual address.

### New file: `internal/pci/bus.go`

PCI bus with device enumeration and config space routing.

```go
const (
    MaxDevices   = 32
    MaxFunctions = 8
)

type BDF struct {
    Bus, Device, Function uint8
}

type PciDevice interface {
    ConfigRead(offset, size int) uint32
    ConfigWrite(offset, size int, val uint32)
    BARAddress(bar int) uint64      // guest physical address of this BAR
    BARSize(bar int) uint64
    BARMapped(bar int) []byte       // host mmap'd region (nil if not mappable)
    IOHandler() MMIOHandler         // handles trapped MMIO to non-mmap'd BAR pages
}

type MMIOHandler interface {
    HandleMMIO(addr uint64, data []byte, isWrite bool)
}

type Bus struct {
    devices [MaxDevices][MaxFunctions]PciDevice
    ecamBase uint64    // guest physical address of ECAM region
}

func NewBus(ecamBase uint64) *Bus
func (b *Bus) AddDevice(slot, function int, dev PciDevice) error
func (b *Bus) HandleECAM(addr uint64, data []byte, isWrite bool)  // ECAM MMIO access
func (b *Bus) HandleCF8CFC(port uint16, data []byte, isWrite bool) // x86 legacy IO
```

**ECAM address decoding:**
```
address = ecamBase + (bus << 20) | (device << 15) | (function << 12) | register_offset
```

### New file: `internal/pci/hostbridge.go`

Minimal PCI host bridge at 00:00.0.

```go
type HostBridge struct {
    config *ConfigSpace
}

func NewHostBridge() *HostBridge
// Vendor: 0x1B36 (Red Hat), Device: 0x0008 (PCIe Host Bridge)
// Class: 0x060000 (Host Bridge)
// Header Type: 0x00, no BARs, no capabilities
```

### New file: `internal/pci/msix.go`

MSI-X capability emulation + table shadow.

```go
const (
    CapIDMSIX = 0x11

    // MSI-X Message Control register bits
    MSIXEnable       = 1 << 15
    MSIXFunctionMask = 1 << 14
)

// MSI-X table entry (16 bytes per vector)
type MSIXEntry struct {
    AddrLo    uint32  // MSI address low
    AddrHi    uint32  // MSI address high
    Data      uint32  // MSI data
    VectorCtl uint32  // bit 0 = masked
}

type MSIXTable struct {
    entries   []MSIXEntry
    eventfds  []int         // one per vector
    gsis      []uint32      // KVM GSI number per vector
    enabled   bool
    barIndex  int           // which BAR contains the table
    barOffset uint64        // offset within that BAR
    pbaIndex  int           // which BAR contains PBA
    pbaOffset uint64
}

func NewMSIXTable(numVectors int, tableBAR int, tableOffset uint64, pbaBAR int, pbaOffset uint64) *MSIXTable
func (t *MSIXTable) CapabilityBytes() []byte                    // for PCI cap chain
func (t *MSIXTable) HandleRead(offset uint64, size int) uint32  // guest reads table
func (t *MSIXTable) HandleWrite(offset uint64, val uint32, size int) // guest writes table
func (t *MSIXTable) Enable(vm *kvm.VM) error                   // create eventfds + irqfds + GSI routing
func (t *MSIXTable) Disable(vm *kvm.VM) error                  // tear down
func (t *MSIXTable) UpdateRouting(vm *kvm.VM, vector int) error // re-route single vector
func (t *MSIXTable) Close()                                     // close eventfds
```

**Critical:** MSI-X table pages must NOT be directly mmap'd to guest. The VMM must trap writes to update KVM GSI routing when the guest programs MSI address/data.

**MSI-X → KVM routing:**
```go
// KVM_IRQ_ROUTING_MSI entry
type IRQRoutingMSI struct {
    AddressLo uint32
    AddressHi uint32
    Data      uint32
    Pad       uint32
}

// When guest writes to MSI-X table entry:
// 1. Update shadow entry
// 2. Rebuild GSI routing table with KVM_SET_GSI_ROUTING (type=KVM_IRQ_ROUTING_MSI)
// 3. Register eventfd with KVM_IRQFD for this GSI
// 4. Register eventfd with VFIO_DEVICE_SET_IRQS for this vector
```

---

## Phase 3: VFIO PCI Device (`internal/vfio/pci_device.go`)

Wraps a VFIO device as a PciDevice for the PCI bus.

```go
type VfioPciDevice struct {
    device    *Device        // VFIO device handle
    config    *ConfigSpace   // emulated config space (shadow)
    msix      *MSIXTable     // MSI-X emulation
    bars      [6]barMapping  // BAR guest address + mmap
    kvmVM     *kvm.VM        // for irqfd registration
    container *Container     // for DMA mapping
}

type barMapping struct {
    guestAddr uint64   // guest physical address
    size      uint64   // BAR size
    mmap      []byte   // host mmap (nil for MSI-X table pages)
    memSlot   int      // KVM memory region slot
}

func NewVfioPciDevice(device *Device, kvmVM *kvm.VM, container *Container) (*VfioPciDevice, error)
func (v *VfioPciDevice) ConfigRead(offset, size int) uint32
func (v *VfioPciDevice) ConfigWrite(offset, size int, val uint32)
func (v *VfioPciDevice) MapBARsToGuest(allocator func(size uint64, align uint64) uint64) error
func (v *VfioPciDevice) EnableMSIX() error
func (v *VfioPciDevice) Close() error
```

**Config space handling:**
- Most reads/writes proxied to VFIO device (region index 7)
- BAR reads return guest-assigned addresses (not physical)
- BAR writes trigger BAR sizing protocol
- Command register writes enable/disable memory space + bus master
- MSI-X capability writes handled by MSIXTable

**BAR mapping to guest:**
```go
// For each BAR:
// 1. Query size from VFIO_DEVICE_GET_REGION_INFO
// 2. Allocate guest physical address from memory map
// 3. mmap BAR from VFIO device fd
// 4. Register with KVM via KVM_SET_USER_MEMORY_REGION (slot N)
//    - guest_phys_addr = allocated address
//    - userspace_addr = mmap pointer
//    - memory_size = BAR size
// 5. For BARs containing MSI-X table: use sparse mmap
//    (skip the pages containing the MSI-X table)
```

---

## Phase 4: VMM Integration

### Modify: `pkg/vmm/vmm.go`

**New VM struct fields:**

```go
type VM struct {
    // ... existing fields ...
    pciBus      *pci.Bus          // PCI bus (nil if no PCI devices)
    vfioDevices []*vfio.VfioPciDevice  // VFIO passthrough devices
    vfioContainer *vfio.Container // VFIO IOMMU container
}
```

**New config fields:**

```go
type Config struct {
    // ... existing fields ...
    VFIODevices []VFIODeviceConfig `json:"vfio_devices,omitempty"`
}

type VFIODeviceConfig struct {
    BDF      string `json:"bdf"`       // "0000:01:00.0"
    GroupID  int    `json:"group_id"`  // IOMMU group number
    Slot     int    `json:"slot"`      // PCI slot in guest (auto if 0)
}
```

**MMIO dispatch extension in `handleMMIO()`:**

```go
func (m *VM) handleMMIO(vcpu *kvm.VCPU) {
    mmio := vcpu.GetMMIOData()

    // UART dispatch (existing, ARM64)
    // ...

    // PCI ECAM dispatch (new)
    if m.pciBus != nil {
        ecamBase := m.pciBus.ECAMBase()
        ecamSize := m.pciBus.ECAMSize()
        if mmio.PhysAddr >= ecamBase && mmio.PhysAddr < ecamBase+ecamSize {
            m.pciBus.HandleECAM(mmio.PhysAddr, mmio.Data[:mmio.Len], mmio.IsWrite == 1)
            return
        }
        // MSI-X table trap dispatch
        for _, dev := range m.vfioDevices {
            if dev.HandleTrappedMMIO(mmio.PhysAddr, mmio.Data[:mmio.Len], mmio.IsWrite == 1) {
                return
            }
        }
    }

    // Virtio transport dispatch (existing)
    // ...
}
```

**I/O port dispatch (x86 only) in `handleIO()`:**

```go
// PCI CF8/CFC config access (x86 legacy)
if m.pciBus != nil && (port == 0xCF8 || (port >= 0xCFC && port <= 0xCFF)) {
    m.pciBus.HandleCF8CFC(port, data, isWrite)
    return
}
```

**Cleanup in `cleanup()`:**

```go
for _, dev := range m.vfioDevices {
    dev.Close()
}
if m.vfioContainer != nil {
    m.vfioContainer.Close()
}
```

### Modify: `internal/acpi/acpi.go` (x86)

Add MCFG table for ECAM and PCI root bridge to DSDT.

```go
// New MCFG table
func buildMCFG(ecamBase uint64) []byte
// Returns ACPI MCFG table with one PCI segment (bus 0-0)

// Extend DSDT
func buildDSDT(mmio []MMIODevice, pci *PCIConfig) []byte
// Add \_SB.PCI0 device with:
//   _HID = "PNP0A08" (PCIe root bridge)
//   _CRS = Memory32Fixed(ecamBase, ecamSize) + QWordMemory(barRange)
//   _BBN = 0
```

### Modify: `internal/fdt/fdt.go` (ARM64)

Add PCI host bridge node to device tree.

```go
// In GenerateARM64(), when PCI devices are present:
b.beginNode(fmt.Sprintf("pci@%x", ecamBase))
b.propStringList("compatible", "pci-host-ecam-generic")
b.propStr("device_type", "pci")
b.propReg(ecamBase, ecamSize)
b.propU32("#address-cells", 3)
b.propU32("#size-cells", 2)
// ranges: 32-bit MMIO window for BARs
// interrupt-map: route to GIC ITS or MSI controller
// msi-parent: <&its> (GICv3 ITS for MSI-X)
b.endNode()
```

**ARM64 consideration:** GICv3 ITS (Interrupt Translation Service) is needed for MSI-X on ARM64. Graviton 1 does NOT have ITS (GICv2 only). GPU passthrough on ARM64 requires Graviton 2+ (GICv3 with ITS).

---

## Phase 5: CLI & API Integration

### CLI flags

```
gocracker run --image ubuntu:22.04 \
  --kernel ./kernel \
  --vfio 0000:01:00.0 \        # pass through this PCI device
  --vfio 0000:01:00.1 \        # and its audio controller
  --mem 16384 \                 # GPUs need more RAM
  --disk 50000
```

### API endpoint

```
PUT /vfio-devices/{bdf}
{
    "group_id": 1,
    "guest_slot": 1
}
```

### Host preparation script

```bash
# tools/prepare-gpu-passthrough.sh
# 1. Unbind GPU from host driver
echo "0000:01:00.0" > /sys/bus/pci/devices/0000:01:00.0/driver/unbind
# 2. Bind to vfio-pci
echo "vfio-pci" > /sys/bus/pci/devices/0000:01:00.0/driver_override
echo "0000:01:00.0" > /sys/bus/pci/drivers/vfio-pci/bind
# 3. Verify
ls -la /dev/vfio/
```

---

## Memory Layout

### x86-64 with PCI

```
0x00000000 - 0x000FFFFF    Low memory (legacy, boot params, page tables)
0x00100000 - 0x0FFFFFFF    Kernel + RAM (low 256 MB)
0x10000000 - 0xBFFFFFFF    RAM continued
0xC0000000 - 0xDFFFFFFF    PCI 32-bit BAR space (512 MB)
0xE0000000 - 0xEFFFFFFF    PCI ECAM (256 MB, 1 segment)
0xD0000000 - 0xD000FFFF    Virtio MMIO (existing, adjust if conflict)
0xFEE00000 - 0xFEEFFFFF    LAPIC (existing)

Above 4GB:
0x100000000+               RAM above 4 GB
0x800000000000+            PCI 64-bit BAR space (for large GPU VRAM)
```

### ARM64 with PCI

```
0x00000000 - 0x3BFFFFFF    PCI ECAM + 32-bit BAR space
0x3C000000 - 0x3CFFFFFF    PCI ECAM (16 MB)
0x3D000000 - 0x3FFFFFFF    PCI 32-bit BARs (48 MB, small devices)
0x3FFFD000 - 0x3FFFFFFF    GIC (existing)
0x40000000 - 0x40002FFF    Boot/RTC/UART (existing)
0x40003000 - 0x4000FFFF    Virtio MMIO (existing)
0x80000000 - 0xFFFFFFFF    Guest RAM (existing)

Above 4GB:
0x100000000+               PCI 64-bit BAR space (for GPU VRAM)
```

---

## NVIDIA GPU Quirks & Considerations

### Known Issues

1. **Function Level Reset (FLR) timeout:** NVIDIA GPUs may take >1s to complete FLR. The VMM should retry with exponential backoff, not fail immediately.

2. **Config space corruption after failed reset:** Header type reads 0x7F, all bytes 0xFF. Use `vendor-reset` kernel module (`github.com/gnif/vendor-reset`) for hardware-specific reset sequences.

3. **IOMMU group isolation:** GPU + audio controller typically share an IOMMU group. Both must be passed through together. Check with:
   ```bash
   ls /sys/kernel/iommu_groups/<N>/devices/
   ```

4. **ROM BAR:** The GPU marked as `boot_vga` has a BIOS-modified ROM. VFIO may reject it. Use `rombar=0` or provide a clean ROM dump.

5. **Large VRAM BARs:** GPUs with 16GB+ VRAM need 64-bit BARs and adequate MMIO64 window in the guest memory map. Ensure `phys-bits=host` equivalent in KVM setup.

6. **Driver compatibility:** Guest must have matching NVIDIA driver version. Container images with pre-installed NVIDIA drivers (e.g., `nvidia/cuda:12.x`) work best.

### ARM64 Limitations

- **Graviton 1 (a1.metal):** GICv2 only, no ITS, no MSI-X support. GPU passthrough NOT possible.
- **Graviton 2+ (m6g, c6g, etc.):** GICv3 with ITS. GPU passthrough possible but AWS does not expose VFIO to metal instances for GPU.
- **Practical ARM64 GPU:** Ampere Altra (Oracle Cloud A1), NVIDIA Grace Hopper (GH200).

---

## Testing Strategy

### Unit Tests (no hardware)

```
internal/pci/config_test.go      - BAR sizing, capability chain, read/write
internal/pci/bus_test.go         - ECAM decode, CF8/CFC decode, device enumeration
internal/pci/msix_test.go        - MSI-X table read/write, enable/disable
internal/vfio/vfio_test.go       - Struct layout verification (sizes match kernel)
```

### Integration Tests (requires VFIO hardware)

```
tests/integration/vfio_test.go   - Full GPU passthrough boot test
```

Test sequence:
1. Bind test device to vfio-pci
2. Boot VM with `--vfio <bdf>`
3. Verify `lspci` inside guest shows the device
4. Run `nvidia-smi` (for NVIDIA GPU)
5. Run a CUDA sample
6. Shutdown cleanly

### Manual Smoke Test

```bash
# Prepare GPU
sudo tools/prepare-gpu-passthrough.sh 0000:01:00.0

# Boot with GPU
sudo gocracker run \
    --image nvidia/cuda:12.4.0-runtime-ubuntu22.04 \
    --kernel artifacts/kernels/gocracker-guest-standard-vmlinux.gz \
    --vfio 0000:01:00.0 \
    --mem 16384 --disk 50000 \
    --cmd 'nvidia-smi'
```

---

## Implementation Order

| Phase | What | New Files | Modified Files | LOC |
|-------|------|-----------|----------------|-----|
| 1 | VFIO kernel interface | `internal/vfio/vfio.go` | — | ~400 |
| 2a | PCI config space | `internal/pci/config.go` | — | ~300 |
| 2b | PCI bus + ECAM | `internal/pci/bus.go` | — | ~250 |
| 2c | Host bridge | `internal/pci/hostbridge.go` | — | ~50 |
| 2d | MSI-X emulation | `internal/pci/msix.go` | `internal/kvm/kvm.go` | ~400 |
| 3 | VFIO PCI device | `internal/vfio/pci_device.go` | — | ~500 |
| 4a | VMM integration | — | `pkg/vmm/vmm.go`, `pkg/vmm/arch_x86.go`, `pkg/vmm/arch_arm64.go` | ~300 |
| 4b | ACPI (x86) | — | `internal/acpi/acpi.go`, `internal/acpi/aml.go` | ~150 |
| 4c | FDT (ARM64) | — | `internal/fdt/fdt.go` | ~100 |
| 5 | CLI + API | — | `cmd/gocracker/main.go`, `internal/api/api.go` | ~150 |
| 6 | Host prep tool | `tools/prepare-gpu-passthrough.sh` | — | ~80 |
| — | Tests | `*_test.go` files | — | ~400 |
| **Total** | | **~8 new files** | **~8 modified** | **~3,100** |

---

## Dependencies

- No new Go modules needed. Uses `golang.org/x/sys/unix` (already imported) for ioctls and mmap.
- Host kernel must have `vfio-pci` module loaded and IOMMU enabled (`intel_iommu=on` or `amd_iommu=on` for x86, SMMU for ARM64).
- Guest kernel needs PCI + GPU driver support (standard Ubuntu/Debian kernels have this).

---

## References

| Resource | URL |
|----------|-----|
| Linux VFIO header | `include/uapi/linux/vfio.h` in kernel source |
| Linux VFIO docs | https://docs.kernel.org/driver-api/vfio.html |
| Cloud Hypervisor VFIO | https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vfio.md |
| Cloud Hypervisor vfio_pci.rs | https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vfio/src/vfio_pci.rs |
| Cloud Hypervisor PCI config | https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/pci/src/configuration.rs |
| crosvm VFIO | https://chromium.googlesource.com/crosvm/crosvm/+/refs/heads/main/devices/src/pci/vfio_pci.rs |
| ixy VFIO driver (C) | https://github.com/emmericp/ixy/blob/master/src/libixy-vfio.c |
| vendor-reset (NVIDIA/AMD) | https://github.com/gnif/vendor-reset |
| NVIDIA GPU Operator + Kata | https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-kata.html |
| KVM API docs | https://docs.kernel.org/virt/kvm/api.html |
| PCIe spec (public summary) | https://pcisig.com/specifications |
| MSI-X spec | PCI Local Bus Specification 3.0, Chapter 6.8.2 |
