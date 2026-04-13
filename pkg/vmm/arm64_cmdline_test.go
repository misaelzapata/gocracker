package vmm

import (
	"strings"
	"testing"
)

func TestNormalizeARM64KernelCmdlineAddsBootConsoleAndEarlycon(t *testing.T) {
	got := normalizeARM64KernelCmdline("console=ttyS0 root=/dev/vda rw")

	for _, want := range []string{
		"console=ttyS0",
		"keep_bootcon",
		arm64EarlyconArg,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalizeARM64KernelCmdline() missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "ttyAMA0") {
		t.Fatalf("normalizeARM64KernelCmdline() unexpectedly introduced ttyAMA0: %q", got)
	}
	if strings.Contains(got, "earlycon=uart8250,mmio32,0x40002000") {
		t.Fatalf("normalizeARM64KernelCmdline() unexpectedly introduced stale 8250/mmio32 earlycon: %q", got)
	}
}

func TestNormalizeARM64KernelCmdlineDoesNotDuplicateExistingArgs(t *testing.T) {
	in := "console=ttyS0 keep_bootcon root=/dev/vda rw " + arm64EarlyconArg
	got := normalizeARM64KernelCmdline(in)

	if strings.Count(got, "keep_bootcon") != 1 {
		t.Fatalf("keep_bootcon duplicated in %q", got)
	}
	if strings.Count(got, arm64EarlyconArg) != 1 {
		t.Fatalf("earlycon duplicated in %q", got)
	}
}

func TestNormalizeARM64KernelCmdlinePreservesUserEarlycon(t *testing.T) {
	in := "console=ttyS0 root=/dev/vda rw earlycon=pl011,mmio32,0x40002000"
	got := normalizeARM64KernelCmdline(in)

	if strings.Count(got, "earlycon=") != 1 {
		t.Fatalf("expected one earlycon in %q", got)
	}
	if !strings.Contains(got, "earlycon=pl011,mmio32,0x40002000") {
		t.Fatalf("expected user earlycon to be preserved in %q", got)
	}
	if strings.Contains(got, arm64EarlyconArg) {
		t.Fatalf("default earlycon should not override user choice in %q", got)
	}
}

func TestNormalizeARM64KernelCmdlineStripsOnlyX86SpecificArgs(t *testing.T) {
	in := "console=ttyS0 i8042.noaux 8250.nr_uarts=0 pci=off swiotlb=noforce"
	got := normalizeARM64KernelCmdline(in)

	for _, unexpected := range []string{"i8042.noaux", "8250.nr_uarts=0", "pci=off"} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("unexpected x86 arg %q left in %q", unexpected, got)
		}
	}
	if !strings.Contains(got, "swiotlb=noforce") {
		t.Fatalf("non-x86 arg removed from %q", got)
	}
}

func TestNormalizeARM64KernelCmdlineDefaultUsesTTYS0(t *testing.T) {
	got := normalizeARM64KernelCmdline("")
	if !strings.Contains(got, "console=ttyS0") {
		t.Fatalf("default ARM64 cmdline missing console=ttyS0 in %q", got)
	}
}
