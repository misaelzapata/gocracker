//go:build linux

package vmm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gocracker/gocracker/internal/uart"
	"github.com/gocracker/gocracker/internal/virtio"
)

const (
	migrationMemFile    = "mem.bin"
	migrationPatchMeta  = "patches.json"
	migrationPatchData  = "patches.bin"
	migrationKernelPath = "artifacts/kernel"
	migrationInitrdPath = "artifacts/initrd"
	migrationDiskPath   = "artifacts/disk.ext4"
)

type DirtyPatchEntry struct {
	Offset     uint64 `json:"offset"`
	Length     uint64 `json:"length"`
	DataOffset uint64 `json:"data_offset"`
}

type DirtyFilePatch struct {
	Path     string            `json:"path"`
	PageSize uint64            `json:"page_size"`
	Entries  []DirtyPatchEntry `json:"entries,omitempty"`
}

type MigrationPatchSet struct {
	Version int              `json:"version"`
	Patches []DirtyFilePatch `json:"patches,omitempty"`
}

// CreateMigrationBundle snapshots a running VM into dir and rewrites referenced
// host-side assets so the bundle can be moved to another process or host.
// The VM is left paused so the caller can decide whether to resume or stop it.
func CreateMigrationBundle(vm *VM, dir string) (*Snapshot, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if _, err := vm.TakeSnapshotWithOptions(dir, SnapshotOptions{Resume: false}); err != nil {
		return nil, err
	}
	return rewriteSnapshotBundle(dir)
}

// RestoreMigrationBundle restores a VM from a migration bundle directory.
// Accepts either a legacy v1 snapshot.json or a portable v2 envelope; the
// format is detected by probeSnapshotVersion on the snapshot.json header.
func RestoreMigrationBundle(dir string, opts RestoreOptions) (*VM, error) {
	if err := ApplyMigrationPatches(dir); err != nil {
		return nil, err
	}
	snap, err := readSnapshotAny(dir)
	if err != nil {
		return nil, err
	}
	return restoreFromSnapshot(dir, snap, opts)
}

// readSnapshotAny reads <dir>/snapshot.json and dispatches on the
// envelope format. v1 (legacy KVM-shaped) is returned via the existing
// readSnapshot path so the bit-for-bit deserialisation is preserved.
// v2 (portable envelope) is decoded and translated into the v1 shape
// the rest of the restore pipeline expects; cross-hypervisor restore
// is out of scope, so a v2 envelope with Hypervisor != "kvm" is
// rejected here rather than silently producing wrong state.
func readSnapshotAny(dir string) (Snapshot, error) {
	metaFile := filepath.Join(dir, "snapshot.json")
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot: %w", err)
	}
	format, err := ProbeSnapshotBytes(data)
	if err != nil {
		return Snapshot{}, fmt.Errorf("probe snapshot version: %w", err)
	}
	switch format {
	case SnapshotFormatV2:
		return decodeSnapshotV2(data)
	default:
		// Legacy path: unmarshal directly into the existing Snapshot
		// shape. Identical to the original readSnapshot body so v1
		// round-trips are bit-identical.
		var snap Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			return Snapshot{}, err
		}
		return snap, nil
	}
}

// decodeSnapshotV2 parses a portable envelope and reconstitutes a
// Snapshot the existing Linux restore path can consume. The KVM
// adapter owns the ExtendedState decode; non-KVM envelopes are
// rejected because the same-hypervisor invariant doesn't hold.
func decodeSnapshotV2(data []byte) (Snapshot, error) {
	v2, err := UnmarshalSnapshotV2(data)
	if err != nil {
		return Snapshot{}, err
	}
	if v2.Hypervisor != SnapshotHypervisorKVM {
		return Snapshot{}, fmt.Errorf("snapshot v2: hypervisor %q not supported on this build (kvm-only)", v2.Hypervisor)
	}
	vcpus := make([]VCPUState, 0, len(v2.VCPUs))
	for _, p := range v2.VCPUs {
		vs, err := portableToVCPUState(p, v2.Arch)
		if err != nil {
			return Snapshot{}, fmt.Errorf("snapshot v2 vcpu %d: %w", p.Index, err)
		}
		vcpus = append(vcpus, vs)
	}
	snap := Snapshot{
		Version: 3,
		ID:      v2.ID,
		VCPUs:   vcpus,
		MemFile: migrationMemFile,
	}
	if len(vcpus) > 0 && vcpus[0].X86 != nil {
		legacy := vcpus[0].normalizedX86()
		snap.Regs = legacy.Regs
		snap.Sregs = legacy.Sregs
		snap.MPState = legacy.MPState
	}
	// SnapshotArchState may be carried in v2.Meta["arch_state"] as a
	// JSON blob; this keeps the in-kernel-device state available for
	// same-hypervisor restores without inflating the envelope schema.
	if raw, ok := v2.Meta["arch_state"]; ok && raw != "" {
		var arch SnapshotArchState
		if err := json.Unmarshal([]byte(raw), &arch); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot v2 arch_state: %w", err)
		}
		snap.Arch = &arch
	}
	return snap, nil
}

