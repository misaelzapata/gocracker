//go:build !linux && !windows

package vmm

// On platforms with no Hypervisor backend (today: anything other than
// linux and — once Phase 2 lands — windows), NewKVMHypervisor returns
// an ErrUnsupportedHV. The shape exists so cross-platform callers can
// pick a backend at startup without compile-time guards.

func NewKVMHypervisor() (Hypervisor, error) {
	return nil, ErrUnsupportedHV{Reason: "KVM is Linux-only on this build"}
}
