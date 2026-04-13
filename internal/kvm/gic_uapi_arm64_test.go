package kvm

import "testing"

func TestARM64VGICUAPIConstants(t *testing.T) {
	if kvmVGICv3AddrTypeDist != 2 {
		t.Fatalf("kvmVGICv3AddrTypeDist = %d, want 2", kvmVGICv3AddrTypeDist)
	}
	if kvmVGICv3AddrTypeRedist != 3 {
		t.Fatalf("kvmVGICv3AddrTypeRedist = %d, want 3", kvmVGICv3AddrTypeRedist)
	}
	if kvmVGICv2AddrTypeDist != 0 {
		t.Fatalf("kvmVGICv2AddrTypeDist = %d, want 0", kvmVGICv2AddrTypeDist)
	}
	if kvmVGICv2AddrTypeCPU != 1 {
		t.Fatalf("kvmVGICv2AddrTypeCPU = %d, want 1", kvmVGICv2AddrTypeCPU)
	}
}
