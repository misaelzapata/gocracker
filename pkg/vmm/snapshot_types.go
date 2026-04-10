package vmm

import (
	"time"

	"github.com/gocracker/gocracker/internal/kvm"
	"github.com/gocracker/gocracker/internal/uart"
	"github.com/gocracker/gocracker/internal/virtio"
)

// Snapshot captures full VM state for persistence and restore.
type Snapshot struct {
	Version    int                     `json:"version"`
	Timestamp  time.Time               `json:"timestamp"`
	ID         string                  `json:"id"`
	Config     Config                  `json:"config"`
	VCPUs      []VCPUState             `json:"vcpus,omitempty"`
	Regs       kvm.Regs                `json:"regs,omitempty"`
	Sregs      kvm.Sregs               `json:"sregs,omitempty"`
	MPState    kvm.MPState             `json:"mp_state,omitempty"`
	MemFile    string                  `json:"mem_file"`
	Arch       *SnapshotArchState      `json:"arch,omitempty"`
	UART       *uart.UARTState         `json:"uart,omitempty"`
	Transports []virtio.TransportState `json:"transports,omitempty"`
}
