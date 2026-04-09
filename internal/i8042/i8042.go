package i8042

const (
	// Base is the i8042 data port. Status/command is Base+4.
	Base = 0x60

	dataOffset   = 0
	statusOffset = 4

	cmdReadCTR   = 0x20
	cmdWriteCTR  = 0x60
	cmdReadOutp  = 0xD0
	cmdWriteOutp = 0xD1
	cmdResetCPU  = 0xFE

	statusOutDataAvail = 0x01
	statusCmdData      = 0x08
	statusKbdEnabled   = 0x10

	controlPostOK = 0x04
	controlKbdInt = 0x01
)

// Device emulates the minimal Firecracker-like i8042 behavior needed for x86
// reboot handling.
type Device struct {
	resetFn func()

	status  uint8
	control uint8
	outp    uint8
	cmd     uint8
	buf     []byte
}

func New(resetFn func()) *Device {
	return &Device{
		resetFn: resetFn,
		status:  statusKbdEnabled,
		control: controlPostOK | controlKbdInt,
		buf:     make([]byte, 0, 16),
	}
}

func (d *Device) Read(offset uint8) uint8 {
	switch offset {
	case statusOffset:
		return d.status
	case dataOffset:
		if len(d.buf) == 0 {
			return 0
		}
		b := d.buf[0]
		d.buf = d.buf[1:]
		if len(d.buf) == 0 {
			d.status &^= statusOutDataAvail
		}
		return b
	default:
		return 0
	}
}

func (d *Device) Write(offset, val uint8) {
	switch {
	case offset == statusOffset && val == cmdResetCPU:
		if d.resetFn != nil {
			d.resetFn()
		}
	case offset == statusOffset && val == cmdReadCTR:
		d.flush()
		d.push(d.control)
	case offset == statusOffset && val == cmdWriteCTR:
		d.flush()
		d.status |= statusCmdData
		d.cmd = val
	case offset == statusOffset && val == cmdReadOutp:
		d.flush()
		d.push(d.outp)
	case offset == statusOffset && val == cmdWriteOutp:
		d.status |= statusCmdData
		d.cmd = val
	case offset == dataOffset && d.status&statusCmdData != 0:
		switch d.cmd {
		case cmdWriteCTR:
			d.control = val
		case cmdWriteOutp:
			d.outp = val
		}
		d.status &^= statusCmdData
	case offset == dataOffset:
		d.flush()
		// Keyboard command ACK, matching Firecracker's minimal emulation.
		d.push(0xFA)
	}
}

func (d *Device) flush() {
	d.buf = d.buf[:0]
	d.status &^= statusOutDataAvail
}

func (d *Device) push(b uint8) {
	d.buf = append(d.buf, b)
	d.status |= statusOutDataAvail
}
