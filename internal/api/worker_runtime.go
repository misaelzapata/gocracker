package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/firecrackerapi"
	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/worker"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

const workerRegistryVersion = 1

type persistedWorkerRecord struct {
	Version    int                `json:"version"`
	VMID       string             `json:"vm_id"`
	Kind       string             `json:"kind"`
	MetadataKV map[string]string  `json:"metadata_kv,omitempty"`
	Config     vmm.Config         `json:"config"`
	Metadata   vmm.WorkerMetadata `json:"metadata"`
	BundleDir  string             `json:"bundle_dir,omitempty"`
	IsRoot     bool               `json:"is_root,omitempty"`
}

func (s *Server) workerLaunchEnabled() bool {
	return strings.TrimSpace(strings.ToLower(s.jailerMode)) != container.JailerModeOff
}

func (s *Server) workerVMMOptions() worker.VMMOptions {
	return worker.VMMOptions{
		JailerBinary: s.jailerBinary,
		VMMBinary:    s.vmmBinary,
		UID:          s.uid,
		GID:          s.gid,
		ChrootBase:   s.chrootBaseDir,
	}
}

func (s *Server) launchManagedVMM(cfg vmm.Config) (vmm.Handle, func(), error) {
	if s.launchVMMFn != nil {
		return s.launchVMMFn(cfg)
	}
	if s.workerLaunchEnabled() {
		return worker.LaunchVMM(cfg, s.workerVMMOptions())
	}
	vm, err := vmm.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := vm.Start(); err != nil {
		return nil, nil, err
	}
	return vm, nil, nil
}

func (s *Server) restoreManagedVMM(snapshotDir string, opts vmm.RestoreOptions, applyMigrationPatches bool, resume bool) (vmm.Handle, func(), error) {
	if applyMigrationPatches {
		if err := vmm.ApplyMigrationPatches(snapshotDir); err != nil {
			return nil, nil, err
		}
	}
	if s.restoreVMMFn != nil {
		return s.restoreVMMFn(snapshotDir, opts)
	}
	if s.workerLaunchEnabled() {
		return worker.LaunchRestoredVMMWithResume(snapshotDir, opts, resume, s.workerVMMOptions())
	}
	vm, err := vmm.RestoreFromSnapshotWithOptions(snapshotDir, opts)
	if err != nil {
		return nil, nil, err
	}
	if resume {
		if err := vm.Start(); err != nil {
			vm.Stop()
			return nil, nil, err
		}
	}
	return vm, nil, nil
}

func (s *Server) reattachManagedVMM(cfg vmm.Config, meta vmm.WorkerMetadata) (vmm.Handle, func(), error) {
	if s.reattachVMMFn != nil {
		return s.reattachVMMFn(cfg, meta)
	}
	return worker.ReattachVMM(worker.ReattachOptions{
		Config:   cfg,
		Metadata: meta,
	})
}

