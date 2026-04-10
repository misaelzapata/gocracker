//go:build linux

package vmm

import (
	"context"
	"testing"
	"time"
)

func TestMemoryLayoutConstants(t *testing.T) {
	if BootParamsAddr != 0x7000 {
		t.Errorf("BootParamsAddr = %#x, want %#x", BootParamsAddr, 0x7000)
	}
	if KernelLoad != 0x100000 {
		t.Errorf("KernelLoad = %#x, want %#x", KernelLoad, 0x100000)
	}
	if InitrdAddr != 0x1000000 {
		t.Errorf("InitrdAddr = %#x, want %#x", InitrdAddr, 0x1000000)
	}
	if COM1Base != 0x3F8 {
		t.Errorf("COM1Base = %#x, want %#x", COM1Base, 0x3F8)
	}
	if COM1IRQ != 4 {
		t.Errorf("COM1IRQ = %d, want 4", COM1IRQ)
	}
}

func TestVirtioLayoutConstants(t *testing.T) {
	if VirtioBase != 0xD0000000 {
		t.Errorf("VirtioBase = %#x, want %#x", VirtioBase, 0xD0000000)
	}
	if VirtioStride != 0x1000 {
		t.Errorf("VirtioStride = %#x, want %#x", VirtioStride, 0x1000)
	}
	if VirtioIRQBase != 5 {
		t.Errorf("VirtioIRQBase = %d, want 5", VirtioIRQBase)
	}
}

func TestNewMachineArchBackendAMD64(t *testing.T) {
	backend, err := newMachineArchBackend(ArchAMD64)
	if err != nil {
		t.Fatalf("newMachineArchBackend(amd64) error = %v", err)
	}
	if backend == nil {
		t.Fatal("newMachineArchBackend(amd64) = nil, want backend")
	}
}

func TestNewMachineArchBackendARM64StillExplicitlyRejected(t *testing.T) {
	backend, err := newMachineArchBackend(ArchARM64)
	if err == nil {
		t.Fatalf("newMachineArchBackend(arm64) error = nil, backend = %#v", backend)
	}
}

func TestWaitStoppedReturnsWhenDone(t *testing.T) {
	vm := &VM{doneCh: make(chan struct{})}
	close(vm.doneCh)
	if err := vm.WaitStopped(context.Background()); err != nil {
		t.Fatalf("WaitStopped() = %v, want nil", err)
	}
}

func TestWaitStoppedHonorsContext(t *testing.T) {
	vm := &VM{doneCh: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := vm.WaitStopped(ctx); err == nil {
		t.Fatal("WaitStopped() = nil, want context error")
	}
}
