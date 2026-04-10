package vmm

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gocracker/gocracker/internal/kvm"
)

func TestRewriteSnapshotBundleCopiesAssetsAndRewritesPaths(t *testing.T) {
	dir := t.TempDir()
	kernel := writeTestAsset(t, dir, "vmlinux", "kernel")
	initrd := writeTestAsset(t, dir, "initrd.img", "initrd")
	disk := writeTestAsset(t, dir, "disk.ext4", "disk")

	snap := Snapshot{
		Version: 3,
		ID:      "vm-test",
		Config: Config{
			ID:         "vm-test",
			KernelPath: kernel,
			InitrdPath: initrd,
			DiskImage:  disk,
		},
		MemFile: "mem.bin",
	}
	if err := os.WriteFile(filepath.Join(dir, "mem.bin"), []byte("mem"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotJSON(dir, snap); err != nil {
		t.Fatal(err)
	}

	rewritten, err := rewriteSnapshotBundle(dir)
	if err != nil {
		t.Fatalf("rewriteSnapshotBundle: %v", err)
	}

	if rewritten.Config.KernelPath != "artifacts/kernel" {
		t.Fatalf("kernel path = %q", rewritten.Config.KernelPath)
	}
	if rewritten.Config.InitrdPath != "artifacts/initrd" {
		t.Fatalf("initrd path = %q", rewritten.Config.InitrdPath)
	}
	if rewritten.Config.DiskImage != "artifacts/disk.ext4" {
		t.Fatalf("disk path = %q", rewritten.Config.DiskImage)
	}
	for rel, want := range map[string]string{
		"artifacts/kernel":    "kernel",
		"artifacts/initrd":    "initrd",
		"artifacts/disk.ext4": "disk",
	} {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", rel, string(data), want)
		}
	}
}

func TestResolveSnapshotPath(t *testing.T) {
	base := "/tmp/snapshot"
	if got := resolveSnapshotPath(base, "mem.bin"); got != filepath.Join(base, "mem.bin") {
		t.Fatalf("relative path resolved to %q", got)
	}
	if got := resolveSnapshotPath(base, "/abs/mem.bin"); got != "/abs/mem.bin" {
		t.Fatalf("absolute path resolved to %q", got)
	}
}

func TestRestoreFromSnapshotRejectsCrossArchBeforeKVM(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot{
		Version: 3,
		ID:      "vm-test",
		Config: Config{
			ID:         "vm-test",
			Arch:       string(ArchARM64),
			MemMB:      128,
			KernelPath: "vmlinux",
			InitrdPath: "initrd.img",
			DiskImage:  "disk.ext4",
			VCPUs:      1,
		},
		MemFile: "mem.bin",
	}
	if err := os.WriteFile(filepath.Join(dir, "mem.bin"), []byte("mem"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vmlinux"), []byte("kernel"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "initrd.img"), []byte("initrd"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.ext4"), []byte("disk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotJSON(dir, snap); err != nil {
		t.Fatal(err)
	}

	if _, err := RestoreFromSnapshotWithOptions(dir, RestoreOptions{}); err == nil {
		t.Fatal("RestoreFromSnapshotWithOptions() error = nil, want cross-arch rejection")
	}
}

func TestSnapshotArchStateNormalizesLegacyX86(t *testing.T) {
	state := (&SnapshotArchState{
		Clock:    kvm.ClockData{Clock: 123},
		PIT2:     kvm.PITState2{Data: [112]byte{1}},
		IRQChips: []kvm.IRQChip{{ChipID: kvm.IRQChipIOAPIC}},
	}).normalizedX86()
	if state == nil {
		t.Fatal("normalizedX86() = nil")
	}
	if state.Clock.Clock != 123 {
		t.Fatalf("clock = %d, want 123", state.Clock.Clock)
	}
	if len(state.IRQChips) != 1 || state.IRQChips[0].ChipID != kvm.IRQChipIOAPIC {
		t.Fatalf("irqchips = %#v", state.IRQChips)
	}
}

func TestVCPUStateNormalizesLegacyX86(t *testing.T) {
	state := (VCPUState{
		ID:      0,
		Regs:    kvm.Regs{RIP: 0x1234},
		Sregs:   kvm.Sregs{CR3: 0x2000},
		MPState: kvm.MPState{State: kvm.MPStateRunnable},
	}).normalizedX86()
	if state.Regs.RIP != 0x1234 {
		t.Fatalf("RIP = %#x, want %#x", state.Regs.RIP, uint64(0x1234))
	}
	if state.Sregs.CR3 != 0x2000 {
		t.Fatalf("CR3 = %#x, want %#x", state.Sregs.CR3, uint64(0x2000))
	}
	if state.MPState.State != kvm.MPStateRunnable {
		t.Fatalf("mp_state = %d, want %d", state.MPState.State, kvm.MPStateRunnable)
	}
}

func TestSnapshotJSONCarriesExplicitX86ArchState(t *testing.T) {
	snap := Snapshot{
		Version: 3,
		ID:      "vm-test",
		Config:  Config{Arch: string(ArchAMD64)},
		VCPUs: []VCPUState{
			newX86VCPUState(0, kvm.Regs{RIP: 0x1000}, kvm.Sregs{CR3: 0x2000}, kvm.MPState{State: kvm.MPStateRunnable}, nil),
		},
		Arch: newX86SnapshotArchState(&X86MachineState{
			Clock: kvm.ClockData{Clock: 42},
		}),
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal(snapshot): %v", err)
	}
	if !bytes.Contains(data, []byte(`"x86"`)) {
		t.Fatalf("snapshot JSON = %s, want explicit x86 arch payload", data)
	}
}

func TestBuildDirtyFilePatchWritesOnlyDirtyRanges(t *testing.T) {
	pageSize := uint64(4)
	src := []byte("AAAABBBBCCCCDDDD")
	bitmap := []uint64{0b1010}
	var out bytes.Buffer
	var next uint64

	patch, err := buildDirtyFilePatch(&out, bytes.NewReader(src), uint64(len(src)), "mem.bin", pageSize, bitmap, &next)
	if err != nil {
		t.Fatalf("buildDirtyFilePatch: %v", err)
	}
	if len(patch.Entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(patch.Entries))
	}
	if got := string(out.Bytes()); got != "BBBBDDDD" {
		t.Fatalf("patch bytes = %q, want %q", got, "BBBBDDDD")
	}
	if patch.Entries[0].Offset != 4 || patch.Entries[0].Length != 4 || patch.Entries[0].DataOffset != 0 {
		t.Fatalf("entry[0] = %#v", patch.Entries[0])
	}
	if patch.Entries[1].Offset != 12 || patch.Entries[1].Length != 4 || patch.Entries[1].DataOffset != 4 {
		t.Fatalf("entry[1] = %#v", patch.Entries[1])
	}
	if next != 8 {
		t.Fatalf("next data offset = %d, want 8", next)
	}
}

func TestBuildDirtyFilePatchCoalescesContiguousPages(t *testing.T) {
	pageSize := uint64(4)
	src := []byte("AAAABBBBCCCCDDDD")
	bitmap := []uint64{0b0110}
	var out bytes.Buffer
	var next uint64

	patch, err := buildDirtyFilePatch(&out, bytes.NewReader(src), uint64(len(src)), "mem.bin", pageSize, bitmap, &next)
	if err != nil {
		t.Fatalf("buildDirtyFilePatch: %v", err)
	}
	if len(patch.Entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(patch.Entries))
	}
	if got := string(out.Bytes()); got != "BBBBCCCC" {
		t.Fatalf("patch bytes = %q, want %q", got, "BBBBCCCC")
	}
	if patch.Entries[0].Offset != 4 || patch.Entries[0].Length != 8 || patch.Entries[0].DataOffset != 0 {
		t.Fatalf("entry[0] = %#v", patch.Entries[0])
	}
	if next != 8 {
		t.Fatalf("next data offset = %d, want 8", next)
	}
}

