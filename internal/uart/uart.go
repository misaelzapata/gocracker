// Package uart emulates a 16550A UART (serial port).
// It handles all register reads/writes the Linux kernel performs
// before and during console I/O on ttyS0 (COM1, base port 0x3F8).
package uart

import (
	"io"
	"os"
	"sync"
)

// UART register offsets from base port
const (
	RegRBR = 0 // Receive Buffer Register (read, DLAB=0)
	RegTHR = 0 // Transmit Holding Register (write, DLAB=0)
	RegDLL = 0 // Divisor Latch Low (DLAB=1)
	RegIER = 1 // Interrupt Enable Register (DLAB=0)
	RegDLH = 1 // Divisor Latch High (DLAB=1)
	RegIIR = 2 // Interrupt Identification Register (read)
	RegFCR = 2 // FIFO Control Register (write)
	RegLCR = 3 // Line Control Register
	RegMCR = 4 // Modem Control Register
	RegLSR = 5 // Line Status Register
	RegMSR = 6 // Modem Status Register
	RegSCR = 7 // Scratch Register
)

// LSR bits
const (
	LSRDataReady     = 0x01
	LSROverrunErr    = 0x02
	LSRParityErr     = 0x04
	LSRFramingErr    = 0x08
	LSRBreakInt      = 0x10
	LSRTHREmpty      = 0x20 // THR is empty — ready to transmit
	LSRTransmitEmpty = 0x40 // transmitter shift register empty
	LSRFIFOErr       = 0x80
)

// IER bits
const (
	IERRxDataAvail = 0x01
	IERTHREmpty    = 0x02
	IERLineStatus  = 0x04
	IERModemStatus = 0x08
)

// MCR bits
const (
	MCRDTR      = 0x01
	MCRRTS      = 0x02
	MCROut1     = 0x04
	MCROut2     = 0x08
	MCRLoopback = 0x10
)

// IIR values (no interrupt pending = 1)
const (
	IIRNoPending   = 0x01
	IIRTHREmpty    = 0x02
	IIRRxDataAvail = 0x04
	IIRLineStatus  = 0x06
	IIRCharTimeout = 0x0C
	IIRFIFOEnabled = 0xC0
)

const defaultOutputBufSize = 64 * 1024 // 64 KiB

// UART emulates a 16550A serial port.
type UART struct {
	mu sync.Mutex

	// Registers
	ier uint8
	iir uint8
	lcr uint8
	mcr uint8
	lsr uint8
	msr uint8
	scr uint8
	dll uint8
	dlh uint8

	// RX FIFO (host → guest)
	rxBuf []byte

	// Output (guest → host)
	out io.Writer
	in  io.Reader

	// Buffered console output for API retrieval (ring buffer)
	outBuf    []byte
	outBufMax int

	// IRQ callback: called when interrupt state changes
	irqFn func(asserted bool)

	nextAttachID uint64
	attachments  map[uint64]*consoleAttachment
}

type consoleAttachment struct {
	uart *UART
	id   uint64

	guestRead  *os.File
	guestWrite *os.File
	hostRead   *os.File
	hostWrite  *os.File

	closeOnce sync.Once
}

// New creates a UART with the given I/O streams and IRQ callback.
func New(out io.Writer, in io.Reader, irqFn func(bool)) *UART {
	u := &UART{
		out:       out,
		in:        in,
		irqFn:     irqFn,
		outBufMax: defaultOutputBufSize,
	}
	// Initial LSR: transmitter empty and idle (ready to send)
	u.lsr = LSRTHREmpty | LSRTransmitEmpty
	u.iir = IIRNoPending
	// MSR: CTS, DSR, DCD asserted (modem connected)
	u.msr = 0xB0
	// MCR: DTR + RTS
	u.mcr = 0x03

	// Start reading from input stream asynchronously
	if in != nil {
		go u.rxPump()
	}
	return u
}

// AttachConsole opens a live bidirectional console attachment.
func (u *UART) AttachConsole() (io.ReadWriteCloser, error) {
	guestRead, hostWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	hostRead, guestWrite, err := os.Pipe()
	if err != nil {
		_ = guestRead.Close()
		_ = hostWrite.Close()
		return nil, err
	}

	attachment := &consoleAttachment{
		uart:       u,
		guestRead:  guestRead,
		guestWrite: guestWrite,
		hostRead:   hostRead,
		hostWrite:  hostWrite,
	}

	u.mu.Lock()
	u.nextAttachID++
	attachment.id = u.nextAttachID
	if u.attachments == nil {
		u.attachments = map[uint64]*consoleAttachment{}
	}
	u.attachments[attachment.id] = attachment
	u.mu.Unlock()

	go attachment.pumpInput()

	return attachment, nil
}

