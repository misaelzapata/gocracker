package vmm

import (
	"fmt"
	"net"
	"time"

	"github.com/gocracker/gocracker/internal/guestexec"
)

const (
	hotplugRegionBaseAddr = uint64(0x1_0000_0000)
	hotplugRegionSlotBase = uint32(1)
	maxHotplugSlots       = 128
)

type memoryHotplugSlot struct {
	slot       uint32
	guestAddr  uint64
	sizeBytes  uint64
}

type memoryHotplugState struct {
	cfg            MemoryHotplugConfig
	baseAddr       uint64
	totalBytes     uint64
	slotBytes      uint64
	blockBytes     uint64
	requestedBytes uint64
	pluggedBytes   uint64
	guestBlockSize uint64
	slots          []memoryHotplugSlot
}

func (m *VM) setupMemoryHotplug() error {
	if m.cfg.MemoryHotplug == nil {
		return nil
	}
	cfg := *m.cfg.MemoryHotplug
	if err := validateMemoryHotplugConfig(cfg); err != nil {
		return err
	}
	totalBytes := hotplugMiBToBytes(cfg.TotalSizeMiB)
	slotBytes := hotplugMiBToBytes(cfg.SlotSizeMiB)
	blockBytes := hotplugMiBToBytes(cfg.BlockSizeMiB)
	slotCount := totalBytes / slotBytes
	state := &memoryHotplugState{
		cfg:        cfg,
		baseAddr:   hotplugRegionBaseAddr,
		totalBytes: totalBytes,
		slotBytes:  slotBytes,
		blockBytes: blockBytes,
	}
	for i := uint64(0); i < slotCount; i++ {
		slotID := hotplugRegionSlotBase + uint32(i)
		guestAddr := hotplugRegionBaseAddr + i*slotBytes
		if _, err := m.kvmVM.AddMemoryRegion(slotID, guestAddr, slotBytes, 0); err != nil {
			for _, mapped := range state.slots {
				_ = m.kvmVM.RemoveMemoryRegion(mapped.slot)
			}
			return err
		}
		state.slots = append(state.slots, memoryHotplugSlot{
			slot:      slotID,
			guestAddr: guestAddr,
			sizeBytes: slotBytes,
		})
	}
	m.memoryHotplug = state
	m.events.Emit(EventDevicesReady, fmt.Sprintf("memory hotplug region reserved total=%d MiB slots=%d", cfg.TotalSizeMiB, len(state.slots)))
	return nil
}

func validateMemoryHotplugConfig(cfg MemoryHotplugConfig) error {
	if cfg.TotalSizeMiB == 0 {
		return fmt.Errorf("memory hotplug total_size_mib is required")
	}
	if cfg.SlotSizeMiB == 0 {
		return fmt.Errorf("memory hotplug slot_size_mib is required")
	}
	if cfg.BlockSizeMiB == 0 {
		return fmt.Errorf("memory hotplug block_size_mib is required")
	}
	if cfg.TotalSizeMiB < cfg.SlotSizeMiB {
		return fmt.Errorf("memory hotplug total_size_mib must be >= slot_size_mib")
	}
	if cfg.SlotSizeMiB < cfg.BlockSizeMiB {
		return fmt.Errorf("memory hotplug slot_size_mib must be >= block_size_mib")
	}
	if cfg.TotalSizeMiB%cfg.SlotSizeMiB != 0 {
		return fmt.Errorf("memory hotplug total_size_mib must be a multiple of slot_size_mib")
	}
	if cfg.SlotSizeMiB%cfg.BlockSizeMiB != 0 {
		return fmt.Errorf("memory hotplug slot_size_mib must be a multiple of block_size_mib")
	}
	if slots := cfg.TotalSizeMiB / cfg.SlotSizeMiB; slots == 0 || slots > maxHotplugSlots {
		return fmt.Errorf("memory hotplug requires between 1 and %d slots; got %d", maxHotplugSlots, slots)
	}
	return nil
}

func hotplugMiBToBytes(v uint64) uint64 { return v << 20 }

func hotplugBytesToMiB(v uint64) uint64 { return v >> 20 }

func validateMemoryHotplugUpdate(state *memoryHotplugState, update MemoryHotplugSizeUpdate) error {
	targetBytes := hotplugMiBToBytes(update.RequestedSizeMiB)
	if targetBytes > state.totalBytes {
		return fmt.Errorf("memory hotplug requested_size_mib exceeds total_size_mib")
	}
	if targetBytes%state.blockBytes != 0 {
		return fmt.Errorf("memory hotplug requested_size_mib must be aligned to block_size_mib")
	}
	return nil
}

