//go:build arm64

package kvm

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/gocracker/gocracker/internal/arm64layout"
	"golang.org/x/sys/unix"
)

// KVM device management ioctl numbers (arm64 encodings).
const (
	kvmCreateDevice  = uintptr(0xC00CAEE0) // _IOWR(KVMIO, 0xE0, kvm_create_device)
	kvmSetDeviceAttr = uintptr(0x4018AEE1) // _IOW(KVMIO, 0xE1, kvm_device_attr)
	kvmGetDeviceAttr = uintptr(0x4018AEE2) // _IOW(KVMIO, 0xE2, kvm_device_attr)
	kvmHasDeviceAttr = uintptr(0x4018AEE3) // _IOW(KVMIO, 0xE3, kvm_device_attr)
)

// GIC device types and attribute constants from Linux include/uapi/linux/kvm.h.
const (
	kvmDevTypeArmVGICv2 = 5
	kvmDevTypeArmVGICv3 = 7

	// Attribute groups (shared between v2 and v3)
	kvmDevArmVGICGrpAddr         = 0
	kvmDevArmVGICGrpDistRegs     = 1  // GICv2 distributor / GICv3 distributor
	kvmDevArmVGICGrpCPURegs      = 2  // GICv2 CPU interface (not used on v3)
	kvmDevArmVGICGrpNrIRQs       = 3
	kvmDevArmVGICGrpCtrl         = 4
	kvmDevArmVGICGrpRedistRegs   = 5 // GICv3 redistributor registers
	kvmDevArmVGICGrpCPUSysregs   = 6 // GICv3 ICC_* sysregs
	kvmDevArmVGICGrpLevelInfo    = 7 // per-IRQ line level

	// Control attributes
	kvmDevArmVGICCtrlInit               = 0
	kvmDevArmVGICSavePendingTables      = 1 // GICv3: flush redistributor pending tables to guest memory before save

	// Attr encoding helpers — for DIST_REGS / REDIST_REGS / CPU_SYSREGS the
	// "attr" u64 carries both the vCPU index (for per-vCPU regions) and the
	// offset into that region. Matches KVM_DEV_ARM_VGIC_V3_MPIDR_SHIFT etc.
	kvmDevArmVGICV3MPIDRShift = 32
	kvmDevArmVGICV3MPIDRMask  = uint64(0xFFFFFFFF) << 32
	kvmDevArmVGICOffsetMask   = uint64(0xFFFFFFFF)
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

// ProbeGICLayout selects a concrete GIC device/layout pair that can be fully
// configured on this host. KVM only allows one GIC per VM, so probing uses a
// short-lived scratch VM instead of the real guest.
func (vm *VM) ProbeGICLayout(vcpuCount int, nrIRQs uint32) (arm64layout.GICLayout, error) {
	candidates := []arm64layout.GICLayout{
		arm64layout.GICv3(vcpuCount),
		arm64layout.GICv2(),
	}
	var errs []error
	for _, layout := range candidates {
		if err := vm.probeGICLayout(layout, vcpuCount, nrIRQs); err == nil {
			return layout, nil
		} else {
			errs = append(errs, fmt.Errorf("GICv%d: %w", layout.Version, err))
		}
	}
	return arm64layout.GICLayout{}, fmt.Errorf("GIC init failed (tried v3 and v2): %w", errors.Join(errs...))
}

// CreateGIC creates the selected in-kernel GIC.
func (vm *VM) CreateGIC(layout arm64layout.GICLayout, nrIRQs uint32) (*GICDevice, error) {
	switch layout.Version {
	case arm64layout.GICVersionV3:
		return vm.tryCreateGICv3(layout.Properties[0], layout.Properties[2], nrIRQs)
	case arm64layout.GICVersionV2:
		return vm.tryCreateGICv2(layout.Properties[0], layout.Properties[2], nrIRQs)
	default:
		return nil, fmt.Errorf("unsupported GIC layout version %d", layout.Version)
	}
}

func (vm *VM) probeGICLayout(layout arm64layout.GICLayout, vcpuCount int, nrIRQs uint32) error {
	if vcpuCount <= 0 {
		vcpuCount = 1
	}
	sys, err := Open()
	if err != nil {
		return err
	}
	defer sys.Close()

	memMB := vm.memSize / (1024 * 1024)
	if memMB == 0 {
		memMB = 64
	}
	probeVM, err := sys.CreateVMWithBase(memMB, vm.guestPhysBase)
	if err != nil {
		return err
	}
	defer probeVM.Close()

	probeVCPUs := make([]*VCPU, 0, vcpuCount)
	for i := 0; i < vcpuCount; i++ {
		vcpu, err := probeVM.CreateVCPU(i)
		if err != nil {
			for _, created := range probeVCPUs {
				_ = created.Close()
			}
			return err
		}
		probeVCPUs = append(probeVCPUs, vcpu)
	}
	defer func() {
		for _, vcpu := range probeVCPUs {
			_ = vcpu.Close()
		}
	}()

	gic, err := probeVM.CreateGIC(layout, nrIRQs)
	if err != nil {
		return err
	}
	return gic.Close()
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

func getDeviceAttr(fd int, group uint32, attr uint64, addrPtr unsafe.Pointer) error {
	da := kvmDeviceAttr{
		Group: group,
		Attr:  attr,
		Addr:  uint64(uintptr(addrPtr)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), kvmGetDeviceAttr, uintptr(unsafe.Pointer(&da)))
	if errno != 0 {
		return errno
	}
	return nil
}

// GetU32Attr reads a 32-bit GIC device attribute. Thin wrapper around
// KVM_GET_DEVICE_ATTR for callers in the vmm package (GIC snapshot/restore).
func (g *GICDevice) GetU32Attr(group uint32, attr uint64) (uint32, error) {
	if g == nil || g.fd < 0 {
		return 0, errors.New("GIC device is closed")
	}
	var v uint32
	if err := getDeviceAttr(g.fd, group, attr, unsafe.Pointer(&v)); err != nil {
		return 0, fmt.Errorf("KVM_GET_DEVICE_ATTR(group=%d,attr=%#x): %w", group, attr, err)
	}
	return v, nil
}

// SetU32Attr writes a 32-bit GIC device attribute.
func (g *GICDevice) SetU32Attr(group uint32, attr uint64, value uint32) error {
	if g == nil || g.fd < 0 {
		return errors.New("GIC device is closed")
	}
	v := value
	if err := setDeviceAttr(g.fd, group, attr, unsafe.Pointer(&v)); err != nil {
		return fmt.Errorf("KVM_SET_DEVICE_ATTR(group=%d,attr=%#x): %w", group, attr, err)
	}
	return nil
}

// GetU64Attr reads a 64-bit GIC device attribute.
func (g *GICDevice) GetU64Attr(group uint32, attr uint64) (uint64, error) {
	if g == nil || g.fd < 0 {
		return 0, errors.New("GIC device is closed")
	}
	var v uint64
	if err := getDeviceAttr(g.fd, group, attr, unsafe.Pointer(&v)); err != nil {
		return 0, fmt.Errorf("KVM_GET_DEVICE_ATTR(group=%d,attr=%#x): %w", group, attr, err)
	}
	return v, nil
}

// SetU64Attr writes a 64-bit GIC device attribute.
func (g *GICDevice) SetU64Attr(group uint32, attr uint64, value uint64) error {
	if g == nil || g.fd < 0 {
		return errors.New("GIC device is closed")
	}
	v := value
	if err := setDeviceAttr(g.fd, group, attr, unsafe.Pointer(&v)); err != nil {
		return fmt.Errorf("KVM_SET_DEVICE_ATTR(group=%d,attr=%#x): %w", group, attr, err)
	}
	return nil
}

// CallCtrl invokes a control operation on the GIC device (e.g. flushing
// pending tables to guest memory before snapshot).
func (g *GICDevice) CallCtrl(attr uint64) error {
	if g == nil || g.fd < 0 {
		return errors.New("GIC device is closed")
	}
	if err := setDeviceAttr(g.fd, kvmDevArmVGICGrpCtrl, attr, nil); err != nil {
		return fmt.Errorf("KVM_SET_DEVICE_ATTR(ctrl=%d): %w", attr, err)
	}
	return nil
}

// VGIC group/attribute constants re-exported for pkg/vmm snapshot code.
const (
	VGICGrpDistRegs          = kvmDevArmVGICGrpDistRegs
	VGICGrpRedistRegs        = kvmDevArmVGICGrpRedistRegs
	VGICGrpCPUSysregs        = kvmDevArmVGICGrpCPUSysregs
	VGICGrpLevelInfo         = kvmDevArmVGICGrpLevelInfo
	VGICCtrlSavePendingTable = kvmDevArmVGICSavePendingTables
	VGICV3MPIDRShift         = kvmDevArmVGICV3MPIDRShift
	VGICOffsetMask           = kvmDevArmVGICOffsetMask
)

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
