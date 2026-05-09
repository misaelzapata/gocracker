//go:build windows

package vmm

// On Windows, NewKVMHypervisor errors — KVM is Linux-only. Phase 2 adds
// NewWHPHypervisor() which dynamically loads WinHvPlatform.dll. Callers
// should select the backend by GOOS or by trying WHP first and falling
// back; a concrete `DefaultHypervisor()` helper lands once both
// adapters are wired up.

func NewKVMHypervisor() (Hypervisor, error) {
	return nil, ErrUnsupportedHV{Reason: "KVM is Linux-only; use NewWHPHypervisor on Windows (Phase 2)"}
}