func (s *Server) buildPrebootVMConfig() (vmm.Config, error) {
	s.mu.RLock()
	p := s.preboot
	rootID := s.rootVMID
	s.mu.RUnlock()

	if rootID != "" {
		return vmm.Config{}, firecrackerapi.InvalidStatef("instance already started")
	}
	spec := firecrackerapi.PrebootConfig{
		DefaultVCPUs: 1,
		DefaultMemMB: 128,
	}
	if p.bootSource != nil {
		spec.BootSource = &firecrackerapi.BootSource{
			KernelImagePath: p.bootSource.KernelImagePath,
			BootArgs:        p.bootSource.BootArgs,
			InitrdPath:      p.bootSource.InitrdPath,
			X86Boot:         p.bootSource.X86Boot,
		}
	}
	if p.machineConf != nil {
		spec.MachineCfg = &firecrackerapi.MachineConfig{
			VcpuCount:  p.machineConf.VcpuCount,
			MemSizeMib: p.machineConf.MemSizeMib,
		}
	}
	if p.balloon != nil {
		spec.Balloon = &firecrackerapi.Balloon{
			AmountMib:             p.balloon.AmountMib,
			DeflateOnOOM:          p.balloon.DeflateOnOOM,
			StatsPollingIntervalS: p.balloon.StatsPollingIntervalS,
			FreePageHinting:       p.balloon.FreePageHinting,
			FreePageReporting:     p.balloon.FreePageReporting,
		}
	}
	if p.memoryHotplug != nil {
		spec.MemoryHotplug = &firecrackerapi.MemoryHotplugConfig{
			TotalSizeMib: p.memoryHotplug.TotalSizeMiB,
			SlotSizeMib:  p.memoryHotplug.SlotSizeMiB,
			BlockSizeMib: p.memoryHotplug.BlockSizeMiB,
		}
	}
	spec.Drives = make([]firecrackerapi.Drive, 0, len(p.drives))
	for _, drive := range p.drives {
		spec.Drives = append(spec.Drives, firecrackerapi.Drive{
			DriveID:      drive.DriveID,
			PathOnHost:   drive.PathOnHost,
			IsRootDevice: drive.IsRootDevice,
		})
	}
	spec.NetIfaces = make([]firecrackerapi.NetworkInterface, 0, len(p.netIfaces))
	for _, iface := range p.netIfaces {
		spec.NetIfaces = append(spec.NetIfaces, firecrackerapi.NetworkInterface{
			IfaceID:     iface.IfaceID,
			HostDevName: iface.HostDevName,
			GuestMAC:    iface.GuestMAC,
		})
	}
	if err := firecrackerapi.ValidatePrebootForStart(spec); err != nil {
		return vmm.Config{}, err
	}
	mode, err := normalizeX86BootMode(s.defaultX86Boot, p.bootSource.X86Boot)
	if err != nil {
		return vmm.Config{}, err
	}
	cfg := vmm.Config{
		ID:         "root-vm",
		KernelPath: p.bootSource.KernelImagePath,
		Cmdline:    p.bootSource.BootArgs,
		InitrdPath: p.bootSource.InitrdPath,
		VCPUs:      1,
		MemMB:      128,
		X86Boot:    mode,
	}
	if p.machineConf != nil {
		if p.machineConf.MemSizeMib > 0 {
			cfg.MemMB = uint64(p.machineConf.MemSizeMib)
		}
		if p.machineConf.VcpuCount > 0 {
			cfg.VCPUs = p.machineConf.VcpuCount
		}
		cfg.RNGRateLimiter = cloneAPILimiter(p.machineConf.RNGRateLimiter)
	}
	if p.balloon != nil {
		cfg.Balloon = &vmm.BalloonConfig{
			AmountMiB:             p.balloon.AmountMib,
			DeflateOnOOM:          p.balloon.DeflateOnOOM,
			StatsPollingIntervalS: p.balloon.StatsPollingIntervalS,
		}
	}
	if p.memoryHotplug != nil {
		cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{
			TotalSizeMiB: p.memoryHotplug.TotalSizeMiB,
			SlotSizeMiB:  p.memoryHotplug.SlotSizeMiB,
			BlockSizeMiB: p.memoryHotplug.BlockSizeMiB,
		}
	}
	if len(p.drives) > 1 {
		cfg.Drives = make([]vmm.DriveConfig, 0, len(p.drives))
	}
	for _, d := range p.drives {
		driveCfg := vmm.DriveConfig{
			ID:          d.DriveID,
			Path:        d.PathOnHost,
			Root:        d.IsRootDevice,
			ReadOnly:    d.IsReadOnly,
			RateLimiter: cloneAPILimiter(d.RateLimiter),
		}
		if d.IsRootDevice {
			cfg.DiskImage = d.PathOnHost
			cfg.DiskRO = d.IsReadOnly
			cfg.BlockRateLimiter = cloneAPILimiter(d.RateLimiter)
		}
		if cfg.Drives != nil {
			cfg.Drives = append(cfg.Drives, driveCfg)
		}
	}
	if len(p.netIfaces) > 0 {
		cfg.TapName = p.netIfaces[0].HostDevName
		cfg.NetRateLimiter = cloneAPILimiter(p.netIfaces[0].RateLimiter)
		if mac := strings.TrimSpace(p.netIfaces[0].GuestMAC); mac != "" {
			hw, err := net.ParseMAC(mac)
			if err != nil {
				return vmm.Config{}, fmt.Errorf("parse guest_mac: %w", err)
			}
			cfg.MACAddr = hw
		}
	}
	return cfg, nil
}

func (s *Server) stopRootVM() error {
	s.mu.RLock()
	rootID := s.rootVMID
	entry := s.vms[rootID]
	s.mu.RUnlock()
	if rootID == "" || entry == nil {
		return firecrackerapi.InvalidStatef("instance not started")
	}
	entry.handle.Stop()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = entry.handle.WaitStopped(ctx)
		s.cleanStopped()
	}()
	return nil
}

func (s *Server) rejectIfRootStarted() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.rootVMID != "" {
		return firecrackerapi.InvalidStatef("instance already started")
	}
	return nil
}

func (s *Server) registerVMEntry(id string, entry *vmEntry) {
	if entry == nil {
		return
	}
	entry.apiID = id
	s.mu.Lock()
	s.vms[id] = entry
	if entry.bundleDir != "" {
		s.vmDirs[id] = entry.bundleDir
	}
	if entry.isRoot {
		s.rootVMID = id
	}
	s.mu.Unlock()
	if err := s.persistWorkerEntry(id, entry); err != nil {
		gclog.API.Warn("persist worker entry failed", "vm_id", id, "error", err)
	}
}

