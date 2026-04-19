//go:build arm64

package kvm

import (
	"fmt"
	"unsafe"
)

const (
	kvmGetOneReg          = uintptr(0x4010AEAB) // _IOW(KVMIO, 0xAB, struct kvm_one_reg{u64,u64}=16 bytes)
	kvmSetOneReg          = uintptr(0x4010AEAC) // _IOW(KVMIO, 0xAC, struct kvm_one_reg{u64,u64}=16 bytes)
	kvmArmVCPUInit        = uintptr(0x4020AEAE)
	kvmArmPreferredTarget = uintptr(0x8020AEAF)
	kvmGetRegList         = uintptr(0xC008AEB0) // _IOWR(KVMIO, 0xB0, struct kvm_reg_list{u64 n, u64 reg[]})

	kvmRegArm64     = 0x6000000000000000
	kvmRegSizeU64   = 0x0030000000000000
	kvmRegArmCore   = 0x0010 << 16
	kvmRegArmCoproc = 0x000000000FFF0000 // mask isolating the "coproc" field
	kvmRegArmSysreg = 0x0013 << 16       // identifies an aarch64 system register
	arm64PSTATEEL1h = 0x3c5

	KVMArmVCPUPowerOff = 0 // KVM_ARM_VCPU_POWER_OFF — secondary vCPUs start halted
	KVMArmVCPUPSCI02   = 2
)

type ARM64VCPUInit struct {
	Target   uint32
	Features [7]uint32
}

type OneReg struct {
	ID   uint64
	Addr uint64
}

type arm64UserPtRegs struct {
	Regs   [31]uint64
	SP     uint64
	PC     uint64
	PState uint64
}

type arm64KVMRegsLayout struct {
	Regs arm64UserPtRegs
}

var (
	arm64RegsArrayOffset = unsafe.Offsetof(arm64KVMRegsLayout{}.Regs.Regs)
	arm64RegX0           = arm64CoreReg(arm64RegsArrayOffset + 0*8)
	arm64RegX1           = arm64CoreReg(arm64RegsArrayOffset + 1*8)
	arm64RegX2           = arm64CoreReg(arm64RegsArrayOffset + 2*8)
	arm64RegX3           = arm64CoreReg(arm64RegsArrayOffset + 3*8)
	arm64RegPC           = arm64CoreReg(unsafe.Offsetof(arm64KVMRegsLayout{}.Regs.PC))
	arm64RegPState       = arm64CoreReg(unsafe.Offsetof(arm64KVMRegsLayout{}.Regs.PState))
)

func arm64CoreReg(offset uintptr) uint64 {
	return kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | uint64(offset/4)
}

func (vm *VM) PreferredARM64Target() (ARM64VCPUInit, error) {
	var init ARM64VCPUInit
	_, err := vmIoctl(vm.fd, kvmArmPreferredTarget, uintptr(unsafe.Pointer(&init)))
	if err != nil {
		return ARM64VCPUInit{}, fmt.Errorf("KVM_ARM_PREFERRED_TARGET: %w", err)
	}
	return init, nil
}

// DefaultARM64VCPUInit returns a zeroed ARM64VCPUInit with PSCI v0.2 enabled.
// Used during restore when the VM fd is not directly available in the
// restoreVCPU interface. The target field 0 is the generic ARM host target
// which KVM accepts on all ARM64 hosts.
func DefaultARM64VCPUInit() ARM64VCPUInit {
	var init ARM64VCPUInit
	init.Features[0] |= 1 << KVMArmVCPUPSCI02
	return init
}

func (vcpu *VCPU) InitARM64(init ARM64VCPUInit) error {
	if _, err := vmIoctl(vcpu.fd, kvmArmVCPUInit, uintptr(unsafe.Pointer(&init))); err != nil {
		return fmt.Errorf("KVM_ARM_VCPU_INIT: %w", err)
	}
	return nil
}

func (vcpu *VCPU) SetOneReg64(id, value uint64) error {
	val := value
	reg := OneReg{
		ID:   id,
		Addr: uint64(uintptr(unsafe.Pointer(&val))),
	}
	if _, err := vmIoctl(vcpu.fd, kvmSetOneReg, uintptr(unsafe.Pointer(&reg))); err != nil {
		return fmt.Errorf("KVM_SET_ONE_REG(%#x): %w", id, err)
	}
	return nil
}

func (vcpu *VCPU) GetOneReg64(id uint64) (uint64, error) {
	var val uint64
	reg := OneReg{
		ID:   id,
		Addr: uint64(uintptr(unsafe.Pointer(&val))),
	}
	if _, err := vmIoctl(vcpu.fd, kvmGetOneReg, uintptr(unsafe.Pointer(&reg))); err != nil {
		return 0, fmt.Errorf("KVM_GET_ONE_REG(%#x): %w", id, err)
	}
	return val, nil
}

// IsARM64Sysreg reports whether a KVM ONE_REG id refers to an aarch64 system
// register (as opposed to a core reg, coproc, SIMD/FP, etc.).
func IsARM64Sysreg(id uint64) bool {
	return id&kvmRegArmCoproc == kvmRegArmSysreg
}

