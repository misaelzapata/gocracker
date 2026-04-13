// Package fdt generates a minimal Flattened Device Tree (DTB) blob
// that tells the Linux kernel where the virtio-mmio devices live.
//
// We write a hand-crafted DTB without external dependencies.
// Spec: https://devicetree-specification.readthedocs.io/
package fdt

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/gocracker/gocracker/internal/arm64layout"
)

// DTB magic and version
const (
	fdtMagic             = 0xD00DFEED
	fdtVersion           = 17
	fdtLastCompatVersion = 16

	// Token types
	tokenBeginNode = 0x00000001
	tokenEndNode   = 0x00000002
	tokenProp      = 0x00000003
	tokenNop       = 0x00000004
	tokenEnd       = 0x00000009
)

// VirtioDevice describes one virtio-mmio device to include in the DTB.
type VirtioDevice struct {
	BaseAddr uint64
	Size     uint64
	IRQ      uint32
}

type ARM64Config struct {
	MemBase       uint64
	MemBytes      uint64
	CPUs          int
	Cmdline       string
	InitrdAddr    uint64
	InitrdSize    uint64
	GIC           arm64layout.GICLayout
	VirtioDevices []VirtioDevice
}

// ARM64 memory layout constants — aligned with Firecracker's aarch64 layout.
// Firecracker uses DRAM at 0x80000000, MMIO devices in the 0x40000000 region,
// and the GIC below the MMIO32 start.
const (
	DefaultARM64MemoryBase = 0x80000000 // DRAM base (Firecracker: DRAM_MEM_START)
	DefaultARM64SystemSize = 0x00200000 // 2 MiB reserved for kernel/DTB overhead

	DefaultARM64MMIO32Start = 0x40000000 // MMIO32 region start

	// Firecracker reserves the serial MMIO slot at 0x40002000. gocracker
	// currently exposes an ns16550a UART at that address, while Firecracker's
	// own AArch64 guests use PL011/ttyAMA0 there.
	DefaultARM64PL011Base = 0x40002000
	DefaultARM64PL011Size = 0x00001000
	DefaultARM64PL011IRQ  = 1 // SPI 1, INTID 33

	// GIC addresses — Firecracker computes these relative to MMIO32_START.
	// GICv2: GICD at MMIO32-0x1000, GICC at GICD-0x2000.
	// GICv3: GICD at MMIO32-0x10000, GICR at GICD-vcpu*0x20000.
	// We use GICv2 values as defaults (Graviton 1 compatibility).
	DefaultARM64GICDBase = 0x3FFFF000 // Firecracker GICv2: MMIO32-0x1000
	DefaultARM64GICDSize = 0x00001000 // GICv2 dist size
	DefaultARM64GICRBase = 0x3FFFD000 // GICv2 CPU interface: GICD-0x2000
	DefaultARM64GICRSize = 0x00002000 // GICv2 CPU interface size

	DefaultARM64GICPhandle = 1
)

// Builder assembles the DTB.
type Builder struct {
	strings []byte
	struct_ []byte
	strIdx  map[string]uint32
}

func newBuilder() *Builder {
	return &Builder{strIdx: make(map[string]uint32)}
}

func (b *Builder) addString(s string) uint32 {
	if idx, ok := b.strIdx[s]; ok {
		return idx
	}
	idx := uint32(len(b.strings))
	b.strings = append(b.strings, []byte(s)...)
	b.strings = append(b.strings, 0)
	b.strIdx[s] = idx
	return idx
}

func (b *Builder) beginNode(name string) {
	b.putU32(tokenBeginNode)
	n := []byte(name)
	n = append(n, 0)
	// pad to 4 bytes
	for len(n)%4 != 0 {
		n = append(n, 0)
	}
	b.struct_ = append(b.struct_, n...)
}

func (b *Builder) endNode() { b.putU32(tokenEndNode) }

func (b *Builder) prop(name string, val []byte) {
	b.putU32(tokenProp)
	b.putU32(uint32(len(val)))
	b.putU32(b.addString(name))
	b.struct_ = append(b.struct_, val...)
	// pad
	for len(b.struct_)%4 != 0 {
		b.struct_ = append(b.struct_, 0)
	}
}

