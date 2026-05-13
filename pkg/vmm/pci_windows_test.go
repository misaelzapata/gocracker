//go:build windows

package vmm

import "testing"

// TestPCIHandlesRange verifies that handles() reports true for every
// port in the Mechanism #1 range (0xCF8..0xCFF) and false for the
// adjacent ports that don't belong to PCI.
func TestPCIHandlesRange(t *testing.T) {
	p := newPCIConfigDummy()

	for port := uint16(0xCF8); port <= 0xCFF; port++ {
		if !p.handles(port) {
			t.Errorf("handles(%#x) = false; want true (PCI config port)", port)
		}
	}

	for _, port := range []uint16{0x00, 0x70, 0xCF7, 0xD00, 0xFFFF} {
		if p.handles(port) {
			t.Errorf("handles(%#x) = true; want false (not a PCI port)", port)
		}
	}
}

// TestPCIDataReadsAreFF verifies that every byte read of the CONFIG_DATA
// window returns 0xFF, so a 32-bit insl pulls 0xFFFFFFFF and Linux
// reads vendor=0xFFFF (no device). We try several different addresses
// to confirm the dummy doesn't decode (bus, dev, fn) at all.
func TestPCIDataReadsAreFF(t *testing.T) {
	p := newPCIConfigDummy()

	addresses := []uint32{
		0x00000000,                 // disabled, bus 0 dev 0 fn 0 reg 0
		0x80000000,                 // enabled,  bus 0 dev 0 fn 0 reg 0
		0x80008000,                 // enabled,  bus 0 dev 1 fn 0 reg 0
		0x8000F800 | (0x3C &^ 0x3), // enabled,  bus 0 dev 31 fn 0 reg 0x3C
		0xFFFFFFFC,                 // saturate everything
	}

	for _, addr := range addresses {
		// Write the address register byte-by-byte (kernel does an outl
		// but the VMM decomposes that into 4 outb).
		for i := uint16(0); i < 4; i++ {
			p.writePort(0xCF8+i, byte(addr>>(i*8)))
		}

		for port := uint16(0xCFC); port <= 0xCFF; port++ {
			if got := p.readPort(port); got != 0xFF {
				t.Errorf("addr=%#x readPort(%#x) = %#x; want 0xFF (no device)",
					addr, port, got)
			}
		}
	}
}

// TestPCIAddressRoundTrip verifies that writes to CONFIG_ADDRESS are
// stashed and read back unchanged, including byte-granular writes that
// patch sub-ranges of the dword.
func TestPCIAddressRoundTrip(t *testing.T) {
	p := newPCIConfigDummy()

	// Whole-dword round trip via byte writes (little-endian).
	want := uint32(0x80FE7B3C)
	for i := uint16(0); i < 4; i++ {
		p.writePort(0xCF8+i, byte(want>>(i*8)))
	}
	var got uint32
	for i := uint16(0); i < 4; i++ {
		got |= uint32(p.readPort(0xCF8+i)) << (i * 8)
	}
	if got != want {
		t.Errorf("address round-trip: got %#x; want %#x", got, want)
	}

	// Per-byte patch: change just byte 2 (bus field) and confirm the
	// rest of the register is undisturbed.
	p.writePort(0xCFA, 0xAB)
	got = 0
	for i := uint16(0); i < 4; i++ {
		got |= uint32(p.readPort(0xCF8+i)) << (i * 8)
	}
	wantPatched := (want &^ 0x00FF0000) | (0xAB << 16)
	if got != wantPatched {
		t.Errorf("patched address: got %#x; want %#x", got, wantPatched)
	}
}

// TestPCIDataWritesDropped verifies that writes to the CONFIG_DATA
// window are silently dropped — they must not corrupt the address
// register or cause reads to return anything other than 0xFF.
func TestPCIDataWritesDropped(t *testing.T) {
	p := newPCIConfigDummy()

	// Set a known address first.
	want := uint32(0x80001234)
	for i := uint16(0); i < 4; i++ {
		p.writePort(0xCF8+i, byte(want>>(i*8)))
	}

	// Hammer the data window — try to poke a vendor/device ID in.
	for port := uint16(0xCFC); port <= 0xCFF; port++ {
		p.writePort(port, 0xAA)
	}

	// Address register must be unchanged.
	var got uint32
	for i := uint16(0); i < 4; i++ {
		got |= uint32(p.readPort(0xCF8+i)) << (i * 8)
	}
	if got != want {
		t.Errorf("data writes corrupted address: got %#x; want %#x", got, want)
	}

	// Data reads must still be 0xFF.
	for port := uint16(0xCFC); port <= 0xCFF; port++ {
		if r := p.readPort(port); r != 0xFF {
			t.Errorf("after writes: readPort(%#x) = %#x; want 0xFF", port, r)
		}
	}
}
