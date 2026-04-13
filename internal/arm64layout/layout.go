package arm64layout

const (
	MemoryBase = 0x80000000
	SystemSize = 0x00200000

	MMIO32Start = 0x40000000

	PL011Base = 0x40002000
	PL011Size = 0x00001000
	PL011IRQ  = 1

	GICVersionV2 = 2
	GICVersionV3 = 3

	gicV2DistSize      = 0x00001000
	gicV2CPUSize       = 0x00002000
	gicV3DistSize      = 0x00010000
	gicV3RedistPerVCPU = 0x00020000
)

// GICLayout mirrors the data Firecracker threads from the chosen GIC device
// into both KVM initialization and the guest FDT.
//
// Properties are:
//
//	[0] distributor base
//	[1] distributor size
//	[2] CPU interface base (v2) or redistributor base (v3)
//	[3] CPU interface size (v2) or redistributor size (v3)
type GICLayout struct {
	Version    int
	Compat     string
	MaintIRQ   uint32
	Properties [4]uint64
}

func (g GICLayout) Valid() bool {
	return g.Version == GICVersionV2 || g.Version == GICVersionV3
}

func GICv2() GICLayout {
	distBase := uint64(MMIO32Start - gicV2DistSize)
	cpuBase := distBase - gicV2CPUSize
	return GICLayout{
		Version:  GICVersionV2,
		Compat:   "arm,gic-400",
		MaintIRQ: 8,
		Properties: [4]uint64{
			distBase,
			gicV2DistSize,
			cpuBase,
			gicV2CPUSize,
		},
	}
}

func GICv3(vcpus int) GICLayout {
	if vcpus <= 0 {
		vcpus = 1
	}
	distBase := uint64(MMIO32Start - gicV3DistSize)
	redistSize := uint64(vcpus) * gicV3RedistPerVCPU
	redistBase := distBase - redistSize
	return GICLayout{
		Version:  GICVersionV3,
		Compat:   "arm,gic-v3",
		MaintIRQ: 9,
		Properties: [4]uint64{
			distBase,
			gicV3DistSize,
			redistBase,
			redistSize,
		},
	}
}
