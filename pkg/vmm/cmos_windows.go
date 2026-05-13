//go:build windows

package vmm

import (
	"sync"
	"time"
)

// cmosRTC is a minimal MC146818-style CMOS RTC. Linux reads BCD time
// fields from ports 0x70 (index) + 0x71 (data) during boot to seed
// its wall clock. Without an answer, the kernel logs "rtc_cmos:
// invalid alarm value" or hangs in hwclock during userspace init.
//
// Ported from node-vmm's native/whp/devices/cmos.cc — only enough to
// satisfy the kernel's RTC probe and a userspace hwclock --systohc.
//
// What this DOES implement:
//   - Index register (0x70) selects which RTC reg the next 0x71 op hits
//   - Read returns the live wall-clock value in BCD on each read of
//     time/date fields (seconds, minutes, hours, day, month, year)
//   - Status registers A (UIP/divider), B (24h+BCD), D (battery ok)
//
// What it does NOT implement:
//   - Alarm interrupts (regs 0x01/0x03/0x05) — read as 0
//   - The PIE/AIE/UIE bits don't generate IRQs; we never raise IRQ 8
//   - Writes to the date fields are ignored — the guest can't set our
//     virtual clock
type cmosRTC struct {
	mu       sync.Mutex
	selected uint8
	regs     [128]byte
}

func newCMOS() *cmosRTC {
	c := &cmosRTC{}
	c.regs[0x0A] = 0x26 // 32.768 kHz divider, 1024 Hz periodic, UIP=0
	c.regs[0x0B] = 0x02 // 24-hour mode + BCD numeric format
	c.regs[0x0D] = 0x80 // VRT (Valid RAM and Time, battery OK)
	c.regs[0x0F] = 0x00 // Shutdown status: clean
	c.refreshTime()
	return c
}

// handles reports whether this port belongs to the RTC.
func (c *cmosRTC) handles(port uint16) bool {
	return port == 0x70 || port == 0x71
}

func (c *cmosRTC) readPort(port uint16) byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if port != 0x71 {
		return 0xFF
	}
	c.refreshTime()
	return c.regs[c.selected&0x7F]
}

func (c *cmosRTC) writePort(port uint16, value byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch port {
	case 0x70:
		c.selected = value
	case 0x71:
		reg := c.selected & 0x7F
		// Only let writes through to control/status regs — keep date
		// fields read-only so the guest can't drag our virtual clock.
		switch reg {
		case 0x0A, 0x0B, 0x0C, 0x0F:
			c.regs[reg] = value
		}
	}
}

// toBCD encodes a decimal value as packed BCD. Used for the
// MC146818-default time/date format.
func toBCD(v uint16) byte {
	return byte(((v / 10) << 4) | (v % 10))
}

// encode picks BCD or binary based on status register B bit 2 (DM —
// "data mode"). Bit set means binary; clear (our default) means BCD.
func (c *cmosRTC) encode(v uint16) byte {
	if c.regs[0x0B]&0x04 != 0 {
		return byte(v)
	}
	return toBCD(v)
}

// refreshTime samples the host wall clock and re-encodes the RTC
// time/date registers. Called from every read so reads always see the
// current host time.
func (c *cmosRTC) refreshTime() {
	now := time.Now().UTC()
	c.regs[0x00] = c.encode(uint16(now.Second()))
	c.regs[0x02] = c.encode(uint16(now.Minute()))
	c.regs[0x04] = c.encode(uint16(now.Hour()))
	c.regs[0x06] = c.encode(uint16(now.Weekday() + 1)) // RTC: Sunday=1
	c.regs[0x07] = c.encode(uint16(now.Day()))
	c.regs[0x08] = c.encode(uint16(now.Month()))
	c.regs[0x09] = c.encode(uint16(now.Year() % 100))
	c.regs[0x32] = c.encode(uint16(now.Year() / 100))
}
