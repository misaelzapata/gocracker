package mptable

import "testing"

func TestStartAddrAlignedTo16Bytes(t *testing.T) {
	for _, cpus := range []int{1, 2, 4, 8, 16} {
		if got := StartAddr(cpus) % 16; got != 0 {
			t.Fatalf("StartAddr(%d) alignment = %d, want 0", cpus, got)
		}
	}
}