// Read handles a guest read from port (base + offset).
func (u *UART) Read(offset uint8) uint8 {
	u.mu.Lock()
	defer u.mu.Unlock()

	dlab := u.lcr&0x80 != 0

	switch offset {
	case RegRBR:
		if dlab {
			return u.dll
		}
		if len(u.rxBuf) > 0 {
			b := u.rxBuf[0]
			u.rxBuf = u.rxBuf[1:]
			// Acknowledge the RDA interrupt on every byte read, the way
			// vm-superio does it. The next injectRXBytesLocked call will
			// re-raise it from a clean state. Without this ack, our IIR
			// stayed at IIRRxDataAvail forever and the dedupe in
			// `injectRXBytesLocked` could swallow the next pulse.
			if u.iir == IIRRxDataAvail {
				u.iir = IIRNoPending
			}
			if len(u.rxBuf) == 0 {
				u.lsr &^= LSRDataReady
			}
			u.updateIIR()
			return b
		}
		return 0

	case RegIER:
		if dlab {
			return u.dlh
		}
		return u.ier

	case RegIIR:
		iir := u.iir | IIRFIFOEnabled
		// Reading IIR clears THR-empty interrupt
		if u.iir == IIRTHREmpty {
			u.iir = IIRNoPending
		}
		return iir

	case RegLCR:
		return u.lcr
	case RegMCR:
		return u.mcr
	case RegLSR:
		return u.lsr
	case RegMSR:
		return u.msr
	case RegSCR:
		return u.scr
	}
	return 0
}

// Write handles a guest write to port (base + offset).
func (u *UART) Write(offset, val uint8) {
	u.mu.Lock()
	defer u.mu.Unlock()

	dlab := u.lcr&0x80 != 0

	switch offset {
	case RegTHR:
		if dlab {
			u.dll = val
			return
		}
		if u.mcr&MCRLoopback != 0 {
			u.injectLoopbackByteLocked(val)
			return
		}
		// Transmit character to output
		if u.out != nil {
			u.out.Write([]byte{val}) //nolint:errcheck
		}
		u.writeAttachmentsLocked(val)
		// Buffer for API retrieval
		u.outBuf = append(u.outBuf, val)
		if len(u.outBuf) > u.outBufMax {
			u.outBuf = u.outBuf[len(u.outBuf)-u.outBufMax:]
		}
		// THR stays empty — we're always ready
		u.lsr |= LSRTHREmpty | LSRTransmitEmpty
		// Raise THR-empty interrupt if enabled
		if u.ier&IERTHREmpty != 0 {
			u.iir = IIRTHREmpty
			if u.irqFn != nil {
				u.irqFn(true)
			}
		}

	case RegIER:
		if dlab {
			u.dlh = val
			return
		}
		u.ier = val & 0x0F
		u.updateIIR()
		// Assert IRQ if enabling an interrupt that already has a pending condition.
		// The 8250 driver expects this (e.g. enabling THRE when THR is already empty).
		if u.iir != IIRNoPending && u.irqFn != nil {
			u.irqFn(true)
		}

	case RegFCR:
		// FIFO control — accept but mostly ignore (we always have FIFO)

	case RegLCR:
		u.lcr = val

	case RegMCR:
		u.mcr = val & 0x1F
		u.updateMSRLocked()

	case RegLSR:
		// Read-only in real hardware; ignore writes

	case RegMSR:
		// Read-only in real hardware; ignore writes

	case RegSCR:
		u.scr = val
	}
}

// InjectByte queues a byte as if received from the serial line (host → guest).
func (u *UART) InjectByte(b byte) {
	u.InjectBytes([]byte{b})
}

// InjectBytes queues a batch of bytes as if received from the serial line.
// This is the preferred path: a single mutex acquisition per batch and a
// single IRQ pulse for the whole burst, which avoids the keystroke-loss race
// where back-to-back single-byte InjectByte calls could spam IRQs faster
// than the guest could acknowledge them.
func (u *UART) InjectBytes(bytes []byte) {
	if len(bytes) == 0 {
		return
	}
	u.mu.Lock()
	for _, b := range bytes {
		u.rxBuf = append(u.rxBuf, b)
	}
	u.lsr |= LSRDataReady
	// Only raise the RDA interrupt when it is not already pending: the
	// guest will drain the entire FIFO from a single ISR. Re-raising it
	// while the guest is still inside the same handler can defeat
	// edge-triggered IRQ delivery and lose the next byte.
	raise := false
	if u.ier&IERRxDataAvail != 0 && u.iir != IIRRxDataAvail {
		u.iir = IIRRxDataAvail
		raise = true
	} else {
		u.updateIIR()
	}
	u.mu.Unlock()
	if raise && u.irqFn != nil {
		u.irqFn(true)
	}
}

