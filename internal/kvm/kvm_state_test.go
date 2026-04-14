//go:build linux

package kvm

import (
	"errors"
	"reflect"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// retryEINTR reissues fn while it returns EINTR. Several KVM ioctls (notably
// KVM_CREATE_VM on older kernels) can be interrupted by Go's runtime preemption
// signals before the work commits; userspace is expected to retry.
func retryEINTR[T any](fn func() (T, error)) (T, error) {
	for {
		v, err := fn()
		if err == nil || !errors.Is(err, unix.EINTR) {
			return v, err
		}
	}
}

// newTestVCPU brings up a tiny VM + one vCPU for ioctl round-trip tests, or
// skips the test cleanly if KVM isn't available (CI runners, sandboxes, etc).
//
// Locks the calling goroutine to its OS thread so the Go runtime's preemption
// signals don't EINTR the KVM ioctls underneath us during setup.
func newTestVCPU(t *testing.T) (*System, *VM, *VCPU) {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)

	sys, err := Open()
	if err != nil {
		t.Skipf("KVM unavailable: %v", err)
	}
	vm, err := retryEINTR(func() (*VM, error) { return sys.CreateVM(8) })
	if err != nil {
		_ = sys.Close()
		t.Skipf("KVM_CREATE_VM unavailable: %v", err)
	}
	vcpu, err := vm.CreateVCPU(0)
	if err != nil {
		_ = vm.Close()
		_ = sys.Close()
		t.Skipf("KVM_CREATE_VCPU unavailable: %v", err)
	}
	// CPUID must be programmed before XSAVE/XCRS/MSRs work meaningfully.
	if err := SetupCPUID(sys, vcpu); err != nil {
		t.Logf("SetupCPUID: %v (continuing; some sub-tests may skip)", err)
	}
	t.Cleanup(func() {
		_ = vcpu.Close()
		_ = vm.Close()
		_ = sys.Close()
	})
	return sys, vm, vcpu
}

func TestStructSizes(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"FPUState", unsafe.Sizeof(FPUState{}), 0x1A0},
		{"XSaveState", unsafe.Sizeof(XSaveState{}), 4096},
		{"XCRsState", unsafe.Sizeof(XCRsState{}), 0x188},
		{"VCPUEvents", unsafe.Sizeof(VCPUEvents{}), 64},
		{"DebugRegs", unsafe.Sizeof(DebugRegs{}), 0x80},
		{"MSREntry", unsafe.Sizeof(MSREntry{}), 16},
		{"XCR", unsafe.Sizeof(XCR{}), 16},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestMSRsRoundTrip(t *testing.T) {
	_, _, vcpu := newTestVCPU(t)
	// Configure default boot MSRs so the kernel has values to read back.
	if err := SetupMSRs(vcpu); err != nil {
		t.Fatalf("SetupMSRs: %v", err)
	}
	indices := SnapshotMSRIndices()
	got1, err := vcpu.GetMSRs(indices)
	if err != nil {
		t.Fatalf("GetMSRs: %v", err)
	}
	if len(got1) == 0 {
		t.Fatalf("GetMSRs returned 0 entries")
	}
	// Round-trip: SET what we got, GET again, expect identical Data values.
	if err := vcpu.SetMSRs(got1); err != nil {
		t.Fatalf("SetMSRs: %v", err)
	}
	got2, err := vcpu.GetMSRs(indices[:len(got1)])
	if err != nil {
		t.Fatalf("GetMSRs (round 2): %v", err)
	}
	if len(got2) != len(got1) {
		t.Fatalf("round-trip length mismatch: got1=%d got2=%d", len(got1), len(got2))
	}
	for i := range got1 {
		if got1[i].Index != got2[i].Index {
			t.Errorf("MSR index mismatch at %d: idx1=%#x idx2=%#x", i, got1[i].Index, got2[i].Index)
			continue
		}
		// Skip MSRs that are inherently time-varying — comparing raw values is
		// not meaningful between two reads even after a perfect round-trip.
		switch got1[i].Index {
		case 0x10, 0xC0000103: // IA32_TSC, MSR_TSC_AUX (TSC-derived on some kernels)
			continue
		case 0x4B564D01, 0x12: // MSR_KVM_SYSTEM_TIME(_NEW): pointer+enable bits, kernel may rewrite
			continue
		}
		if got1[i].Data != got2[i].Data {
			t.Errorf("MSR %#x mismatch: data1=%#x data2=%#x", got1[i].Index, got1[i].Data, got2[i].Data)
		}
	}
}

