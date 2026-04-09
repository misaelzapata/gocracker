// Package pl011 emulates an ARM PrimeCell PL011 UART.
package pl011

import (
	"io"
	"sync"
)

const (
	RegDR   = 0x000
	RegFR   = 0x018
	RegIBRD = 0x024
	RegFBRD = 0x028
	RegLCRH = 0x02C
	RegCR   = 0x030
	RegIFLS = 0x034
	RegIMSC = 0x038
	RegRIS  = 0x03C
	RegMIS  = 0x040
	RegICR  = 0x044
)

const (
	FRTxFF = 1 << 5
	FRRxFE = 1 << 4
	FRBusy = 1 << 3
)

const (
	CRUARTEN = 1 << 0
	CRTXE    = 1 << 8
	CRRXE    = 1 << 9
)

const (
	intRx = 1 << 4
	intTx = 1 << 5
)

const defaultOutputBufSize = 64 * 1024

type State struct {
	IBRD uint32 `json:"ibrd,omitempty"`
	FBRD uint32 `json:"fbrd,omitempty"`
	LCRH uint32 `json:"lcrh,omitempty"`
	CR   uint32 `json:"cr,omitempty"`
	IFLS uint32 `json:"ifls,omitempty"`
	IMSC uint32 `json:"imsc,omitempty"`
	RIS  uint32 `json:"ris,omitempty"`
}

type Device struct {
	mu sync.Mutex

	ibrd uint32
	fbrd uint32
	lcrh uint32
	cr   uint32
	ifls uint32
	imsc uint32
	ris  uint32

	rxBuf []byte

	out io.Writer
	in  io.Reader

	outBuf    []byte
	outBufMax int

	irqFn func(bool)
}

func New(out io.Writer, in io.Reader, irqFn func(bool)) *Device {
	d := &Device{
		out:       out,
		in:        in,
		irqFn:     irqFn,
		outBufMax: defaultOutputBufSize,
		cr:        CRUARTEN | CRTXE | CRRXE,
	}
	if in != nil {
		go d.rxPump()
	}
	return d
}

func (d *Device) Read32(offset uint64) uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch offset {
	case RegDR:
		if len(d.rxBuf) == 0 {
			return 0
		}
		b := d.rxBuf[0]
		d.rxBuf = d.rxBuf[1:]
		if len(d.rxBuf) == 0 {
			d.ris &^= intRx
			d.updateIRQ()
		}
		return uint32(b)
	case RegFR:
		return d.flagReg()
	case RegIBRD:
		return d.ibrd
	case RegFBRD:
		return d.fbrd
	case RegLCRH:
		return d.lcrh
	case RegCR:
		return d.cr
	case RegIFLS:
		return d.ifls
	case RegIMSC:
		return d.imsc
	case RegRIS:
		return d.ris
	case RegMIS:
		return d.ris & d.imsc
	default:
		return 0
	}
}

func (d *Device) Write32(offset uint64, val uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch offset {
	case RegDR:
		if d.out != nil {
			_, _ = d.out.Write([]byte{byte(val)})
		}
		d.outBuf = append(d.outBuf, byte(val))
		if len(d.outBuf) > d.outBufMax {
			d.outBuf = d.outBuf[len(d.outBuf)-d.outBufMax:]
		}
		d.ris |= intTx
		d.updateIRQ()
	case RegIBRD:
		d.ibrd = val
	case RegFBRD:
		d.fbrd = val
	case RegLCRH:
		d.lcrh = val
	case RegCR:
		d.cr = val
	case RegIFLS:
		d.ifls = val
	case RegIMSC:
		d.imsc = val
		d.updateIRQ()
	case RegICR:
		d.ris &^= val
		d.updateIRQ()
	}
}

func (d *Device) InjectBytes(data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(data) == 0 {
		return
	}
	d.rxBuf = append(d.rxBuf, data...)
	d.ris |= intRx
	d.updateIRQ()
}

func (d *Device) OutputBytes() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.outBuf...)
}

func (d *Device) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return State{
		IBRD: d.ibrd,
		FBRD: d.fbrd,
		LCRH: d.lcrh,
		CR:   d.cr,
		IFLS: d.ifls,
		IMSC: d.imsc,
		RIS:  d.ris,
	}
}

func (d *Device) RestoreState(state State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ibrd = state.IBRD
	d.fbrd = state.FBRD
	d.lcrh = state.LCRH
	d.cr = state.CR
	d.ifls = state.IFLS
	d.imsc = state.IMSC
	d.ris = state.RIS
	d.updateIRQLocked()
}

func (d *Device) rxPump() {
	buf := make([]byte, 1024)
	for {
		n, err := d.in.Read(buf)
		if n > 0 {
			d.InjectBytes(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (d *Device) flagReg() uint32 {
	fr := uint32(0)
	if len(d.rxBuf) == 0 {
		fr |= FRRxFE
	}
	if len(d.rxBuf) > 0 {
		fr |= FRBusy
	}
	return fr
}

func (d *Device) updateIRQ() {
	d.updateIRQLocked()
}

func (d *Device) updateIRQLocked() {
	if d.irqFn == nil {
		return
	}
	d.irqFn((d.ris & d.imsc) != 0)
}