func (b *Builder) propEmpty(name string) {
	b.prop(name, nil)
}

func (b *Builder) propU32(name string, v uint32) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	b.prop(name, buf[:])
}

func (b *Builder) propU64(name string, v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	b.prop(name, buf[:])
}

func (b *Builder) propU32s(name string, vals ...uint32) {
	buf := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.BigEndian.PutUint32(buf[i*4:], v)
	}
	b.prop(name, buf)
}

func (b *Builder) propU64s(name string, vals ...uint64) {
	buf := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.BigEndian.PutUint64(buf[i*8:], v)
	}
	b.prop(name, buf)
}

func (b *Builder) propStr(name, s string) {
	b.prop(name, append([]byte(s), 0))
}

func (b *Builder) propStrList(name string, strs ...string) {
	var buf []byte
	for _, s := range strs {
		buf = append(buf, []byte(s)...)
		buf = append(buf, 0)
	}
	b.prop(name, buf)
}

func (b *Builder) putU32(v uint32) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	b.struct_ = append(b.struct_, buf[:]...)
}

func (b *Builder) putU64(v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	b.struct_ = append(b.struct_, buf[:]...)
}

func (b *Builder) propReg(addr, size uint64) {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:], addr)
	binary.BigEndian.PutUint64(buf[8:], size)
	b.prop("reg", buf[:])
}

func (b *Builder) propRegPairs(name string, pairs ...[2]uint64) {
	buf := make([]byte, 16*len(pairs))
	for i, pair := range pairs {
		binary.BigEndian.PutUint64(buf[i*16:], pair[0])
		binary.BigEndian.PutUint64(buf[i*16+8:], pair[1])
	}
	b.prop(name, buf)
}

func (b *Builder) propInterrupt(irq uint32) {
	// GIC interrupt cell format: <type irq flags>
	// type 0 = SPI, flags 4 = level-high
	var buf [12]byte
	binary.BigEndian.PutUint32(buf[0:], 0)      // SPI
	binary.BigEndian.PutUint32(buf[4:], irq-32) // GIC offset
	binary.BigEndian.PutUint32(buf[8:], 4)      // level high
	b.prop("interrupts", buf[:])
}

// propGICInterrupt encodes an SPI interrupt for the GIC in the DTB.
// Firecracker uses IRQ_TYPE_EDGE_RISING (1) for all device interrupts.
func (b *Builder) propGICInterrupt(spi uint32) {
	const irqTypeEdgeRising = 1 // matches Firecracker
	b.propU32s("interrupts", 0, spi, irqTypeEdgeRising)
}

// Generate builds a complete DTB blob for the given virtio devices.
// memBytes is the total guest RAM size.
// Returns the DTB bytes and the guest-physical address it should be placed at.
func Generate(memBytes uint64, cpus int, devices []VirtioDevice) ([]byte, error) {
	b := newBuilder()

	// root node
	b.beginNode("")
	b.propU32("#address-cells", 2)
	b.propU32("#size-cells", 2)
	b.propStr("compatible", "linux,dummy-virt")

	// /chosen — kernel cmdline placeholder (actual cmdline via boot_params)
	b.beginNode("chosen")
	b.propStr("stdout-path", "/uart@3f8")
	b.endNode()

	// /memory
	b.beginNode("memory")
	b.propStr("device_type", "memory")
	{
		var buf [16]byte
		binary.BigEndian.PutUint64(buf[0:], 0) // base
		binary.BigEndian.PutUint64(buf[8:], memBytes)
		b.prop("reg", buf[:])
	}
	b.endNode()

	// /cpus
	b.beginNode("cpus")
	b.propU32("#address-cells", 1)
	b.propU32("#size-cells", 0)
	for i := 0; i < cpus; i++ {
		b.beginNode(fmt.Sprintf("cpu@%d", i))
		b.propStr("device_type", "cpu")
		b.propStrList("compatible", "arm,cortex-a57", "arm,armv8")
		b.propStr("enable-method", "psci")
		b.propU32("reg", uint32(i))
		b.endNode()
	}
	b.endNode()

	// /uart (16550 at 0x3F8)
	b.beginNode("uart@3f8")
	b.propStrList("compatible", "ns16550a")
	b.propReg(0x3F8, 8)
	b.propU32("clock-frequency", 1843200)
	b.propU32("current-speed", 115200)
	b.propU32("reg-shift", 0)
	b.propU32("reg-io-width", 1)
	b.endNode()

	// virtio-mmio devices
	for i, dev := range devices {
		name := fmt.Sprintf("virtio_mmio@%x", dev.BaseAddr)
		b.beginNode(name)
		b.propStr("compatible", "virtio,mmio")
		b.propReg(dev.BaseAddr, dev.Size)
		{
			var buf [4]byte
			binary.BigEndian.PutUint32(buf[:], dev.IRQ)
			b.prop("interrupts", buf[:])
		}
		b.propU32("interrupt-parent", uint32(i+1))
		b.endNode()
	}

	b.endNode() // close root node
	b.putU32(tokenEnd)

	return b.buildDTB(), nil
}