func (m *VM) GetMemoryHotplug() (MemoryHotplugStatus, error) {
	m.mu.Lock()
	state := m.memoryHotplug
	cfg := m.cfg
	vmState := m.state
	var out MemoryHotplugStatus
	if state != nil {
		out = MemoryHotplugStatus{
			TotalSizeMiB:     state.cfg.TotalSizeMiB,
			SlotSizeMiB:      state.cfg.SlotSizeMiB,
			BlockSizeMiB:     state.cfg.BlockSizeMiB,
			PluggedSizeMiB:   hotplugBytesToMiB(state.pluggedBytes),
			RequestedSizeMiB: hotplugBytesToMiB(state.requestedBytes),
		}
	}
	m.mu.Unlock()
	if state == nil {
		return MemoryHotplugStatus{}, fmt.Errorf("memory hotplug is not configured")
	}
	if cfg.Exec == nil || !cfg.Exec.Enabled || (vmState != StateRunning && vmState != StatePaused) {
		return out, nil
	}
	hotplug, err := m.queryGuestMemoryHotplug(cfg, state, state.requestedBytes)
	if err != nil {
		return out, nil
	}
	m.mu.Lock()
	if m.memoryHotplug != nil {
		m.memoryHotplug.pluggedBytes = hotplug.PluggedBytes
		m.memoryHotplug.guestBlockSize = hotplug.BlockSizeBytes
		out.PluggedSizeMiB = hotplugBytesToMiB(hotplug.PluggedBytes)
		out.RequestedSizeMiB = hotplugBytesToMiB(m.memoryHotplug.requestedBytes)
	}
	m.mu.Unlock()
	return out, nil
}

func (m *VM) UpdateMemoryHotplug(update MemoryHotplugSizeUpdate) error {
	m.mu.Lock()
	state := m.memoryHotplug
	cfg := m.cfg
	if state == nil {
		m.mu.Unlock()
		return fmt.Errorf("memory hotplug is not configured")
	}
	if err := validateMemoryHotplugUpdate(state, update); err != nil {
		m.mu.Unlock()
		return err
	}
	if cfg.Exec == nil || !cfg.Exec.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("memory hotplug requires the guest exec agent to be enabled")
	}
	m.mu.Unlock()

	targetBytes := hotplugMiBToBytes(update.RequestedSizeMiB)
	hotplug, err := m.updateGuestMemoryHotplug(cfg, state, targetBytes)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if m.memoryHotplug != nil {
		m.memoryHotplug.requestedBytes = targetBytes
		m.memoryHotplug.pluggedBytes = hotplug.PluggedBytes
		m.memoryHotplug.guestBlockSize = hotplug.BlockSizeBytes
	}
	m.mu.Unlock()
	return nil
}

func (m *VM) queryGuestMemoryHotplug(cfg Config, state *memoryHotplugState, requestedBytes uint64) (guestexec.MemoryHotplug, error) {
	return m.callGuestMemoryHotplug(cfg, guestexec.Request{
		Mode:                     guestexec.ModeMemoryHotplugGet,
		MemoryHotplugBaseAddr:    state.baseAddr,
		MemoryHotplugTotalBytes:  state.totalBytes,
		MemoryHotplugBlockBytes:  state.blockBytes,
		MemoryHotplugTargetBytes: requestedBytes,
	})
}

func (m *VM) updateGuestMemoryHotplug(cfg Config, state *memoryHotplugState, targetBytes uint64) (guestexec.MemoryHotplug, error) {
	return m.callGuestMemoryHotplug(cfg, guestexec.Request{
		Mode:                     guestexec.ModeMemoryHotplugUpdate,
		MemoryHotplugBaseAddr:    state.baseAddr,
		MemoryHotplugTotalBytes:  state.totalBytes,
		MemoryHotplugBlockBytes:  state.blockBytes,
		MemoryHotplugTargetBytes: targetBytes,
	})
}

func (m *VM) callGuestMemoryHotplug(cfg Config, req guestexec.Request) (guestexec.MemoryHotplug, error) {
	conn, err := m.DialVsock(execAgentPort(cfg))
	if err != nil {
		return guestexec.MemoryHotplug{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	if err := guestexec.Encode(conn, req); err != nil {
		return guestexec.MemoryHotplug{}, err
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		if err == net.ErrClosed {
			return guestexec.MemoryHotplug{}, fmt.Errorf("guest memory hotplug connection closed")
		}
		return guestexec.MemoryHotplug{}, err
	}
	if resp.Error != "" {
		return guestexec.MemoryHotplug{}, fmt.Errorf("%s", resp.Error)
	}
	if resp.MemoryHotplug == nil {
		return guestexec.MemoryHotplug{}, fmt.Errorf("guest memory hotplug response missing status")
	}
	return *resp.MemoryHotplug, nil
}

var _ MemoryHotplugController = (*VM)(nil)