// PrepareMigrationBundle creates the pre-copy base bundle while the VM is still
// running. It copies static artifacts plus a full RAM image and enables dirty
// tracking so FinalizeMigrationBundle can later ship only the delta.
func PrepareMigrationBundle(vm *VM, dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := vm.prepareSnapshot(); err != nil {
		return err
	}
	if err := vm.kvm().EnableDirtyLogging(); err != nil {
		return fmt.Errorf("enable dirty logging: %w", err)
	}
	if err := vm.kvm().ResetDirtyLog(0); err != nil {
		return fmt.Errorf("reset dirty log: %w", err)
	}
	if vm.memDirty != nil {
		vm.memDirty.Reset()
	}
	if vm.blkDev != nil {
		vm.blkDev.ResetDirty()
	}

	if err := writeMemoryFile(filepath.Join(dir, migrationMemFile), vm.kvm().Memory()); err != nil {
		return fmt.Errorf("write base memory: %w", err)
	}
	if _, err := bundleAsset(dir, vm.cfg.KernelPath, migrationKernelPath); err != nil {
		return err
	}
	if _, err := bundleAsset(dir, vm.cfg.InitrdPath, migrationInitrdPath); err != nil {
		return err
	}
	if _, err := bundleAsset(dir, vm.cfg.DiskImage, migrationDiskPath); err != nil {
		return err
	}
	return nil
}

// FinalizeMigrationBundle pauses the VM, captures device/vCPU state, and writes
// only the dirty page/file deltas needed to reconstruct the final state on the
// destination side.
func FinalizeMigrationBundle(vm *VM, dir string) (*Snapshot, *MigrationPatchSet, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, err
	}

	wasRunning := false
	switch vm.State() {
	case StateRunning:
		// Same contract as TakeSnapshotWithOptions: tell the guest driver
		// to reset every open AF_VSOCK connection BEFORE pausing, so the
		// restored process on the destination has no pre-migration
		// orphan conns to deal with. Without this the post-migrate exec
		// sits waiting on a pipe whose host half disappeared when the
		// source process exited.
		if vm.vsockDev != nil {
			vm.vsockDev.QuiesceForSnapshot()
		}
		if err := vm.Pause(); err != nil {
			return nil, nil, err
		}
		wasRunning = true
	case StatePaused:
	default:
		return nil, nil, fmt.Errorf("VM must be running or paused to finalize migration (state: %s)", vm.State())
	}
	_ = wasRunning

	if err := vm.prepareSnapshot(); err != nil {
		return nil, nil, err
	}

	snap, err := captureSnapshotState(vm)
	if err != nil {
		return nil, nil, err
	}
	rewriteSnapshotPathsForBundle(snap)

	patches, err := writeMigrationPatches(vm, dir)
	if err != nil {
		return nil, nil, err
	}

	data, err := marshalSnapshotForSource(snap, vm)
	if err != nil {
		return nil, nil, err
	}
	if err := WriteSnapshotJSON(dir, data); err != nil {
		return nil, nil, err
	}

	return snap, patches, nil
}