// GenerateARM64 builds a DTB suitable for an arm64 Linux guest booting with a
// DTB, PSCI, a probed GIC layout, the current gocracker serial device model,
// and virtio-mmio devices.
func GenerateARM64(cfg ARM64Config) ([]byte, error) {
	if cfg.MemBase == 0 {
		cfg.MemBase = DefaultARM64MemoryBase
	}
	if cfg.MemBytes == 0 {
		return nil, fmt.Errorf("arm64 dtb requires a positive memory size")
	}
	if cfg.CPUs <= 0 {
		cfg.CPUs = 1
	}
	if !cfg.GIC.Valid() {
		cfg.GIC = arm64layout.GICv2()
	}

	b := newBuilder()
	b.beginNode("")
	b.propU32("#address-cells", 2)
	b.propU32("#size-cells", 2)
	b.propStrList("compatible", "gocracker,arm64-virt", "linux,dummy-virt")

	serialNodeName := fmt.Sprintf("uart@%x", DefaultARM64PL011Base)
	b.beginNode("chosen")
	b.propStr("stdout-path", "/"+serialNodeName)
	if cfg.Cmdline != "" {
		b.propStr("bootargs", cfg.Cmdline)
	}
	if cfg.InitrdAddr != 0 && cfg.InitrdSize != 0 {
		b.propU64("linux,initrd-start", cfg.InitrdAddr)
		b.propU64("linux,initrd-end", cfg.InitrdAddr+cfg.InitrdSize)
	}
	b.endNode()

	b.beginNode("aliases")
	b.propStr("serial0", "/"+serialNodeName)
	b.endNode()

	b.beginNode(fmt.Sprintf("memory@%x", cfg.MemBase))
	b.propStr("device_type", "memory")
	b.propReg(cfg.MemBase, cfg.MemBytes)
	b.endNode()

	b.beginNode("cpus")
	b.propU32("#address-cells", 2)
	b.propU32("#size-cells", 0)
	for i := 0; i < cfg.CPUs; i++ {
		b.beginNode(fmt.Sprintf("cpu@%x", i))
		b.propStr("device_type", "cpu")
		b.propStrList("compatible", "arm,arm-v8")
		b.propStr("enable-method", "psci")
		b.propU64("reg", uint64(i))
		b.endNode()
	}
	b.endNode()

	b.beginNode("psci")
	b.propStrList("compatible", "arm,psci-1.0", "arm,psci-0.2")
	b.propStr("method", "hvc")
	b.endNode()

	b.beginNode("intc")
	b.propStrList("compatible", cfg.GIC.Compat)
	b.propEmpty("interrupt-controller")
	b.propU32("#interrupt-cells", 3)
	b.propU32("#address-cells", 2)
	b.propU32("#size-cells", 2)
	b.propU32("phandle", DefaultARM64GICPhandle)
	b.propRegPairs("reg",
		[2]uint64{cfg.GIC.Properties[0], cfg.GIC.Properties[1]},
		[2]uint64{cfg.GIC.Properties[2], cfg.GIC.Properties[3]},
	)
	b.propEmpty("ranges")
	b.propU32s("interrupts", 1, cfg.GIC.MaintIRQ, 4)
	b.endNode()

	// Timer node (Firecracker: create_timer_node).
	const (
		gicIRQTypePPI  = 1
		irqTypeLevelHi = 4
		clockPhandle   = 2
	)
	b.beginNode("timer")
	b.propStr("compatible", "arm,armv8-timer")
	b.propEmpty("always-on")
	b.propU32("interrupt-parent", DefaultARM64GICPhandle)
	b.propU32s("interrupts",
		gicIRQTypePPI, 13, irqTypeLevelHi,
		gicIRQTypePPI, 14, irqTypeLevelHi,
		gicIRQTypePPI, 11, irqTypeLevelHi,
		gicIRQTypePPI, 10, irqTypeLevelHi,
	)
	b.endNode()

	// APB clock node (Firecracker: create_clock_node).
	b.beginNode("apb-pclk")
	b.propStr("compatible", "fixed-clock")
	b.propU32("#clock-cells", 0)
	b.propU32("clock-frequency", 24000000)
	b.propStr("clock-output-names", "clk24mhz")
	b.propU32("phandle", clockPhandle)
	b.endNode()

	// gocracker currently uses an ns16550a UART in Firecracker's serial MMIO
	// slot so the guest console stays on ttyS0.
	b.beginNode(fmt.Sprintf("uart@%x", DefaultARM64PL011Base))
	b.propStr("compatible", "ns16550a")
	b.propReg(DefaultARM64PL011Base, DefaultARM64PL011Size)
	b.propU32("clocks", clockPhandle)
	b.propStr("clock-names", "apb_pclk")
	b.propU32("interrupt-parent", DefaultARM64GICPhandle)
	b.propGICInterrupt(DefaultARM64PL011IRQ)
	b.endNode()

	for _, dev := range cfg.VirtioDevices {
		b.beginNode(fmt.Sprintf("virtio_mmio@%x", dev.BaseAddr))
		b.propStr("compatible", "virtio,mmio")
		b.propReg(dev.BaseAddr, dev.Size)
		b.propEmpty("dma-coherent") // required on ARM64 (Firecracker sets this)
		b.propU32("interrupt-parent", DefaultARM64GICPhandle)
		b.propGICInterrupt(dev.IRQ)
		b.endNode()
	}

	b.endNode() // close root node
	b.putU32(tokenEnd)
	return b.buildDTB(), nil
}

