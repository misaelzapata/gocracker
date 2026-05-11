package vmm

import (
	"fmt"
	"runtime"
	"strings"
)

// boot_modes.go intentionally has NO //go:build linux constraint so that
// X86BootMode / MachineArch / their constants are available cross-platform.
// internal/firecrackerapi (validation.go) and Phase 2's Windows-side
// boot path both import these types directly; keeping them in
// vmm.go would force linux-only on those packages too.

// X86BootMode controls how the boot transition to long mode is set up:
// ACPI tables (modern firmware-style), legacy multiboot-style, or
// auto-detect based on the kernel image.
type X86BootMode string

// MachineArch identifies the guest CPU architecture. Today only amd64
// and arm64 are supported; the WHP backend is amd64-only in v1.
type MachineArch string

const (
	X86BootAuto   X86BootMode = "auto"
	X86BootACPI   X86BootMode = "acpi"
	X86BootLegacy X86BootMode = "legacy"

	ArchAMD64 MachineArch = "amd64"
	ArchARM64 MachineArch = "arm64"
)

// normalizeX86BootMode returns mode itself for the known values, the
// default (auto) for the empty string, or an error.
func normalizeX86BootMode(mode X86BootMode) (X86BootMode, error) {
	switch mode {
	case "":
		return X86BootAuto, nil
	case X86BootAuto, X86BootACPI, X86BootLegacy:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid x86 boot mode %q", mode)
	}
}

// HostArch reports the architecture the gocracker binary is running on.
// gocracker treats the host arch as the default guest arch — same-arch
// guests don't need binary translation and boot faster.
func HostArch() MachineArch {
	return MachineArch(runtime.GOARCH)
}

func normalizeMachineArch(raw string) (MachineArch, error) {
	arch := strings.TrimSpace(raw)
	if arch == "" {
		return HostArch(), nil
	}
	switch MachineArch(arch) {
	case ArchAMD64, ArchARM64:
		return MachineArch(arch), nil
	default:
		return "", fmt.Errorf("unsupported arch %q (supported: amd64, arm64)", arch)
	}
}