// marshalSnapshotForSource picks between v1 (legacy KVM-shaped) and v2
// (portable envelope) on-disk emission based on the source hypervisor.
//
// Linux KVM source keeps emitting v1 so KVM↔KVM bundles produced by older
// gocracker builds stay byte-identical and the established round-trip
// fixtures don't need re-baking. WHP source emits v2 because the WHP
// adapter (Windows-only) has no KVM uapi structs to populate the legacy
// fields with — its capture path produces a SnapshotV2 envelope directly.
//
// On the Linux build this file lives under, isWHPSource is always false
// (WHP only exists on Windows), so this is effectively a v1 emission with
// the dispatch wired through for the cross-platform follow-up that owns
// the Windows-side capture path.
func marshalSnapshotForSource(snap *Snapshot, vm *VM) ([]byte, error) {
	if isWHPSource(vm) {
		return writeSnapshotV2Bytes(snap, SnapshotHypervisorWHP)
	}
	return json.MarshalIndent(snap, "", "  ")
}

// isWHPSource reports whether the VM's hypervisor backend is WHP. On
// Linux the answer is always false — the WHP adapter is gated behind
// //go:build windows in hypervisor_windows.go, so the type assertion in
// the Windows build catches it. Centralising the check here means the
// Windows-side migration follow-up only has to override one function.
func isWHPSource(vm *VM) bool {
	if vm == nil || vm.hv == nil {
		return false
	}
	return hypervisorIsWHP(vm.hv)
}

// hypervisorIsWHP is the Linux build's view of the WHP detector: WHP is
// Windows-only, so the answer is always false here. The Windows build
// of pkg/vmm provides a concrete implementation in hypervisor_windows.go
// that type-asserts *whpHypervisor.
func hypervisorIsWHP(_ Hypervisor) bool { return false }

// writeSnapshotV2Bytes builds a portable SnapshotV2 envelope from a v1
// Snapshot in memory. The result is JSON ready to hand to WriteSnapshotJSON.
//
// The envelope carries vcpu state through vcpuStateToPortable (which
// owns the KVM-specific ExtendedState encoding on Linux). Arch state
// rides in Meta["arch_state"] as a JSON blob so the legacy
// SnapshotArchState type doesn't need a v2-specific equivalent — same
// strategy decodeSnapshotV2 uses on the read side.
//
// hypervisor MUST be one of SnapshotHypervisorKVM / SnapshotHypervisorWHP;
// MarshalSnapshotV2 rejects an empty value.
func writeSnapshotV2Bytes(snap *Snapshot, hypervisor string) ([]byte, error) {
	if snap == nil {
		return nil, fmt.Errorf("writeSnapshotV2Bytes: nil snapshot")
	}
	env := &SnapshotV2{
		Hypervisor: hypervisor,
		Arch:       SnapshotArchAMD64,
		ID:         snap.ID,
	}
	if snap.Config.Arch == "arm64" {
		env.Arch = SnapshotArchARM64
	}
	for _, vs := range snap.VCPUs {
		p, err := vcpuStateToPortable(vs)
		if err != nil {
			return nil, fmt.Errorf("writeSnapshotV2Bytes: vcpu %d: %w", vs.ID, err)
		}
		env.VCPUs = append(env.VCPUs, p)
	}
	if snap.Arch != nil {
		archBlob, err := json.Marshal(snap.Arch)
		if err != nil {
			return nil, fmt.Errorf("writeSnapshotV2Bytes: arch_state marshal: %w", err)
		}
		if env.Meta == nil {
			env.Meta = make(map[string]string, 1)
		}
		env.Meta["arch_state"] = string(archBlob)
	}
	if snap.MemFile != "" {
		env.Memory = []MemRegionSnapshot{{
			GPA:      0,
			Size:     0,
			DataFile: snap.MemFile,
		}}
	}
	return MarshalSnapshotV2(env)
}

// ResetMigrationTracking disables dirty tracking after a failed migration so
// the source VM can keep running without carrying migration state.
func ResetMigrationTracking(vm *VM) error {
	if vm.memDirty != nil {
		vm.memDirty.Reset()
	}
	if vm.blkDev != nil {
		vm.blkDev.ResetDirty()
	}
	if vm.kvm().DirtyLoggingEnabled() {
		if err := vm.kvm().DisableDirtyLogging(); err != nil {
			return err
		}
	}
	return nil
}