func ContainsNodeName(dtb []byte, name string) bool {
	return bytes.Contains(dtb, append([]byte(name), 0))
}

// buildDTB assembles the final DTB binary.
func (b *Builder) buildDTB() []byte {
	const headerSize = 40
	// Memory reservation map: one empty entry (16 bytes of zeros) terminates it.
	const memRsvMapSize = 16

	memRsvMapOff := uint32(headerSize)
	structOff := memRsvMapOff + memRsvMapSize
	strOff := structOff + uint32(len(b.struct_))
	totalSize := strOff + uint32(len(b.strings))
	// align total to 8 bytes
	for totalSize%8 != 0 {
		totalSize++
		b.strings = append(b.strings, 0)
	}

	var hdr [headerSize]byte
	binary.BigEndian.PutUint32(hdr[0:], fdtMagic)
	binary.BigEndian.PutUint32(hdr[4:], totalSize)
	binary.BigEndian.PutUint32(hdr[8:], structOff)     // off_dt_struct
	binary.BigEndian.PutUint32(hdr[12:], strOff)       // off_dt_strings
	binary.BigEndian.PutUint32(hdr[16:], memRsvMapOff) // off_mem_rsvmap
	binary.BigEndian.PutUint32(hdr[20:], fdtVersion)
	binary.BigEndian.PutUint32(hdr[24:], fdtLastCompatVersion)
	binary.BigEndian.PutUint32(hdr[28:], 0) // boot_cpuid_phys
	binary.BigEndian.PutUint32(hdr[32:], uint32(len(b.strings)))
	binary.BigEndian.PutUint32(hdr[36:], uint32(len(b.struct_)))

	out := make([]byte, 0, totalSize)
	out = append(out, hdr[:]...)
	out = append(out, make([]byte, memRsvMapSize)...) // empty reservation map
	out = append(out, b.struct_...)
	out = append(out, b.strings...)
	return out
}
