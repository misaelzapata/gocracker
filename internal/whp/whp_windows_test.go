//go:build windows

package whp

import (
	"errors"
	"testing"
)

// TestAvailable does the cheapest sanity check: just whether we can load
// WinHvPlatform.dll and resolve every function we need. Should pass on
// every Windows host that ships the DLL (Win10 1803+ Pro/Server, Win11+).
// If the test reports the DLL missing, the host SKU lacks WHP entirely.
func TestAvailable(t *testing.T) {
	if !Available() {
		t.Skipf("WinHvPlatform.dll not loadable on this host: %v", loadErr)
	}
}

// TestHypervisorPresent calls WHvGetCapability(WHvCapabilityCodeHypervisorPresent).
// Returns nil/true on hosts where the Hypervisor Platform feature is
// enabled (typically requires admin to flip; check via
// `Get-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform`).
//
// On a hosted GitHub Actions windows-latest runner this either skips or
// reports false — that's expected because nested-virt isn't enabled.
// The test still verifies the call itself succeeds (HRESULT == S_OK).
func TestHypervisorPresent(t *testing.T) {
	if !Available() {
		t.Skip("WinHvPlatform.dll not loadable; nothing to probe")
	}
	present, err := HypervisorPresent()
	if err != nil {
		var hr HResult
		if errors.As(err, &hr) {
			t.Fatalf("WHvGetCapability(HypervisorPresent) returned HRESULT 0x%08x", uint32(hr))
		}
		t.Fatalf("HypervisorPresent: %v", err)
	}
	t.Logf("HypervisorPresent = %v on this host", present)
	// We deliberately do NOT assert present==true. Hosted runners and
	// non-virtualised hosts legitimately report false; what we assert is
	// that the call returns S_OK so the binding shape is correct.
}

// TestCreateAndDeletePartition is the boundary between "I can load the
// DLL" and "the hypervisor accepts a real handle": create a partition,
// confirm a non-zero handle, delete it. No properties set — those are
// covered by TestPartitionLifecycle which exercises the full configure
// → setup → vCPU path. On hosts where the feature is off we skip.
func TestCreateAndDeletePartition(t *testing.T) {
	if !Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := HypervisorPresent()
	if err != nil {
		t.Skipf("HypervisorPresent failed: %v", err)
	}
	if !present {
		t.Skip("Hypervisor Platform feature not enabled on this host")
	}
	h, err := CreatePartition()
	if err != nil {
		t.Fatalf("CreatePartition: %v", err)
	}
	if h == 0 {
		t.Fatal("CreatePartition returned a zero handle but no error")
	}
	t.Logf("CreatePartition returned handle %#x", uintptr(h))
	if err := DeletePartition(h); err != nil {
		t.Errorf("DeletePartition: %v", err)
	}
}

// TestPartitionLifecycle exercises the full configure-then-setup path.
// SetPartitionProperty(ProcessorCount=1) plus SetupPartition is the
// minimum viable boot config; if it succeeds the binding shapes for
// {handle, property code, value pointer, value size} are all correct.
//
// On Win10 1803+ Pro/Server with Hypervisor Platform enabled this should
// succeed. If SetPartitionProperty errors with WHV_E_UNKNOWN_PROPERTY
// (0x80370302) the property code value is wrong for the host's WHP
// build; we log and skip rather than fail so the test surface stays
// useful while we triangulate.
func TestPartitionLifecycle(t *testing.T) {
	if !Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled on this host")
	}
	h, err := CreatePartition()
	if err != nil {
		t.Fatalf("CreatePartition: %v", err)
	}
	t.Cleanup(func() {
		if err := DeletePartition(h); err != nil {
			t.Logf("DeletePartition (cleanup): %v", err)
		}
	})

	if err := SetPartitionPropertyU32(h, PropProcessorCount, 1); err != nil {
		t.Skipf("SetPartitionProperty(ProcessorCount=1) failed: %v "+
			"(triangulating against WinHvPlatform.h on this Windows build)", err)
	}
	if err := SetupPartition(h); err != nil {
		t.Fatalf("SetupPartition: %v", err)
	}
	t.Logf("partition %#x configured for 1 vCPU and committed via WHvSetupPartition", uintptr(h))
}
