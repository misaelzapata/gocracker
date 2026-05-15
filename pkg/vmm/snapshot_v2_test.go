package vmm

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestSnapshotV2RoundTrip marshals a fully populated envelope, parses
// it back, and verifies every field survives. ExtendedState is opaque
// so it's tested as a byte-slice equality.
func TestSnapshotV2RoundTrip(t *testing.T) {
	original := &SnapshotV2{
		Hypervisor: SnapshotHypervisorKVM,
		Arch:       SnapshotArchAMD64,
		ID:         "vm-roundtrip",
		VCPUs: []PortableVCPUState{
			{
				Index: 0,
				GPRs: Registers{
					RAX: 0xdeadbeef, RBX: 0x1234, RIP: 0xffff_8000_0000_1000,
					RFLAGS: 0x202,
				},
				Sregs: SegmentRegisters{
					CR0: 0x80050033, CR3: 0x1000, CR4: 0x20,
					EFER: 0xd01,
					CS: Segment{Base: 0, Limit: 0xffffffff, Selector: 0x08, Present: 1, S: 1, L: 1, G: 1},
				},
				ExtendedState: json.RawMessage(`{"mp_state":{"state":0},"tsc_khz":2500000}`),
			},
			{
				Index: 1,
				GPRs: Registers{
					RAX: 0xcafebabe,
					RIP: 0xffff_8000_0000_2000,
				},
				ExtendedState: json.RawMessage(`{"mp_state":{"state":2}}`),
			},
		},
		Devices: []DeviceSnapshotState{
			{
				ID:      "blk0",
				Kind:    "virtio-blk",
				Payload: json.RawMessage(`{"capacity_bytes":1073741824}`),
			},
		},
		Memory: []MemRegionSnapshot{
			{GPA: 0x0, Size: 128 * 1024 * 1024, DataFile: "mem.bin", Flags: 0x7},
		},
		Meta: map[string]string{
			"capture_host":  "host-a",
			"capture_epoch": "1715800000",
		},
	}

	data, err := MarshalSnapshotV2(original)
	if err != nil {
		t.Fatalf("MarshalSnapshotV2: %v", err)
	}
	if !bytes.Contains(data, []byte(`"version": 2`)) {
		t.Fatalf("marshal output missing version=2 header: %s", data)
	}
	if !bytes.Contains(data, []byte(`"hypervisor": "kvm"`)) {
		t.Fatalf("marshal output missing hypervisor=kvm: %s", data)
	}

	parsed, err := UnmarshalSnapshotV2(data)
	if err != nil {
		t.Fatalf("UnmarshalSnapshotV2: %v", err)
	}

	if parsed.Version != SnapshotV2Version {
		t.Fatalf("Version = %d, want %d", parsed.Version, SnapshotV2Version)
	}
	if parsed.Hypervisor != original.Hypervisor {
		t.Fatalf("Hypervisor = %q, want %q", parsed.Hypervisor, original.Hypervisor)
	}
	if parsed.Arch != original.Arch {
		t.Fatalf("Arch = %q, want %q", parsed.Arch, original.Arch)
	}
	if parsed.ID != original.ID {
		t.Fatalf("ID = %q, want %q", parsed.ID, original.ID)
	}
	if len(parsed.VCPUs) != len(original.VCPUs) {
		t.Fatalf("VCPUs len = %d, want %d", len(parsed.VCPUs), len(original.VCPUs))
	}
	for i, want := range original.VCPUs {
		got := parsed.VCPUs[i]
		if got.Index != want.Index {
			t.Errorf("vcpu[%d].Index = %d, want %d", i, got.Index, want.Index)
		}
		if got.GPRs != want.GPRs {
			t.Errorf("vcpu[%d].GPRs mismatch:\n got  %+v\n want %+v", i, got.GPRs, want.GPRs)
		}
		if got.Sregs != want.Sregs {
			t.Errorf("vcpu[%d].Sregs mismatch", i)
		}
		// ExtendedState is opaque but must round-trip byte-for-byte
		// after JSON re-encoding (modulo whitespace), so compare the
		// canonicalised parse.
		var gotBlob, wantBlob map[string]any
		if err := json.Unmarshal(got.ExtendedState, &gotBlob); err != nil {
			t.Fatalf("vcpu[%d] ExtendedState parse: %v", i, err)
		}
		if err := json.Unmarshal(want.ExtendedState, &wantBlob); err != nil {
			t.Fatalf("vcpu[%d] reference ExtendedState parse: %v", i, err)
		}
		if !reflect.DeepEqual(gotBlob, wantBlob) {
			t.Errorf("vcpu[%d].ExtendedState mismatch:\n got  %v\n want %v", i, gotBlob, wantBlob)
		}
	}
	// Devices.Payload is json.RawMessage, so MarshalIndent reformats it
	// during emit. Compare structurally rather than byte-for-byte.
	if len(parsed.Devices) != len(original.Devices) {
		t.Fatalf("Devices len = %d, want %d", len(parsed.Devices), len(original.Devices))
	}
	for i, want := range original.Devices {
		got := parsed.Devices[i]
		if got.ID != want.ID || got.Kind != want.Kind {
			t.Errorf("Devices[%d] header mismatch:\n got  %+v\n want %+v", i, got, want)
		}
		var gotPayload, wantPayload map[string]any
		if err := json.Unmarshal(got.Payload, &gotPayload); err != nil {
			t.Fatalf("Devices[%d] payload parse: %v", i, err)
		}
		if err := json.Unmarshal(want.Payload, &wantPayload); err != nil {
			t.Fatalf("Devices[%d] reference payload parse: %v", i, err)
		}
		if !reflect.DeepEqual(gotPayload, wantPayload) {
			t.Errorf("Devices[%d].Payload mismatch:\n got  %v\n want %v", i, gotPayload, wantPayload)
		}
	}
	if !reflect.DeepEqual(parsed.Memory, original.Memory) {
		t.Errorf("Memory round-trip mismatch:\n got  %+v\n want %+v", parsed.Memory, original.Memory)
	}
	if !reflect.DeepEqual(parsed.Meta, original.Meta) {
		t.Errorf("Meta round-trip mismatch:\n got  %+v\n want %+v", parsed.Meta, original.Meta)
	}
}

