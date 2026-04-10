package vmm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/kvm"
)

func TestSnapshotJSONRoundTrip(t *testing.T) {
	snap := Snapshot{
		Version:   2,
		Timestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		ID:        "test-vm",
		Config: Config{
			MemMB:      256,
			Arch:       "amd64",
			KernelPath: "/boot/vmlinuz",
			VCPUs:      2,
		},
		MemFile: "mem.bin",
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal(Snapshot) = %v", err)
	}

	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(Snapshot) = %v", err)
	}

	if decoded.Version != snap.Version {
		t.Errorf("Version = %d, want %d", decoded.Version, snap.Version)
	}
	if decoded.ID != snap.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, snap.ID)
	}
	if decoded.Config.MemMB != snap.Config.MemMB {
		t.Errorf("Config.MemMB = %d, want %d", decoded.Config.MemMB, snap.Config.MemMB)
	}
	if decoded.MemFile != snap.MemFile {
		t.Errorf("MemFile = %q, want %q", decoded.MemFile, snap.MemFile)
	}
}

func TestSnapshotWithVCPUState(t *testing.T) {
	snap := Snapshot{
		Version: 2,
		ID:      "test-vm",
		VCPUs: []VCPUState{
			{ID: 0, Regs: kvm.Regs{RAX: 42, RIP: 0x100000}},
		},
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal = %v", err)
	}

	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal = %v", err)
	}

	if len(decoded.VCPUs) != 1 {
		t.Fatalf("VCPUs len = %d, want 1", len(decoded.VCPUs))
	}
	if decoded.VCPUs[0].Regs.RAX != 42 {
		t.Errorf("VCPUs[0].Regs.RAX = %d, want 42", decoded.VCPUs[0].Regs.RAX)
	}
	if decoded.VCPUs[0].Regs.RIP != 0x100000 {
		t.Errorf("VCPUs[0].Regs.RIP = %#x, want 0x100000", decoded.VCPUs[0].Regs.RIP)
	}
}
