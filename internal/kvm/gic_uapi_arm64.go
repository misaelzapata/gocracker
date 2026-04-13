package kvm

// ARM64 VGIC address attributes from Linux asm/kvm.h. Keep these in a
// host-agnostic file so regression tests can validate them in the normal suite.
const (
	kvmVGICv3AddrTypeDist   = 2
	kvmVGICv3AddrTypeRedist = 3

	kvmVGICv2AddrTypeDist = 0
	kvmVGICv2AddrTypeCPU  = 1
)
