//go:build windows

package vmm

import (
	"testing"
	"time"
)

// TestCMOSReadbacks drives the index/data port handshake and verifies
// the static control registers + that the time fields encode the host
// wall clock as BCD.
func TestCMOSReadbacks(t *testing.T) {
	c := newCMOS()

	read := func(reg uint8) byte {
		c.writePort(0x70, reg)
		return c.readPort(0x71)
	}

	if got := read(0x0A); got != 0x26 {
		t.Errorf("Status A = %#x; want 0x26 (default divider+rate)", got)
	}
	if got := read(0x0B); got != 0x02 {
		t.Errorf("Status B = %#x; want 0x02 (24h+BCD)", got)
	}
	if got := read(0x0D); got&0x80 == 0 {
		t.Errorf("Status D = %#x; want VRT bit (0x80) set", got)
	}

	// Read seconds register; should match host time modulo a second
	// boundary. Decode BCD and check it's within a 2-second window.
	now := time.Now().UTC()
	got := read(0x00)
	hi := got >> 4
	lo := got & 0x0F
	if hi > 9 || lo > 9 {
		t.Fatalf("seconds reg = %#x; not valid BCD", got)
	}
	sec := int(hi)*10 + int(lo)
	delta := sec - now.Second()
	if delta < 0 {
		delta = -delta
	}
	if delta > 2 && delta < 58 { // tolerate wrap-around at minute boundary
		t.Errorf("seconds reg decodes to %d; host has %d", sec, now.Second())
	}
}

// TestCMOSWriteIgnored verifies date-field writes don't take effect
// (the guest can't tamper with our virtual clock).
func TestCMOSWriteIgnored(t *testing.T) {
	c := newCMOS()
	c.writePort(0x70, 0x09) // year reg
	c.writePort(0x71, 0xFF) // try to set year to 99
	c.writePort(0x70, 0x09)
	got := c.readPort(0x71)
	now := time.Now().UTC().Year() % 100
	if got == 0xFF {
		t.Errorf("year reg = 0xFF; writes should be ignored, expected encoded host year (%d)", now)
	}
}
