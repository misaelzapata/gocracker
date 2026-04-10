//go:build linux && arm64

package kvm

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// KVM device management ioctl numbers (arm64 encodings).
const (
	kvmCreateDevice  = uintptr(0xC00CAEE0) // _IOWR(KVMIO, 0xE0, kvm_create_device)
	kvmSetDeviceAttr = uintptr(0x4018AEE1) // _IOW(KVMIO, 0xE1, kvm_device_attr)
	kvmGetDeviceAttr = uintptr(0xC018AEE2) // _IOWR(KVMIO, 0xE2, kvm_device_attr)
)

// GIC device types and attribute constants from Linux include/uapi/linux/kvm.h.
const (
	kvmDevTypeArmVGICv2 = 5
	kvmDevTypeArmVGICv3 = 7

	// Attribute groups (shared between v2 and v3)
	kvmDevArmVGICGrpAddr   = 0
	kvmDevArmVGICGrpNrIRQs = 3
	kvmDevArmVGICGrpCtrl   = 4

	// GICv3 address type attributes
	kvmVGICv3AddrTypeDist   = 0
	kvmVGICv3AddrTypeRedist = 1

	// GICv2 address type attributes
	kvmVGICv2AddrTypeDist = 0
	kvmVGICv2AddrTypeCPU  = 1

	// Control attributes
	kvmDevArmVGICCtrlInit = 0
)

type kvmCreateDeviceData struct {
	Type  uint32
	Fd    uint32
	Flags uint32
}

type kvmDeviceAttr struct {
	Flags uint32
	Group uint32
	Attr  uint64
	Addr  uint64
}

// GICDevice represents an in-kernel GIC (v2 or v3) interrupt controller.
type GICDevice struct {
	fd      int
	version int // 2 or 3
}

// CreateGIC creates an in-kernel GIC. It probes GICv3 first and falls back to
// GICv2 if the host doesn't support v3, matching Firecracker's behavior.
//
// gicdBase is the distributor MMIO base (same for v2 and v3).
// gicrBase is the redistributor base (v3) — for the v2 fallback the CPU
// interface is placed at gicdBase + 0x10000 (QEMU virt convention).
func (vm *VM) CreateGIC(gicdBase, gicrBase uint64, nrIRQs uint32) (*GICDevice, error) {
	// KVM allows only one GIC per VM — a failed create still registers
	// the device, blocking fallback. Use probeDevice (TEST flag) to pick
	// the right version before creating for real.
	//
	// Prefer GICv3 when available (Graviton 2+). Fall back to GICv2
	// (Graviton 1 / Cortex-A72). Matches Firecracker's gic/mod.rs.
	//
	// Note: some hosts (Graviton 1) report GICv3 as known but fail to
	// init it. We detect this by trying v3 creation first on hosts that
	// DON'T support v2 (pure v3), and preferring v2 when both probe OK.
	v2ok := vm.probeDevice(kvmDevTypeArmVGICv2)
	v3ok := vm.probeDevice(kvmDevTypeArmVGICv3)

	if v2ok {
		// GICv2 available — use it. Safe on Graviton 1 and QEMU.
		// CPU interface is BELOW the distributor (Firecracker: GICD - CPU_SIZE).
		const gicv2CPUSize uint64 = 0x2000
		gicv2CPUBase := gicdBase - gicv2CPUSize
		gic, err := vm.tryCreateGICv2(gicdBase, gicv2CPUBase, nrIRQs)
		if err == nil {
			return gic, nil
		}
		return nil, fmt.Errorf("GICv2: %w", err)
	}
	if v3ok {
		// Only GICv3 available (Graviton 2+, no v2 fallback).
		gic, err := vm.tryCreateGICv3(gicdBase, gicrBase, nrIRQs)
		if err == nil {
			return gic, nil
		}
		return nil, fmt.Errorf("GICv3: %w", err)
	}
	return nil, fmt.Errorf("neither GICv2 nor GICv3 supported by host KVM")
}