// ApplyMigrationPatches merges pre-copy deltas into dir before restore.
func ApplyMigrationPatches(dir string) error {
	metaPath := filepath.Join(dir, migrationPatchMeta)
	if _, err := os.Stat(metaPath); errorsIsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	data, err := os.ReadFile(metaPath)
	if err != nil {
		return err
	}
	var patchSet MigrationPatchSet
	if err := json.Unmarshal(data, &patchSet); err != nil {
		return err
	}
	if len(patchSet.Patches) == 0 {
		return nil
	}

	patchData, err := os.Open(filepath.Join(dir, migrationPatchData))
	if err != nil {
		return err
	}
	defer patchData.Close()

	for _, filePatch := range patchSet.Patches {
		target := filepath.Join(dir, filepath.FromSlash(filePatch.Path))
		f, err := os.OpenFile(target, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		for _, entry := range filePatch.Entries {
			buf := make([]byte, entry.Length)
			if _, err := patchData.ReadAt(buf, int64(entry.DataOffset)); err != nil {
				_ = f.Close()
				return err
			}
			if _, err := f.WriteAt(buf, int64(entry.Offset)); err != nil {
				_ = f.Close()
				return err
			}
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func rewriteSnapshotBundle(dir string) (*Snapshot, error) {
	snap, err := readSnapshot(dir)
	if err != nil {
		return nil, err
	}
	return rewriteSnapshotBundleWithConfig(dir, snap, snap.Config)
}

// RewriteSnapshotBundleWithConfig rewrites a raw snapshot directory into a
// migration-safe bundle using the provided host-visible asset paths.
func RewriteSnapshotBundleWithConfig(dir string, cfg Config) (*Snapshot, error) {
	snap, err := readSnapshot(dir)
	if err != nil {
		return nil, err
	}
	return rewriteSnapshotBundleWithConfig(dir, snap, cfg)
}

func rewriteSnapshotBundleWithConfig(dir string, snap Snapshot, cfg Config) (*Snapshot, error) {
	return rewriteSnapshotBundleOpts(dir, snap, cfg, false)
}

func rewriteSnapshotBundleOpts(dir string, snap Snapshot, cfg Config, skipDisk bool) (*Snapshot, error) {
	if snap.MemFile == "" {
		snap.MemFile = "mem.bin"
	}
	snap.Config = cfg
	var err error
	if snap.Config.KernelPath, err = bundleAsset(dir, snap.Config.KernelPath, "artifacts/kernel"); err != nil {
		return nil, err
	}
	if snap.Config.InitrdPath, err = bundleAsset(dir, snap.Config.InitrdPath, "artifacts/initrd"); err != nil {
		return nil, err
	}
	if !skipDisk {
		if snap.Config.DiskImage, err = bundleAsset(dir, snap.Config.DiskImage, "artifacts/disk.ext4"); err != nil {
			return nil, err
		}
	}
	// When skipDisk is true, snap.Config.DiskImage retains its original path
	// (e.g. /worker/drives/0). The host-side takeSnapshotViaExport hardlinks
	// the real disk into artifacts/disk.ext4 after exporting and rewrites
	// snap.Config.DiskImage = "artifacts/disk.ext4".

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := WriteSnapshotJSON(dir, data); err != nil {
		return nil, err
	}
	return &snap, nil
}

// WriteSnapshotJSON atomically writes data to <dir>/snapshot.json using
// tmp+rename with fsync on both the temp file and the parent directory. The
// previous implementation opened snapshot.json with O_TRUNC, so a crash
// mid-write left a truncated file that a later restore would happily feed to
// the kernel. tmp+rename ensures readers always see either the old contents
// or the full new payload, never a torn write.
func WriteSnapshotJSON(dir string, data []byte) error {
	return writeFileAtomic(filepath.Join(dir, "snapshot.json"), data, 0644)
}

// writeFileAtomic writes data to path atomically: it creates a sibling temp
// file, fsyncs it, renames it over path, then fsyncs the parent directory so
// the rename itself is durable. On any error before the rename, the temp file
// is removed.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	if dirF, err := os.Open(dir); err == nil {
		_ = dirF.Sync()
		_ = dirF.Close()
	}
	return nil
}

func captureSnapshotState(m *VM) (*Snapshot, error) {
	vcpuStates := make([]VCPUState, 0, len(m.hvVCPUs))
	for _, hvcpu := range m.hvVCPUs {
		state, err := m.archBackend.captureVCPU(hvcpu)
		if err != nil {
			return nil, err
		}
		vcpuStates = append(vcpuStates, state)
	}

	archState, err := m.archBackend.captureVMState(m)
	if err != nil {
		return nil, fmt.Errorf("capture vm arch state: %w", err)
	}

	var uartState *uart.UARTState
	if m.uart0 != nil {
		s := m.uart0.State()
		uartState = &s
	}
	var tStates []virtio.TransportState
	for _, t := range m.transports {
		tStates = append(tStates, t.State())
	}
	cfg := m.cfg
	if m.balloonDev != nil && cfg.Balloon != nil {
		balloonCfg := *cfg.Balloon
		balloonCfg.AmountMiB = m.balloonDev.GetConfig().AmountMiB
		balloonCfg.StatsPollingIntervalS = int(m.balloonDev.StatsPollingInterval() / time.Second)
		balloonCfg.SnapshotPages = m.balloonDev.SnapshotPages()
		cfg.Balloon = &balloonCfg
	}

	snap := &Snapshot{
		Version:    3,
		Timestamp:  time.Now(),
		ID:         m.cfg.ID,
		Config:     cfg,
		VCPUs:      vcpuStates,
		MemFile:    migrationMemFile,
		Arch:       archState,
		UART:       uartState,
		Transports: tStates,
	}
	if len(vcpuStates) > 0 && vcpuStates[0].X86 != nil {
		legacy := vcpuStates[0].normalizedX86()
		snap.Regs = legacy.Regs
		snap.Sregs = legacy.Sregs
		snap.MPState = legacy.MPState
	}
	return snap, nil
}

func rewriteSnapshotPathsForBundle(snap *Snapshot) {
	snap.MemFile = migrationMemFile
	if snap.Config.KernelPath != "" {
		snap.Config.KernelPath = filepath.ToSlash(migrationKernelPath)
	}
	if snap.Config.InitrdPath != "" {
		snap.Config.InitrdPath = filepath.ToSlash(migrationInitrdPath)
	}
	if snap.Config.DiskImage != "" {
		snap.Config.DiskImage = filepath.ToSlash(migrationDiskPath)
	}
}

func writeMigrationPatches(vm *VM, dir string) (*MigrationPatchSet, error) {
	patchFile, err := os.OpenFile(filepath.Join(dir, migrationPatchData), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer patchFile.Close()

	var patches []DirtyFilePatch
	var dataOffset uint64

	memBitmap, err := vm.kvm().GetDirtyLog(0)
	if err != nil {
		return nil, err
	}
	memBitmap = mergeDirtyBitmaps(memBitmap, vm.memDirty.SnapshotAndReset())
	mem := vm.kvm().Memory()
	memPatch, err := buildDirtyFilePatch(patchFile, bytes.NewReader(mem), uint64(len(mem)), migrationMemFile, vm.memDirty.PageSize(), memBitmap, &dataOffset)
	if err != nil {
		return nil, err
	}
	if len(memPatch.Entries) > 0 {
		patches = append(patches, memPatch)
	}

	if vm.blkDev != nil && vm.cfg.DiskImage != "" {
		if err := vm.blkDev.PrepareSnapshot(); err != nil {
			return nil, err
		}
		diskBitmap := vm.blkDev.DirtyBitmapAndReset()
		if len(diskBitmap) > 0 {
			diskPatch, err := buildDirtyFilePatch(patchFile, vm.blkDev, vm.blkDev.SizeBytes(), migrationDiskPath, vm.blkDev.DirtyPageSize(), diskBitmap, &dataOffset)
			if err != nil {
				return nil, err
			}
			if len(diskPatch.Entries) > 0 {
				patches = append(patches, diskPatch)
			}
		}
	}

	patchSet := &MigrationPatchSet{Version: 1, Patches: patches}
	data, err := json.MarshalIndent(patchSet, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := patchFile.Sync(); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, migrationPatchMeta), data, 0644); err != nil {
		return nil, err
	}
	return patchSet, nil
}

func buildDirtyFilePatch(w io.Writer, src io.ReaderAt, srcSize uint64, relPath string, pageSize uint64, bitmap []uint64, nextDataOffset *uint64) (DirtyFilePatch, error) {
	patch := DirtyFilePatch{Path: filepath.ToSlash(relPath), PageSize: pageSize}
	if len(bitmap) == 0 || srcSize == 0 {
		return patch, nil
	}
	if pageSize == 0 {
		pageSize = 4096
		patch.PageSize = pageSize
	}
	var dataOffset uint64
	if nextDataOffset != nil {
		dataOffset = *nextDataOffset
	}
	var pending *DirtyPatchEntry
	appendPending := func() error {
		if pending != nil {
			if err := copyReaderAtRange(w, src, pending.Offset, pending.Length); err != nil {
				return err
			}
			patch.Entries = append(patch.Entries, *pending)
			pending = nil
		}
		return nil
	}
	for wordIdx, word := range bitmap {
		if word == 0 {
			continue
		}
		for bit := 0; bit < 64; bit++ {
			if word&(uint64(1)<<bit) == 0 {
				if err := appendPending(); err != nil {
					return DirtyFilePatch{}, err
				}
				continue
			}
			page := uint64(wordIdx*64 + bit)
			offset := page * pageSize
			if offset >= srcSize {
				if err := appendPending(); err != nil {
					return DirtyFilePatch{}, err
				}
				break
			}
			length := pageSize
			if offset+length > srcSize {
				length = srcSize - offset
			}
			if pending != nil && pending.Offset+pending.Length == offset {
				pending.Length += length
				dataOffset += length
				continue
			}
			if err := appendPending(); err != nil {
				return DirtyFilePatch{}, err
			}
			pending = &DirtyPatchEntry{
				Offset:     offset,
				Length:     length,
				DataOffset: dataOffset,
			}
			dataOffset += length
		}
		if err := appendPending(); err != nil {
			return DirtyFilePatch{}, err
		}
	}
	if nextDataOffset != nil {
		*nextDataOffset = dataOffset
	}
	return patch, nil
}

func copyReaderAtRange(dst io.Writer, src io.ReaderAt, offset, length uint64) error {
	const chunkSize = 1 << 20
	buf := make([]byte, chunkSize)
	for length > 0 {
		n := len(buf)
		if uint64(n) > length {
			n = int(length)
		}
		read, err := src.ReadAt(buf[:n], int64(offset))
		if err != nil && err != io.EOF {
			return err
		}
		if read == 0 {
			return io.ErrUnexpectedEOF
		}
		if _, err := dst.Write(buf[:read]); err != nil {
			return err
		}
		offset += uint64(read)
		length -= uint64(read)
	}
	return nil
}

func mergeDirtyBitmaps(a, b []uint64) []uint64 {
	size := len(a)
	if len(b) > size {
		size = len(b)
	}
	if size == 0 {
		return nil
	}
	out := make([]uint64, size)
	for i := 0; i < size; i++ {
		if i < len(a) {
			out[i] |= a[i]
		}
		if i < len(b) {
			out[i] |= b[i]
		}
	}
	return out
}

func writeMemoryFile(path string, mem []byte) error {
	return os.WriteFile(path, mem, 0600)
}

func bundleAsset(dir, srcPath, relDest string) (string, error) {
	if srcPath == "" {
		return "", nil
	}
	resolved := resolveSnapshotPath(dir, srcPath)
	if resolved == "" {
		return "", nil
	}

	dstPath := filepath.Join(dir, relDest)
	if sameFilePath(resolved, dstPath) {
		return filepath.ToSlash(relDest), nil
	}
	if err := copyFile(dstPath, resolved); err != nil {
		return "", fmt.Errorf("bundle asset %s: %w", resolved, err)
	}
	return filepath.ToSlash(relDest), nil
}

func sameFilePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func copyFile(dst, src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported asset type %s", info.Mode())
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	// Try hardlink first: instant, zero extra space, works when src/dst are
	// on the same filesystem (which is always true for /tmp-to-/tmp copies in
	// the warmcache path). Snapshot assets are read-only after bundling so
	// sharing an inode with the original is safe. Falls back on EXDEV (cross
	// filesystem) or other errors.
	if err := os.Link(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	// Try FICLONE: on btrfs/xfs/overlayfs creates a reflink (COW clone,
	// microseconds). Falls back to io.Copy on ext4/tmpfs.
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err == nil {
		return out.Sync()
	}

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}
