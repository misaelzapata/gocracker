//go:build linux && arm64

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

	kvmRegArm64     = 0x6000000000000000
	kvmRegSizeU64   = 0x0030000000000000
	kvmRegArmCore   = 0x0010 << 16
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
