package vmm

import "testing"

func TestARM64SetupVCPUsInParallelDisabled(t *testing.T) {
	if arm64SetupVCPUsInParallel() {
		t.Fatal("ARM64 vCPU setup should remain sequential for the current guest baseline")
	}
}
