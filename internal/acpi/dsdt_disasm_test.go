package acpi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildDSDT_FourDevicesDisassembles verifies that a DSDT generated with
// 4 virtio-mmio devices (rng + net + blk + fs, the compose-volume config)
// round-trips through iasl's disassembler without errors and contains all
// four V### device nodes. This is the minimum shape the kernel's ACPI
// platform bus needs to enumerate them.
//
// If iasl is not installed the test is skipped.
func TestBuildDSDT_FourDevicesDisassembles(t *testing.T) {
	iasl, err := exec.LookPath("iasl")
	if err != nil {
		t.Skip("iasl not installed; skipping DSDT disassembly test")
	}

	// 4 MMIO devices matching what compose-volume actually produces:
	// rng @ D000_0000 GSI 5, net @ D000_1000 GSI 6, blk @ D000_2000 GSI 7,
	// virtio-fs @ D000_3000 GSI 8. (Slot 0=rng is virtio slot 0 → GSI 5
	// given VirtioIRQBase=5 in pkg/vmm/vmm.go.)
	devices := []MMIODevice{
		{Addr: 0xD0000000, Len: 0x1000, GSI: 5},
		{Addr: 0xD0001000, Len: 0x1000, GSI: 6},
		{Addr: 0xD0002000, Len: 0x1000, GSI: 7},
		{Addr: 0xD0003000, Len: 0x1000, GSI: 8},
	}

	body, err := buildDSDT(devices)
	if err != nil {
		t.Fatalf("buildDSDT: %v", err)
	}
	table := buildDSDTTable(body)

	dir := t.TempDir()
	dsdtPath := filepath.Join(dir, "dsdt.aml")
	if err := os.WriteFile(dsdtPath, table, 0o644); err != nil {
		t.Fatalf("write dsdt.aml: %v", err)
	}

	// iasl -d dsdt.aml → dsdt.dsl (disassembly). Run in tempdir so the
	// output file lands next to the input.
	cmd := exec.Command(iasl, "-d", dsdtPath)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Logf("iasl -d output:\n%s", string(out))
	if err != nil {
		t.Fatalf("iasl disassembly failed: %v\n%s", err, string(out))
	}

	// iasl exits 0 even on warnings; look for "Error" in the output as a
	// stricter check.
	if strings.Contains(string(out), "Error") && !strings.Contains(string(out), "Errors 0") {
		t.Fatalf("iasl reported errors:\n%s", string(out))
	}

	dsl, err := os.ReadFile(filepath.Join(dir, "dsdt.dsl"))
	if err != nil {
		t.Fatalf("read dsdt.dsl: %v", err)
	}
	dslText := string(dsl)
	t.Logf("dsdt.dsl V### count: V000=%d V001=%d V002=%d V003=%d",
		strings.Count(dslText, "Device (V000)"),
		strings.Count(dslText, "Device (V001)"),
		strings.Count(dslText, "Device (V002)"),
		strings.Count(dslText, "Device (V003)"),
	)
	t.Logf("dsdt.dsl (first 8192 bytes):\n%s", truncateLog(dslText, 8192))

	// All four V### devices must be present in the disassembly.
	for _, name := range []string{"V000", "V001", "V002", "V003"} {
		if !strings.Contains(dslText, name) {
			t.Errorf("disassembled DSDT missing device %s", name)
		}
	}
	// Each virtio device should carry the LNRO0005 _HID string.
	hidCount := strings.Count(dslText, "LNRO0005")
	if hidCount != 4 {
		t.Errorf("LNRO0005 _HID count = %d, want 4", hidCount)
	}
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}
