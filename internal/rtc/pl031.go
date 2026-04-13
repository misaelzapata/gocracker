// Package rtc implements an ARM PL031 real-time clock.
// The kernel reads RTCDR at boot to set the system wall clock.
package rtc

import (
	"encoding/binary"
	"sync"
	"time"
)

// PL031 register offsets.
const (
	RTCDR   = 0x00 // Data Register (RO): current time in seconds since epoch
	RTCMR   = 0x04 // Match Register (RW): alarm trigger
	RTCLR   = 0x08 // Load Register (WO): set current time
	RTCCR   = 0x0C // Control Register (RW): enable/disable
	RTCIMSC = 0x10 // Interrupt Mask Set/Clear (RW)
	RTCRIS  = 0x14 // Raw Interrupt Status (RO)
	RTCMIS  = 0x18 // Masked Interrupt Status (RO)
	RTCICR  = 0x1C // Interrupt Clear Register (WO)
)

// PL031 implements the ARM AMBA PL031 RTC as used by Firecracker.
type PL031 struct {
	mu       sync.Mutex
	baseTime time.Time // host wall clock at creation
	loadOff  int64     // offset applied by guest RTCLR writes
	matchReg uint32
	irqMask  uint32
	enabled  uint32 // RTCCR: 1 = enabled
}

// New creates a PL031 RTC initialized to the current wall clock time.
func New() *PL031 {
	return &PL031{
		baseTime: time.Now(),
		enabled:  1,
	}
}

// Read handles a 4-byte MMIO read at the given register offset.
func (r *PL031) Read(offset uint16) uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()

	off := uint32(offset) & 0xFFC
	switch {
	case off == RTCDR:
		return uint32(r.baseTime.Unix() + r.loadOff + int64(time.Since(r.baseTime).Seconds()))
	case off == RTCMR:
		return r.matchReg
	case off == RTCLR:
		return 0
	case off == RTCCR:
		return r.enabled
	case off == RTCIMSC:
		return r.irqMask
	case off == RTCRIS:
		return 0
	case off == RTCMIS:
		return 0
	case off == RTCICR:
		return 0
	case off >= 0xFE0 && off <= 0xFFC:
		// AMBA PrimeCell Peripheral ID and PCell ID registers.
		// The kernel reads these to identify the device on the AMBA bus.
		// Values match the real ARM PL031 hardware (part number 0x031).
		idx := (off - 0xFE0) / 4
		periphID := [8]uint32{
			0x31, 0x10, 0x04, 0x00, // PeriphID0-3: part=0x031, designer=ARM
			0x0D, 0xF0, 0x05, 0xB1, // PrimeCellID0-3: standard 0xB105F00D
		}
		return periphID[idx]
	default:
		return 0
	}
}

// Write handles a 4-byte MMIO write at the given register offset.
func (r *PL031) Write(offset uint16, val uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch uint32(offset) & 0xFC {
	case RTCMR:
		r.matchReg = val
	case RTCLR:
		r.baseTime = time.Now()
		r.loadOff = int64(val) - r.baseTime.Unix()
	case RTCCR:
		r.enabled = val & 1
	case RTCIMSC:
		r.irqMask = val & 1
	case RTCICR:
		// clear interrupt - no-op since we don't generate them
	}
}

// ReadBytes handles an MMIO read of arbitrary length (typically 4 bytes).
func (r *PL031) ReadBytes(offset uint16, data []byte) {
	if len(data) == 4 {
		binary.LittleEndian.PutUint32(data, r.Read(offset))
	}
}

// WriteBytes handles an MMIO write of arbitrary length (typically 4 bytes).
func (r *PL031) WriteBytes(offset uint16, data []byte) {
	if len(data) == 4 {
		r.Write(offset, binary.LittleEndian.Uint32(data))
	}
}
