package vmm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// SnapshotV2Version is the on-disk version sentinel for the portable
// snapshot envelope. Stored in SnapshotV2.Version and recognised by
// probeSnapshotVersion.
const SnapshotV2Version uint8 = 2

// SnapshotV2 is the portable snapshot envelope.
//
// Cross-hypervisor restore is OUT OF SCOPE — the v2 format only
// guarantees same-hypervisor save+restore (KVM->KVM or WHP->WHP). MSR
// layouts, vendor-specific feature bits, and in-kernel device state
// (LAPIC/PIT/IRQ chip) differ enough between hypervisors that an
// envelope alone can't bridge the two; what v2 buys is a stable,
// hypervisor-agnostic JSON wrapper that a migration tool can parse on
// any host, route to the matching backend, and restore.
//
// ExtendedState on each PortableVCPUState is an opaque blob the source
// hypervisor knows how to interpret. On KVM source it is the
// JSON-encoded kvm.{FPUState,XSaveState,LAPICState,VCPUEvents,
// DebugRegs,...} chunk. On WHP source it is a (future) WHP-specific
// register-table encoding. Migration tools inspect Hypervisor to know
// how to route.
//
// Hypervisor and Arch are required. Devices is an optional list the
// device emulation layer can populate; the device-specific payload
// shape is kept fully opaque (json.RawMessage) so adding a new device
// kind on one backend doesn't force the other to re-encode.
type SnapshotV2 struct {
	Version    uint8                 `json:"version"`    // always SnapshotV2Version (2)
	Hypervisor string                `json:"hypervisor"` // "kvm" | "whp"
	Arch       string                `json:"arch"`       // "amd64" | "arm64"
	ID         string                `json:"id,omitempty"`
	VCPUs      []PortableVCPUState   `json:"vcpus,omitempty"`
	Devices    []DeviceSnapshotState `json:"devices,omitempty"`
	Memory     []MemRegionSnapshot   `json:"memory_layout,omitempty"`
	Meta       map[string]string     `json:"meta,omitempty"`
}

// PortableVCPUState captures one vCPU in hypervisor-agnostic shape.
// GPRs and Sregs use the portable Registers / SegmentRegisters types
// (defined in hypervisor.go) so a migration tool can read them without
// pulling in kvm-package UAPI structs.
//
// ExtendedState is opaque on purpose. Its decoder lives in the
// hypervisor adapter that produced the snapshot — see
// snapshot_arch.go (Linux) for the KVM encode/decode pair.
type PortableVCPUState struct {
	Index uint32 `json:"index"`

	// GPRs holds the architecturally visible general-purpose register
	// set. On amd64 these are RAX..R15, RIP, RFLAGS; on arm64 the
	// Registers shape is unused and CoreRegs ride inside
	// ExtendedState.
	GPRs Registers `json:"gprs"`

	// Sregs holds the segment / descriptor-table state on amd64. On
	// arm64 it is left zero-valued and the system-register table is
	// part of ExtendedState.
	Sregs SegmentRegisters `json:"sregs"`

	// ExtendedState is a hypervisor-defined blob. Migration tools
	// must check the envelope's Hypervisor field before decoding.
	ExtendedState json.RawMessage `json:"extended_state,omitempty"`
}

// DeviceSnapshotState wraps an opaque per-device payload. Kind is a
// short stable identifier ("virtio-blk", "virtio-net", "uart-16550",
// "pic-8259", ...). Payload is whatever the device emulator chooses
// to round-trip; envelope readers MUST NOT assume any shape beyond
// "valid JSON".
type DeviceSnapshotState struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// MemRegionSnapshot describes one guest physical address region. The
// inline form (DataFile == "") is reserved for tiny regions
// (sub-page); the on-disk form points to a chunk file in the
// snapshot dir, matching the existing migration.go convention where
// `mem.bin` is the canonical full-RAM dump.
type MemRegionSnapshot struct {
	GPA      uint64 `json:"gpa"`
	Size     uint64 `json:"size"`
	DataFile string `json:"data_file,omitempty"`
	// Flags carries portable MemFlags (MemRead/MemWrite/MemExecute);
	// stored as uint32 so a non-Go reader doesn't have to know our
	// internal type alias.
	Flags uint32 `json:"flags,omitempty"`
}