func TestFPURoundTrip(t *testing.T) {
	_, _, vcpu := newTestVCPU(t)
	if err := SetupFPU(vcpu); err != nil {
		t.Fatalf("SetupFPU: %v", err)
	}
	got1, err := vcpu.GetFPU()
	if err != nil {
		t.Fatalf("GetFPU: %v", err)
	}
	if got1.FCW == 0 && got1.MXCSR == 0 {
		t.Errorf("GetFPU returned zeroed state (FCW=%#x MXCSR=%#x)", got1.FCW, got1.MXCSR)
	}
	if err := vcpu.SetFPUState(got1); err != nil {
		t.Fatalf("SetFPUState: %v", err)
	}
	got2, err := vcpu.GetFPU()
	if err != nil {
		t.Fatalf("GetFPU (round 2): %v", err)
	}
	if got1 != got2 {
		t.Errorf("FPU round-trip mismatch")
	}
}

func TestXSAVERoundTrip(t *testing.T) {
	_, _, vcpu := newTestVCPU(t)
	got1, err := vcpu.GetXSAVE()
	if err != nil {
		t.Skipf("KVM_GET_XSAVE unavailable: %v", err)
	}
	if err := vcpu.SetXSAVE(got1); err != nil {
		t.Fatalf("SetXSAVE: %v", err)
	}
	got2, err := vcpu.GetXSAVE()
	if err != nil {
		t.Fatalf("GetXSAVE (round 2): %v", err)
	}
	if got1 != got2 {
		t.Errorf("XSAVE round-trip mismatch")
	}
}

func TestXCRSRoundTrip(t *testing.T) {
	_, _, vcpu := newTestVCPU(t)
	got1, err := vcpu.GetXCRS()
	if err != nil {
		t.Skipf("KVM_GET_XCRS unavailable: %v", err)
	}
	if err := vcpu.SetXCRS(got1); err != nil {
		t.Fatalf("SetXCRS: %v", err)
	}
	got2, err := vcpu.GetXCRS()
	if err != nil {
		t.Fatalf("GetXCRS (round 2): %v", err)
	}
	if !reflect.DeepEqual(got1, got2) {
		t.Errorf("XCRS round-trip mismatch:\ngot1=%+v\ngot2=%+v", got1, got2)
	}
}

func TestVCPUEventsRoundTrip(t *testing.T) {
	_, _, vcpu := newTestVCPU(t)
	got1, err := vcpu.GetVCPUEvents()
	if err != nil {
		t.Fatalf("GetVCPUEvents: %v", err)
	}
	if err := vcpu.SetVCPUEvents(got1); err != nil {
		t.Fatalf("SetVCPUEvents: %v", err)
	}
	got2, err := vcpu.GetVCPUEvents()
	if err != nil {
		t.Fatalf("GetVCPUEvents (round 2): %v", err)
	}
	// Note: the Flags field on read echoes the kernel's view of which sub-fields
	// are valid; on write the kernel may toggle bits like KVM_VCPUEVENT_VALID_*
	// that aren't strictly user-controlled. We compare only the user-meaningful
	// state — pending/injected exceptions, nmi, smi, sipi, payload.
	if got1.Exception != got2.Exception ||
		got1.Interrupt != got2.Interrupt ||
		got1.NMI != got2.NMI ||
		got1.SMI != got2.SMI ||
		got1.SIPIVector != got2.SIPIVector ||
		got1.TripleFault != got2.TripleFault ||
		got1.ExceptionHasPayload != got2.ExceptionHasPayload ||
		got1.ExceptionPayload != got2.ExceptionPayload {
		t.Errorf("VCPUEvents round-trip mismatch:\ngot1=%+v\ngot2=%+v", got1, got2)
	}
}

func TestDebugRegsRoundTrip(t *testing.T) {
	_, _, vcpu := newTestVCPU(t)
	got1, err := vcpu.GetDebugRegs()
	if err != nil {
		t.Fatalf("GetDebugRegs: %v", err)
	}
	if err := vcpu.SetDebugRegs(got1); err != nil {
		t.Fatalf("SetDebugRegs: %v", err)
	}
	got2, err := vcpu.GetDebugRegs()
	if err != nil {
		t.Fatalf("GetDebugRegs (round 2): %v", err)
	}
	if got1 != got2 {
		t.Errorf("DebugRegs round-trip mismatch:\ngot1=%+v\ngot2=%+v", got1, got2)
	}
}

func TestTSCKHzRoundTrip(t *testing.T) {
	_, _, vcpu := newTestVCPU(t)
	khz, err := vcpu.GetTSCKHz()
	if err != nil {
		// KVM_CAP_GET_TSC_KHZ is optional.
		t.Skipf("KVM_GET_TSC_KHZ unavailable: %v", err)
	}
	if khz == 0 {
		t.Fatalf("GetTSCKHz returned 0")
	}
	if err := vcpu.SetTSCKHz(khz); err != nil {
		// SET_TSC_KHZ may require KVM_CAP_TSC_CONTROL on the host CPU.
		t.Skipf("KVM_SET_TSC_KHZ unavailable: %v", err)
	}
	khz2, err := vcpu.GetTSCKHz()
	if err != nil {
		t.Fatalf("GetTSCKHz (round 2): %v", err)
	}
	if khz != khz2 {
		t.Errorf("TSC khz round-trip mismatch: got1=%d got2=%d", khz, khz2)
	}
}
