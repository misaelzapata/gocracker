//go:build windows

package vmm

import "sync"

// pciConfigDummy is a no-device implementation of the legacy x86 PCI
// Configuration Mechanism #1 (ports 0xCF8 + 0xCFC). It exists purely
// to silence the Linux kernel's PCI bus enumeration on a machine that
// has no PCI fabric: every config-space read returns 0xFFFFFFFF — the
// PCI spec's "no device present" sentinel — so Linux walks the (bus,
// dev, fn) space, sees vendor ID 0xFFFF everywhere, and gives up
// without complaining.
//
// Wire format reminder (PCI Local Bus 2.3 §3.2.2):
//   - 0xCF8..0xCFB (4 bytes) = CONFIG_ADDRESS register (little-endian
//     dword). Bit 31 = enable, 30-24 = reserved, 23-16 = bus,
//     15-11 = device, 10-8 = function, 7-2 = register, 1-0 = byte.
//   - 0xCFC..0xCFF (4 bytes) = CONFIG_DATA window. A read returns the
//     32-bit register selected by CONFIG_ADDRESS; byte-granular reads
//     pull the corresponding byte of that dword.
//
// What this DOES implement:
//   - The 32-bit address register stores the last write byte-by-byte
//     so the guest sees a round-tripped value when it reads back.
//   - Every byte read from CONFIG_DATA returns 0xFF, regardless of
//     bus/dev/fn/reg. This means insl reads of 0xCFC come back as
//     0xFFFFFFFF, which Linux interprets as "no device".
//
// What it does NOT implement:
//   - Any actual PCI devices. If you want real PCI emulation, replace
//     this with a per-(bus, dev, fn) config-space router.
//   - Mechanism #2 (deprecated, never used by modern Linux).
//   - ECAM / MMCONFIG MMIO config space — that's a separate concern.
type pciConfigDummy struct {
	mu   sync.Mutex
	addr uint32 // last value written to CONFIG_ADDRESS (0xCF8..0xCFB)
}

func newPCIConfigDummy() *pciConfigDummy {
	return &pciConfigDummy{}
}

// handles reports whether the I/O port belongs to PCI Mechanism #1.
// The range is 0xCF8..0xCFF inclusive: 4 bytes of address register
// followed by 4 bytes of data window.
func (p *pciConfigDummy) handles(port uint16) bool {
	return port >= 0xCF8 && port <= 0xCFF
}

// readPort returns the byte the guest reads from a PCI config port.
// The kernel typically performs a 32-bit insl on 0xCFC, which the
// VMM decomposes into 4 sequential byte reads of 0xCFC, 0xCFD, 0xCFE,
// 0xCFF — each of which returns 0xFF here, yielding 0xFFFFFFFF.
// Reads of the address register return whatever the guest last wrote
// (little-endian byte view of p.addr).
func (p *pciConfigDummy) readPort(port uint16) byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case port >= 0xCF8 && port <= 0xCFB:
		shift := uint(port-0xCF8) * 8
		return byte(p.addr >> shift)
	case port >= 0xCFC && port <= 0xCFF:
		// Every byte of every config register on every (bus, dev, fn)
		// reads as 0xFF — Linux sees vendor=0xFFFF and skips.
		return 0xFF
	}
	return 0xFF
}

// writePort handles a guest write to a PCI config port. Writes to the
// address register are stashed so subsequent reads round-trip. Writes
// to the data window are silently dropped — we have no devices to
// configure.
func (p *pciConfigDummy) writePort(port uint16, value byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case port >= 0xCF8 && port <= 0xCFB:
		shift := uint(port-0xCF8) * 8
		mask := uint32(0xFF) << shift
		p.addr = (p.addr &^ mask) | (uint32(value) << shift)
	case port >= 0xCFC && port <= 0xCFF:
		// No devices, nowhere to write. Drop on the floor.
	}
}