// IsARM64CoreReg reports whether a KVM ONE_REG id is a "core" register
// (struct user_pt_regs + extensions like sp_el1, elr_el1, spsr, FPSIMD).
// These are captured/restored explicitly by the vmm package and must NOT
// double-capture when iterating the full GetRegList set.
func IsARM64CoreReg(id uint64) bool {
	return id&kvmRegArmCoproc == kvmRegArmCore
}

// ARM64RegSize extracts the size field from a KVM ONE_REG id. The value
// matches the KVM_REG_SIZE_U* width.
func ARM64RegSize(id uint64) uint64 {
	return id & (0xFF << 52)
}

const (
	ARM64RegSizeU32  = uint64(0x0020) << 48 // 0x0020000000000000
	ARM64RegSizeU64  = uint64(0x0030) << 48 // 0x0030000000000000
	ARM64RegSizeU128 = uint64(0x0040) << 48 // 0x0040000000000000
)

// GetOneReg128 reads a 128-bit KVM ONE_REG (used for aarch64 SIMD vregs
// V0..V31). Returns the register value as a [16]byte (little-endian,
// vreg[0] is the low 64 bits).
func (vcpu *VCPU) GetOneReg128(id uint64) ([16]byte, error) {
	var val [16]byte
	reg := OneReg{
		ID:   id,
		Addr: uint64(uintptr(unsafe.Pointer(&val[0]))),
	}
	if _, err := vmIoctl(vcpu.fd, kvmGetOneReg, uintptr(unsafe.Pointer(&reg))); err != nil {
		return val, fmt.Errorf("KVM_GET_ONE_REG(%#x): %w", id, err)
	}
	return val, nil
}

// SetOneReg128 writes a 128-bit KVM ONE_REG.
func (vcpu *VCPU) SetOneReg128(id uint64, val [16]byte) error {
	v := val
	reg := OneReg{
		ID:   id,
		Addr: uint64(uintptr(unsafe.Pointer(&v[0]))),
	}
	if _, err := vmIoctl(vcpu.fd, kvmSetOneReg, uintptr(unsafe.Pointer(&reg))); err != nil {
		return fmt.Errorf("KVM_SET_ONE_REG(%#x): %w", id, err)
	}
	return nil
}

// GetOneReg32 reads a 32-bit KVM ONE_REG (used for FPCR/FPSR).
func (vcpu *VCPU) GetOneReg32(id uint64) (uint32, error) {
	var val uint32
	reg := OneReg{
		ID:   id,
		Addr: uint64(uintptr(unsafe.Pointer(&val))),
	}
	if _, err := vmIoctl(vcpu.fd, kvmGetOneReg, uintptr(unsafe.Pointer(&reg))); err != nil {
		return 0, fmt.Errorf("KVM_GET_ONE_REG(%#x): %w", id, err)
	}
	return val, nil
}

// SetOneReg32 writes a 32-bit KVM ONE_REG.
func (vcpu *VCPU) SetOneReg32(id uint64, value uint32) error {
	val := value
	reg := OneReg{
		ID:   id,
		Addr: uint64(uintptr(unsafe.Pointer(&val))),
	}
	if _, err := vmIoctl(vcpu.fd, kvmSetOneReg, uintptr(unsafe.Pointer(&reg))); err != nil {
		return fmt.Errorf("KVM_SET_ONE_REG(%#x): %w", id, err)
	}
	return nil
}

// GetRegList returns every KVM ONE_REG id that this vCPU currently exposes.
// Used during snapshot capture to enumerate the full sysreg set without
// hardcoding the list — the exact set depends on kernel version and host CPU.
//
// Implementation follows the two-call protocol: first call with n=0 returns
// E2BIG and populates n with the required size; second call allocates the
// right-sized buffer and fills it.
func (vcpu *VCPU) GetRegList() ([]uint64, error) {
	var probe struct {
		N uint64
	}
	_, err := vmIoctl(vcpu.fd, kvmGetRegList, uintptr(unsafe.Pointer(&probe)))
	// The first call is expected to fail with E2BIG when n=0 — the kernel
	// writes the real count into probe.N and returns -1. Any other error is
	// fatal; a nil error with probe.N==0 means there are no regs (also ok).
	if err != nil && probe.N == 0 {
		return nil, fmt.Errorf("KVM_GET_REG_LIST probe: %w", err)
	}
	if probe.N == 0 {
		return nil, nil
	}
	buf := make([]uint64, 1+probe.N)
	buf[0] = probe.N
	if _, err := vmIoctl(vcpu.fd, kvmGetRegList, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return nil, fmt.Errorf("KVM_GET_REG_LIST: %w", err)
	}
	ids := make([]uint64, probe.N)
	copy(ids, buf[1:])
	return ids, nil
}

func (vcpu *VCPU) SetupARM64Boot(entryPoint, dtbAddr uint64) error {
	for _, reg := range []struct {
		id    uint64
		value uint64
	}{
		{arm64RegPC, entryPoint},
		{arm64RegX0, dtbAddr},
		{arm64RegX1, 0},
		{arm64RegX2, 0},
		{arm64RegX3, 0},
		{arm64RegPState, arm64PSTATEEL1h},
	} {
		if err := vcpu.SetOneReg64(reg.id, reg.value); err != nil {
			return err
		}
	}
	return nil
}