func (s *Server) persistWorkerEntry(id string, entry *vmEntry) error {
	if s.stateDir == "" || entry == nil {
		return nil
	}
	workerHandle, ok := entry.handle.(vmm.WorkerBacked)
	if !ok {
		s.removePersistedWorkerRecord(id)
		return nil
	}
	meta := workerHandle.WorkerMetadata()
	record := persistedWorkerRecord{
		Version:    workerRegistryVersion,
		VMID:       id,
		Kind:       entry.kind,
		MetadataKV: cloneMetadata(entry.metadata),
		Config:     entry.handle.VMConfig(),
		Metadata: vmm.WorkerMetadata{
			Kind:       entry.kind,
			SocketPath: meta.SocketPath,
			WorkerPID:  meta.WorkerPID,
			JailRoot:   meta.JailRoot,
			RunDir:     meta.RunDir,
			CreatedAt:  entry.createdAt,
		},
		BundleDir: entry.bundleDir,
		IsRoot:    entry.isRoot,
	}
	return writeJSONAtomically(s.workerRecordPath(id), record)
}

func (s *Server) removePersistedWorkerRecord(id string) {
	if s.stateDir == "" || id == "" {
		return
	}
	_ = os.Remove(s.workerRecordPath(id))
}

func (s *Server) loadPersistedWorkers() {
	if s.stateDir == "" || !s.workerLaunchEnabled() {
		return
	}
	dir := s.workerRegistryDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		gclog.API.Warn("create worker state dir failed", "dir", dir, "error", err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		gclog.API.Warn("read worker state dir failed", "dir", dir, "error", err)
		return
	}
	for _, file := range entries {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, file.Name())
		record, err := readWorkerRecord(path)
		if err != nil {
			gclog.API.Warn("read worker record failed", "path", path, "error", err)
			_ = os.Remove(path)
			continue
		}
		if !workerRecordLive(record) {
			s.cleanupPersistedWorkerRecord(path, record)
			continue
		}
		handle, cleanup, err := s.reattachManagedVMM(record.Config, record.Metadata)
		if err != nil {
			gclog.API.Warn("reattach worker failed", "vm_id", record.VMID, "error", err)
			s.cleanupPersistedWorkerRecord(path, record)
			continue
		}
		entry := s.newVMEntry(handle, cleanup)
		entry.apiID = record.VMID
		entry.kind = record.Kind
		entry.metadata = cloneMetadata(record.MetadataKV)
		entry.bundleDir = record.BundleDir
		entry.isRoot = record.IsRoot
		s.mu.Lock()
		s.vms[record.VMID] = entry
		if record.BundleDir != "" {
			s.vmDirs[record.VMID] = record.BundleDir
		}
		if record.IsRoot {
			s.rootVMID = record.VMID
		}
		s.mu.Unlock()
	}
}

func (s *Server) cleanupPersistedWorkerRecord(path string, record persistedWorkerRecord) {
	if record.Metadata.RunDir != "" {
		_ = os.RemoveAll(record.Metadata.RunDir)
	}
	if record.BundleDir != "" {
		_ = os.RemoveAll(record.BundleDir)
	}
	if record.Metadata.JailRoot != "" {
		target := record.Metadata.JailRoot
		if filepath.Base(target) == "root" {
			target = filepath.Dir(target)
		}
		_ = os.RemoveAll(target)
	}
	_ = os.Remove(path)
}

func (s *Server) workerRegistryDir() string {
	return filepath.Join(s.stateDir, "vms")
}

func (s *Server) workerRecordPath(id string) string {
	return filepath.Join(s.workerRegistryDir(), id+".json")
}

func readWorkerRecord(path string) (persistedWorkerRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return persistedWorkerRecord{}, err
	}
	var record persistedWorkerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return persistedWorkerRecord{}, err
	}
	if record.Version != workerRegistryVersion {
		return persistedWorkerRecord{}, fmt.Errorf("unsupported worker record version %d", record.Version)
	}
	return record, nil
}

func workerRecordLive(record persistedWorkerRecord) bool {
	if record.Metadata.SocketPath == "" || record.Metadata.WorkerPID <= 0 {
		return false
	}
	if err := syscall.Kill(record.Metadata.WorkerPID, 0); err != nil && err != syscall.EPERM {
		return false
	}
	conn, err := net.DialTimeout("unix", record.Metadata.SocketPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func writeJSONAtomically(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