// State returns a snapshot of the UART state.
func (u *UART) State() UARTState {
	u.mu.Lock()
	defer u.mu.Unlock()
	rxCopy := make([]byte, len(u.rxBuf))
	copy(rxCopy, u.rxBuf)
	outCopy := make([]byte, len(u.outBuf))
	copy(outCopy, u.outBuf)
	return UARTState{
		IER: u.ier, IIR: u.iir, LCR: u.lcr, MCR: u.mcr,
		LSR: u.lsr, MSR: u.msr, SCR: u.scr, DLL: u.dll, DLH: u.dlh,
		RxBuf:  rxCopy,
		OutBuf: outCopy,
	}
}

// RestoreState restores UART registers from a snapshot.
func (u *UART) RestoreState(s UARTState) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.ier = s.IER
	u.iir = s.IIR
	u.lcr = s.LCR
	u.mcr = s.MCR
	u.lsr = s.LSR
	u.msr = s.MSR
	u.scr = s.SCR
	u.dll = s.DLL
	u.dlh = s.DLH
	u.rxBuf = make([]byte, len(s.RxBuf))
	copy(u.rxBuf, s.RxBuf)
	u.outBuf = make([]byte, len(s.OutBuf))
	copy(u.outBuf, s.OutBuf)
}

// OutputBytes returns a copy of the buffered console output.
func (u *UART) OutputBytes() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]byte, len(u.outBuf))
	copy(out, u.outBuf)
	return out
}

func (u *UART) writeAttachmentsLocked(b byte) {
	if len(u.attachments) == 0 {
		return
	}
	var stale []uint64
	for id, attachment := range u.attachments {
		if _, err := attachment.guestWrite.Write([]byte{b}); err != nil {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		if attachment := u.attachments[id]; attachment != nil {
			delete(u.attachments, id)
			go attachment.closeOwned()
		}
	}
}

func (u *UART) detachConsole(id uint64) {
	u.mu.Lock()
	attachment := u.attachments[id]
	delete(u.attachments, id)
	u.mu.Unlock()
	if attachment != nil {
		attachment.closeOwned()
	}
}

// Ports returns the I/O port range for this UART [base, base+8).
func Ports(base uint16) (uint16, uint16) {
	return base, base + 8
}

// updateIIR recalculates the IIR based on pending conditions.
// Must be called with u.mu held.
func (u *UART) updateIIR() {
	if u.lsr&LSRDataReady != 0 && u.ier&IERRxDataAvail != 0 {
		u.iir = IIRRxDataAvail
	} else if u.lsr&LSRTHREmpty != 0 && u.ier&IERTHREmpty != 0 {
		u.iir = IIRTHREmpty
	} else {
		u.iir = IIRNoPending
	}
}

func (u *UART) injectLoopbackByteLocked(b byte) {
	u.injectRXByteLocked(b)
}

func (u *UART) injectRXByteLocked(b byte) {
	u.rxBuf = append(u.rxBuf, b)
	u.lsr |= LSRDataReady
	u.updateIIR()
}

func (u *UART) updateMSRLocked() {
	if u.mcr&MCRLoopback == 0 {
		u.msr = 0xB0
		return
	}

	var msr uint8
	if u.mcr&MCRRTS != 0 {
		msr |= 0x10 // CTS
	}
	if u.mcr&MCRDTR != 0 {
		msr |= 0x20 // DSR
	}
	if u.mcr&MCROut1 != 0 {
		msr |= 0x40 // RI
	}
	if u.mcr&MCROut2 != 0 {
		msr |= 0x80 // DCD
	}
	u.msr = msr
}

func (a *consoleAttachment) Read(p []byte) (int, error) {
	return a.hostRead.Read(p)
}

func (a *consoleAttachment) Write(p []byte) (int, error) {
	return a.hostWrite.Write(p)
}

func (a *consoleAttachment) Close() error {
	if a == nil || a.uart == nil {
		return nil
	}
	a.uart.detachConsole(a.id)
	return nil
}

func (a *consoleAttachment) closeOwned() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		if a.hostRead != nil {
			_ = a.hostRead.Close()
		}
		if a.hostWrite != nil {
			_ = a.hostWrite.Close()
		}
		if a.guestRead != nil {
			_ = a.guestRead.Close()
		}
		if a.guestWrite != nil {
			_ = a.guestWrite.Close()
		}
	})
}

func (a *consoleAttachment) pumpInput() {
	buf := make([]byte, 4096)
	for {
		n, err := a.guestRead.Read(buf)
		if n > 0 {
			a.uart.InjectBytes(buf[:n])
		}
		if err != nil {
			a.uart.detachConsole(a.id)
			return
		}
	}
}

// rxPump reads from the input reader and injects bytes into the RX buffer.
// We deliver batches with a single IRQ pulse so a fast typist or pasted
// input never races the guest's ISR boundary.
func (u *UART) rxPump() {
	buf := make([]byte, 64)
	for {
		n, err := u.in.Read(buf)
		if n > 0 {
			u.InjectBytes(buf[:n])
		}
		if err != nil {
			return
		}
	}
}
