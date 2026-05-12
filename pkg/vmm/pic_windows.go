//go:build windows

package vmm

import "sync"

// pic8259 is a minimal pair of chained 8259A Programmable Interrupt
// Controllers (master + slave). Linux uses this for legacy IRQ delivery
// when the APIC is unavailable.
//
// Ported from node-vmm's native/whp/devices/pic.cc (Misael Zapata),
// which proved the same wire-format works on Windows Hyper-V partitions.
//
// What this DOES implement:
//   - ICW1-ICW4 init sequence (port 0x20/0xA0 command + 0x21/0xA1 data)
//   - IMR (interrupt mask register) read/write
//   - request_irq(irq) bridge: if the IRQ is unmasked, call out to
//     whp.RequestFixedInterrupt to deliver the corresponding vector
//
// What it does NOT yet implement:
//   - ISR/IRR readback via OCW3 (kernel reads 0)
//   - Specific EOI / cascade modes (any EOI is accepted but ignored)
//   - Polled mode (port 0x20 read returns 0)
//   - Edge/level trigger mode register (port 0x4D0/0x4D1)
//
// The 8259 init sequence (ICW1-ICW4) the kernel sends:
//   1. Write 0x11 to command port (ICW1: cascade, ICW4 needed)
//   2. Write vector base to data port (ICW2: master usually 0x20, slave 0x28)
//   3. Write cascade info to data port (ICW3: master 0x04, slave 0x02)
//   4. Write 0x01 to data port (ICW4: 8086 mode)
//   5. From here on, data-port writes set the IMR (mask).
type pic8259 struct {
	mu     sync.Mutex
	master picController
	slave  picController
}

type picController struct {
	vector   uint8 // ICW2 — vector base (master 0x20, slave 0x28 typically)
	mask     uint8 // IMR — 1 = masked
	initStep uint8 // 0 = no init in progress; 2/3/4 = expected next ICW
}

func newPIC8259() *pic8259 {
	return &pic8259{
		master: picController{mask: 0xFF},
		slave:  picController{mask: 0xFF},
	}
}

// handles reports whether the given I/O port is owned by the PIC.
func (p *pic8259) handles(port uint16) bool {
	return port == 0x20 || port == 0x21 || port == 0xA0 || port == 0xA1
}

// readPort returns the byte the guest reads from a PIC port.
func (p *pic8259) readPort(port uint16) byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch port {
	case 0x21:
		return p.master.mask
	case 0xA1:
		return p.slave.mask
	}
	return 0
}

// writePort handles a guest write to a PIC port.
func (p *pic8259) writePort(port uint16, value byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch port {
	case 0x20:
		p.writeCommand(&p.master, value)
	case 0x21:
		p.writeData(&p.master, value)
	case 0xA0:
		p.writeCommand(&p.slave, value)
	case 0xA1:
		p.writeData(&p.slave, value)
	}
}

func (p *pic8259) writeCommand(c *picController, value byte) {
	// ICW1 has bit 4 set. Bit 0 = ICW4 needed (we expect it). Bit 1 =
	// single-PIC mode (we don't honour it). Bit 3 = level-triggered
	// (we don't honour it).
	if value&0x10 != 0 {
		c.initStep = 2
		c.mask = 0xFF
		return
	}
	// EOI / OCW2 / OCW3 commands — we accept and ignore. A real PIC
	// would update ISR here.
}

func (p *pic8259) writeData(c *picController, value byte) {
	switch c.initStep {
	case 2:
		// ICW2: vector base.
		c.vector = value
		c.initStep = 3
	case 3:
		// ICW3: cascade info. Ignore — we don't simulate the slave-
		// chain except via the second controller.
		c.initStep = 4
	case 4:
		// ICW4: 8086/auto-EOI/buffer mode. We don't honour the details
		// — we behave as 8086 mode + non-auto-EOI for both controllers.
		c.initStep = 0
	default:
		// OCW1 — set IMR.
		c.mask = value
	}
}

// irqUnmasked returns true if the given IRQ (0-7 master, 8-15 slave) is
// currently allowed to fire on the PIC. Used by the caller to skip
// pointless RequestFixedInterrupt calls.
func (p *pic8259) irqUnmasked(irq uint8) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if irq < 8 {
		return p.master.mask&(1<<irq) == 0
	}
	if irq < 16 {
		return p.slave.mask&(1<<(irq-8)) == 0
	}
	return false
}

// vectorForIRQ returns the x86 vector the PIC delivers when IRQ `irq`
// fires — vector base + IRQ offset.
func (p *pic8259) vectorForIRQ(irq uint8) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if irq < 8 {
		return uint32(p.master.vector) + uint32(irq)
	}
	if irq < 16 {
		return uint32(p.slave.vector) + uint32(irq-8)
	}
	return 0
}

// initialized reports whether the kernel has finished the ICW
// sequence. Before init, the PIC has its default mask (0xFF) and
// any IRQ requests would be misdirected, so callers should not fire
// IRQs until this returns true.
func (p *pic8259) initialized() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.master.vector != 0
}