// snapshotVersionHead is the minimal JSON header the probe decodes.
// Keeping it small avoids parsing the full envelope twice and works
// on both v1 (legacy, has Version=3 + no Hypervisor) and v2 (Version=2
// + Hypervisor != "").
type snapshotVersionHead struct {
	Version    uint8  `json:"version"`
	Hypervisor string `json:"hypervisor"`
}

// SnapshotFormatLegacy and SnapshotFormatV2 are the two values
// probeSnapshotVersion can return. Callers treat anything other than
// SnapshotFormatV2 as legacy and go down the existing kvm-shaped read
// path; this keeps the v1 restore code bit-for-bit untouched.
const (
	SnapshotFormatLegacy uint8 = 1
	SnapshotFormatV2     uint8 = 2
)

// probeSnapshotVersion peeks at the JSON header of a snapshot stream
// and returns SnapshotFormatV2 when the document is a portable
// envelope (Version==2 AND a non-empty Hypervisor field, used as a
// disambiguator because legacy snapshots already use Version==2 in
// some older fixtures), or SnapshotFormatLegacy otherwise.
//
// The returned reader is the original payload (decoder consumed bytes
// internally; caller is expected to seek/reopen if they need to read
// the full document again). For convenience the helper does not
// require io.Seeker — most callers pass bytes already in memory.
func probeSnapshotVersion(r io.Reader) (uint8, error) {
	if r == nil {
		return 0, errors.New("probeSnapshotVersion: nil reader")
	}
	var head snapshotVersionHead
	dec := json.NewDecoder(r)
	if err := dec.Decode(&head); err != nil {
		return 0, fmt.Errorf("probeSnapshotVersion: decode header: %w", err)
	}
	if head.Version == SnapshotV2Version && head.Hypervisor != "" {
		return SnapshotFormatV2, nil
	}
	return SnapshotFormatLegacy, nil
}

// ProbeSnapshotBytes is the byte-slice convenience form of
// probeSnapshotVersion. Returns the format sentinel without
// consuming the buffer.
func ProbeSnapshotBytes(data []byte) (uint8, error) {
	return probeSnapshotVersion(bytes.NewReader(data))
}

// MarshalSnapshotV2 serialises an envelope to indented JSON. The
// envelope's Version is forced to SnapshotV2Version so callers
// constructing one by hand can't emit a wrong-version document by
// accident.
func MarshalSnapshotV2(snap *SnapshotV2) ([]byte, error) {
	if snap == nil {
		return nil, errors.New("MarshalSnapshotV2: nil snapshot")
	}
	if snap.Hypervisor == "" {
		return nil, errors.New("MarshalSnapshotV2: empty Hypervisor field — required to disambiguate from legacy snapshots")
	}
	out := *snap
	out.Version = SnapshotV2Version
	return json.MarshalIndent(&out, "", "  ")
}

// UnmarshalSnapshotV2 parses an envelope and validates the version
// and hypervisor-field invariants. Callers that want to be tolerant
// of legacy payloads should probe first.
func UnmarshalSnapshotV2(data []byte) (*SnapshotV2, error) {
	if len(data) == 0 {
		return nil, errors.New("UnmarshalSnapshotV2: empty input")
	}
	var snap SnapshotV2
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("UnmarshalSnapshotV2: %w", err)
	}
	if snap.Version != SnapshotV2Version {
		return nil, fmt.Errorf("UnmarshalSnapshotV2: version %d, want %d", snap.Version, SnapshotV2Version)
	}
	if snap.Hypervisor == "" {
		return nil, errors.New("UnmarshalSnapshotV2: empty Hypervisor field")
	}
	return &snap, nil
}

// SnapshotV2Hypervisor enumerates the recognised values of
// SnapshotV2.Hypervisor. Adding a new backend means adding a new
// constant here; readers should accept unknown values rather than
// reject (forward-compat) but emit a warning.
const (
	SnapshotHypervisorKVM = "kvm"
	SnapshotHypervisorWHP = "whp"
)

// SnapshotV2Arch enumerates the recognised values of SnapshotV2.Arch.
const (
	SnapshotArchAMD64 = "amd64"
	SnapshotArchARM64 = "arm64"
)
