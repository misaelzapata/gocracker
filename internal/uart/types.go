// This file contains pure data type definitions used for snapshot serialization.
// These types have no platform-specific dependencies and compile on all targets.
package uart

// UARTState stores the serializable state of a 16550A UART device for
// snapshot/restore.
type UARTState struct {
	IER    uint8  `json:"ier"`
	IIR    uint8  `json:"iir"`
	LCR    uint8  `json:"lcr"`
	MCR    uint8  `json:"mcr"`
	LSR    uint8  `json:"lsr"`
	MSR    uint8  `json:"msr"`
	SCR    uint8  `json:"scr"`
	DLL    uint8  `json:"dll"`
	DLH    uint8  `json:"dlh"`
	RxBuf  []byte `json:"rx_buf,omitempty"`
	OutBuf []byte `json:"out_buf,omitempty"`
}