// probeDevice tests if a KVM device type is supported without creating it.
func (vm *VM) probeDevice(devType uint32) bool {
	data := kvmCreateDeviceData{Type: devType, Flags: 1} // flags=1 = KVM_CREATE_DEVICE_TEST
	_, err := vmIoctl(vm.fd, kvmCreateDevice, uintptr(unsafe.Pointer(&data)))
	return err == nil
}

func (vm *VM) tryCreateGICv3(gicdBase, gicrBase uint64, nrIRQs uint32) (*GICDevice, error) {
	fd, err := vm.createDevice(kvmDevTypeArmVGICv3)
	if err != nil {
		return nil, err
	}
	steps := []struct {
		name  string
		group uint32
		attr  uint64
		val   uint64
	}{
		{"distributor", kvmDevArmVGICGrpAddr, kvmVGICv3AddrTypeDist, gicdBase},
		{"redistributor", kvmDevArmVGICGrpAddr, kvmVGICv3AddrTypeRedist, gicrBase},
		{"nr_irqs", kvmDevArmVGICGrpNrIRQs, 0, uint64(nrIRQs)},
	}
	for _, s := range steps {
		val := s.val
		if err := setDeviceAttr(fd, s.group, s.attr, unsafe.Pointer(&val)); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("GICv3 %s: %w", s.name, err)
		}
	}
	if err := setDeviceAttr(fd, kvmDevArmVGICGrpCtrl, kvmDevArmVGICCtrlInit, nil); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("GICv3 ctrl init: %w", err)
	}
	return &GICDevice{fd: fd, version: 3}, nil
}

func (vm *VM) tryCreateGICv2(gicdBase, cpuBase uint64, nrIRQs uint32) (*GICDevice, error) {
	fd, err := vm.createDevice(kvmDevTypeArmVGICv2)
	if err != nil {
		return nil, err
	}
	steps := []struct {
		name  string
		group uint32
		attr  uint64
		val   uint64
	}{
		{"distributor", kvmDevArmVGICGrpAddr, kvmVGICv2AddrTypeDist, gicdBase},
		{"cpu_interface", kvmDevArmVGICGrpAddr, kvmVGICv2AddrTypeCPU, cpuBase},
		{"nr_irqs", kvmDevArmVGICGrpNrIRQs, 0, uint64(nrIRQs)},
	}
	for _, s := range steps {
		val := s.val
		if err := setDeviceAttr(fd, s.group, s.attr, unsafe.Pointer(&val)); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("GICv2 %s: %w", s.name, err)
		}
	}
	if err := setDeviceAttr(fd, kvmDevArmVGICGrpCtrl, kvmDevArmVGICCtrlInit, nil); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("GICv2 ctrl init: %w", err)
	}
	return &GICDevice{fd: fd, version: 2}, nil
}

func (vm *VM) createDevice(devType uint32) (int, error) {
	data := kvmCreateDeviceData{Type: devType}
	_, err := vmIoctl(vm.fd, kvmCreateDevice, uintptr(unsafe.Pointer(&data)))
	if err != nil {
		return 0, fmt.Errorf("KVM_CREATE_DEVICE(type=%d): %w", devType, err)
	}
	return int(data.Fd), nil
}

func setDeviceAttr(fd int, group uint32, attr uint64, addrPtr unsafe.Pointer) error {
	da := kvmDeviceAttr{
		Group: group,
		Attr:  attr,
		Addr:  uint64(uintptr(addrPtr)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), kvmSetDeviceAttr, uintptr(unsafe.Pointer(&da)))
	if errno != 0 {
		return errno
	}
	return nil
}

func (g *GICDevice) Close() error {
	if g.fd >= 0 {
		err := unix.Close(g.fd)
		g.fd = -1
		return err
	}
	return nil
}

func (g *GICDevice) Fd() int      { return g.fd }
func (g *GICDevice) Version() int { return g.version }
