//go:build linux

package vmm

import (
	"testing"

	"github.com/gocracker/gocracker/internal/kvm"
)

// TestWriteSnapshotV2BytesEmitsWHPEnvelope is the smoke test for the
// portable-envelope emission path. It builds a minimal v1 Snapshot in
// memory, calls writeSnapshotV2Bytes with the WHP source tag, and
// validates the bytes parse back through UnmarshalSnapshotV2 with the
// expected shape.
//
// The point of the test is the dispatch + envelope construction —
// vcpuStateToPortable already has its own round-trip coverage in
// snapshot_v2_test.go.
func TestWriteSnapshotV2BytesEmitsWHPEnvelope(t *testing.T) {
	snap := &Snapshot{
		Version: 3,
		ID:      "vm-migv2",
		Config:  Config{ID: "vm-migv2", Arch: "amd64"},
		VCPUs: []VCPUState{
			newX86VCPUState(0, X86VCPUState{
				Regs:  kvm.Regs{RIP: 0xffff_8000_0000_1000, RAX: 0xdeadbeef, RFLAGS: 0x202},
				MPState: kvm.MPState{State: 0},
				TSCKHz:  2500000,
			}),
		},
		MemFile: "mem.bin",
	}

	data, err := writeSnapshotV2Bytes(snap, SnapshotHypervisorWHP)
	if err != nil {
		t.Fatalf("writeSnapshotV2Bytes: %v", err)
	}

	format, err := ProbeSnapshotBytes(data)
	if err != nil {
		t.Fatalf("ProbeSnapshotBytes: %v", err)
	}
	if format != SnapshotFormatV2 {
		t.Fatalf("ProbeSnapshotBytes = %d, want %d (v2)", format, SnapshotFormatV2)
	}

	parsed, err := UnmarshalSnapshotV2(data)
	if err != nil {
		t.Fatalf("UnmarshalSnapshotV2: %v", err)
	}
	if parsed.Hypervisor != SnapshotHypervisorWHP {
		t.Fatalf("Hypervisor = %q, want %q", parsed.Hypervisor, SnapshotHypervisorWHP)
	}
	if parsed.Arch != SnapshotArchAMD64 {
		t.Fatalf("Arch = %q, want %q", parsed.Arch, SnapshotArchAMD64)
	}
	if parsed.ID != "vm-migv2" {
		t.Fatalf("ID = %q, want %q", parsed.ID, "vm-migv2")
	}
	if len(parsed.VCPUs) != 1 {
		t.Fatalf("VCPUs len = %d, want 1", len(parsed.VCPUs))
	}
	v0 := parsed.VCPUs[0]
	if v0.Index != 0 {
		t.Fatalf("VCPUs[0].Index = %d, want 0", v0.Index)
	}
	if v0.GPRs.RIP != 0xffff_8000_0000_1000 {
		t.Fatalf("VCPUs[0].GPRs.RIP = %#x, want 0xffff800000001000", v0.GPRs.RIP)
	}
	if v0.GPRs.RAX != 0xdeadbeef {
		t.Fatalf("VCPUs[0].GPRs.RAX = %#x, want 0xdeadbeef", v0.GPRs.RAX)
	}
	if len(v0.ExtendedState) == 0 {
		t.Fatal("VCPUs[0].ExtendedState empty — KVM-source encoder did not populate it")
	}
	if len(parsed.Memory) != 1 {
		t.Fatalf("Memory len = %d, want 1", len(parsed.Memory))
	}
	if parsed.Memory[0].DataFile != "mem.bin" {
		t.Fatalf("Memory[0].DataFile = %q, want mem.bin", parsed.Memory[0].DataFile)
	}
}

// TestMarshalSnapshotForSourceLinuxDefaultsToV1 verifies the dispatcher
// stays v1 on Linux when isWHPSource is false (the live default —
// hypervisorIsWHP is a constant-false stub in the Linux build). This is
// what keeps KVM↔KVM migration bundles byte-identical with older
// gocracker builds. A nil VM is sufficient to exercise the false branch.
func TestMarshalSnapshotForSourceLinuxDefaultsToV1(t *testing.T) {
	snap := &Snapshot{
		Version: 3,
		ID:      "vm-v1-default",
		Config:  Config{ID: "vm-v1-default", Arch: "amd64"},
		MemFile: "mem.bin",
	}

	data, err := marshalSnapshotForSource(snap, nil)
	if err != nil {
		t.Fatalf("marshalSnapshotForSource: %v", err)
	}

	format, err := ProbeSnapshotBytes(data)
	if err != nil {
		t.Fatalf("ProbeSnapshotBytes: %v", err)
	}
	if format != SnapshotFormatLegacy {
		t.Fatalf("ProbeSnapshotBytes = %d, want %d (legacy v1)", format, SnapshotFormatLegacy)
	}
	// The Linux build's hypervisorIsWHP stub must always return false,
	// otherwise the v1↔v1 KVM bundle invariant breaks.
	if hypervisorIsWHP(nil) {
		t.Fatal("hypervisorIsWHP(nil) returned true on Linux; expected stub to be false")
	}
}