// TestProbeSnapshotVersionV2 verifies the probe identifies a v2
// envelope by the combination of Version==2 AND non-empty Hypervisor.
func TestProbeSnapshotVersionV2(t *testing.T) {
	v2 := &SnapshotV2{
		Hypervisor: SnapshotHypervisorKVM,
		Arch:       SnapshotArchAMD64,
	}
	data, err := MarshalSnapshotV2(v2)
	if err != nil {
		t.Fatalf("MarshalSnapshotV2: %v", err)
	}
	got, err := ProbeSnapshotBytes(data)
	if err != nil {
		t.Fatalf("ProbeSnapshotBytes: %v", err)
	}
	if got != SnapshotFormatV2 {
		t.Fatalf("ProbeSnapshotBytes = %d, want %d (v2)", got, SnapshotFormatV2)
	}
}

// TestProbeSnapshotVersionLegacy verifies the probe treats anything
// without a Hypervisor field — including v3 legacy snapshots and
// older v1/v2 legacy fixtures — as the legacy format. This is the
// critical guarantee: existing snapshots on disk must keep reading
// through the v1 code path.
func TestProbeSnapshotVersionLegacy(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "v3-legacy-current",
			body: `{"version":3,"id":"legacy","mem_file":"mem.bin"}`,
		},
		{
			name: "v2-legacy-fixture-without-hypervisor",
			body: `{"version":2,"id":"legacy","mem_file":"mem.bin"}`,
		},
		{
			name: "v1-legacy-fixture",
			body: `{"version":1,"id":"legacy","mem_file":"mem.bin"}`,
		},
		{
			name: "no-version-field",
			body: `{"id":"legacy","mem_file":"mem.bin"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProbeSnapshotBytes([]byte(tc.body))
			if err != nil {
				t.Fatalf("ProbeSnapshotBytes: %v", err)
			}
			if got != SnapshotFormatLegacy {
				t.Fatalf("ProbeSnapshotBytes = %d, want %d (legacy)", got, SnapshotFormatLegacy)
			}
		})
	}
}

// TestProbeSnapshotVersionErrors covers the error cases the probe
// must reject: nil reader and unparseable JSON.
func TestProbeSnapshotVersionErrors(t *testing.T) {
	if _, err := probeSnapshotVersion(nil); err == nil {
		t.Fatal("probeSnapshotVersion(nil) returned no error")
	}
	if _, err := ProbeSnapshotBytes([]byte("not-json")); err == nil {
		t.Fatal("ProbeSnapshotBytes on garbage returned no error")
	}
}

// TestMarshalSnapshotV2EnforcesInvariants checks the encoder refuses
// to emit a half-built envelope.
func TestMarshalSnapshotV2EnforcesInvariants(t *testing.T) {
	if _, err := MarshalSnapshotV2(nil); err == nil {
		t.Fatal("MarshalSnapshotV2(nil) returned no error")
	}
	if _, err := MarshalSnapshotV2(&SnapshotV2{Arch: SnapshotArchAMD64}); err == nil {
		t.Fatal("MarshalSnapshotV2 with empty Hypervisor returned no error")
	}

	// Forced Version override: even if caller passes a wrong number
	// the marshal output must contain the canonical SnapshotV2Version.
	envelope := &SnapshotV2{
		Version:    99,
		Hypervisor: SnapshotHypervisorWHP,
		Arch:       SnapshotArchAMD64,
	}
	data, err := MarshalSnapshotV2(envelope)
	if err != nil {
		t.Fatalf("MarshalSnapshotV2: %v", err)
	}
	if !bytes.Contains(data, []byte(`"version": 2`)) {
		t.Fatalf("marshaler did not force version=2, got: %s", data)
	}
}

// TestUnmarshalSnapshotV2RejectsLegacy ensures a legacy payload (no
// Hypervisor field, or Version != 2) fails the strict decoder. The
// tolerant entry point for migration code is probeSnapshotVersion +
// decodeSnapshotV2; the strict UnmarshalSnapshotV2 is for callers
// that already know they hold a v2 document.
func TestUnmarshalSnapshotV2RejectsLegacy(t *testing.T) {
	cases := []string{
		`{"version":3,"id":"x"}`,                              // wrong version, no hypervisor
		`{"version":2,"id":"x"}`,                              // right version but no hypervisor
		`{"version":2,"hypervisor":"","arch":"amd64","id":"x"}`, // explicit empty hypervisor
	}
	for _, body := range cases {
		if _, err := UnmarshalSnapshotV2([]byte(body)); err == nil {
			t.Fatalf("UnmarshalSnapshotV2(%q) returned no error", body)
		}
	}

	if _, err := UnmarshalSnapshotV2(nil); err == nil {
		t.Fatal("UnmarshalSnapshotV2(nil) returned no error")
	}
	if _, err := UnmarshalSnapshotV2([]byte(``)); err == nil {
		t.Fatal("UnmarshalSnapshotV2(empty) returned no error")
	}
}

// TestSnapshotV2OmitsEmptyOptionalFields verifies the json:omitempty
// hints actually kick in — a minimal envelope should not carry empty
// arrays / maps / blob fields. Keeps the on-disk format tight so
// later schema additions stay backward-compatible.
func TestSnapshotV2OmitsEmptyOptionalFields(t *testing.T) {
	envelope := &SnapshotV2{
		Hypervisor: SnapshotHypervisorKVM,
		Arch:       SnapshotArchAMD64,
	}
	data, err := MarshalSnapshotV2(envelope)
	if err != nil {
		t.Fatalf("MarshalSnapshotV2: %v", err)
	}
	for _, key := range []string{`"vcpus"`, `"devices"`, `"memory_layout"`, `"meta"`} {
		if strings.Contains(string(data), key) {
			t.Errorf("expected %s to be omitted from minimal envelope, got: %s", key, data)
		}
	}
}