func TestBuildDirtyFilePatchUsesSharedDataOffsetAcrossFiles(t *testing.T) {
	pageSize := uint64(4)
	var out bytes.Buffer
	next := uint64(0)

	first, err := buildDirtyFilePatch(&out, bytes.NewReader([]byte("AAAABBBB")), 8, "mem.bin", pageSize, []uint64{0b0011}, &next)
	if err != nil {
		t.Fatalf("first patch: %v", err)
	}
	second, err := buildDirtyFilePatch(&out, bytes.NewReader([]byte("CCCCDDDD")), 8, "disk.ext4", pageSize, []uint64{0b0011}, &next)
	if err != nil {
		t.Fatalf("second patch: %v", err)
	}

	if got := string(out.Bytes()); got != "AAAABBBBCCCCDDDD" {
		t.Fatalf("patch bytes = %q, want %q", got, "AAAABBBBCCCCDDDD")
	}
	if len(first.Entries) != 1 || first.Entries[0].DataOffset != 0 {
		t.Fatalf("first entries = %#v", first.Entries)
	}
	if len(second.Entries) != 1 || second.Entries[0].DataOffset != 8 {
		t.Fatalf("second entries = %#v", second.Entries)
	}
	if next != 16 {
		t.Fatalf("next data offset = %d, want 16", next)
	}
}

func writeTestAsset(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSnapshotJSON(dir string, snap Snapshot) error {
	data, err := jsonMarshalIndent(snap)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "snapshot.json"), data, 0644)
}

func jsonMarshalIndent(snap Snapshot) ([]byte, error) {
	return json.MarshalIndent(snap, "", "  ")
}
