package virtio

import (
	"encoding/binary"
	"testing"
	"time"
)

func writeTestDesc(mem []byte, descBase uint64, idx int, addr uint64, length uint32, flags uint16, next uint16) {
	off := descBase + uint64(idx)*16
	binary.LittleEndian.PutUint64(mem[off:], addr)
	binary.LittleEndian.PutUint32(mem[off+8:], length)
	binary.LittleEndian.PutUint16(mem[off+12:], flags)
	binary.LittleEndian.PutUint16(mem[off+14:], next)
}

func TestBalloonDeviceInflateDeflate(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{StatsPollingInterval: time.Second}, nil, nil)
	defer dev.Close()

	const (
		descBase  = uint64(0x2000)
		availBase = uint64(0x3000)
		usedBase  = uint64(0x4000)
		pfnBase   = uint64(0x5000)
	)

	qInflate := dev.Transport.Queue(balloonQueueInflate)
	setupQueueInMemory(mem, qInflate, descBase, availBase, usedBase)
	qInflate.Ready = true

	pfns := make([]byte, 256*4)
	for i := 0; i < 256; i++ {
		binary.LittleEndian.PutUint32(pfns[i*4:], uint32(i+1))
	}
	copy(mem[pfnBase:], pfns)
	writeTestDesc(mem, descBase, 0, pfnBase, uint32(len(pfns)), 0, 0)
	writeAvailEntry(mem, availBase, 0, 0)
	dev.Transport.Write(RegQueueNotify, balloonQueueInflate)

	stats := dev.Stats()
	if stats.ActualPages != 256 {
		t.Fatalf("actual_pages = %d, want 256", stats.ActualPages)
	}
	if stats.ActualMiB != 1 {
		t.Fatalf("actual_mib = %d, want 1", stats.ActualMiB)
	}

	qDeflate := dev.Transport.Queue(balloonQueueDeflate)
	setupQueueInMemory(mem, qDeflate, descBase+0x1000, availBase+0x1000, usedBase+0x1000)
	qDeflate.Ready = true
	copy(mem[pfnBase+0x1000:], pfns)
	writeTestDesc(mem, descBase+0x1000, 0, pfnBase+0x1000, uint32(len(pfns)), 0, 0)
	writeAvailEntry(mem, availBase+0x1000, 0, 0)
	dev.Transport.Write(RegQueueNotify, balloonQueueDeflate)

	stats = dev.Stats()
	if stats.ActualPages != 0 || stats.ActualMiB != 0 {
		t.Fatalf("actual after deflate = pages:%d mib:%d, want 0/0", stats.ActualPages, stats.ActualMiB)
	}

	if err := dev.SetTargetMiB(8); err == nil {
		t.Fatal("SetTargetMiB should reject target larger than guest memory")
	}
	if err := dev.SetTargetMiB(2); err != nil {
		t.Fatalf("SetTargetMiB(2): %v", err)
	}
	if got := dev.Transport.InterruptStat(); got&2 == 0 {
		t.Fatalf("config change interrupt bit not raised: %#x", got)
	}
}

func TestBalloonDevicePollStats(t *testing.T) {
	mem := make([]byte, 128<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{AmountMiB: 64, StatsPollingInterval: time.Second}, nil, nil)
	defer dev.Close()

	const (
		descBase  = uint64(0x6000)
		availBase = uint64(0x7000)
		usedBase  = uint64(0x8000)
		statsBase = uint64(0x9000)
	)

	qStats := dev.Transport.Queue(balloonQueueStats)
	setupQueueInMemory(mem, qStats, descBase, availBase, usedBase)
	qStats.Ready = true

	raw := make([]byte, balloonStatsEntrySize*3)
	binary.LittleEndian.PutUint16(raw[0:], balloonStatAvailableMemory)
	binary.LittleEndian.PutUint64(raw[2:], 321<<20)
	binary.LittleEndian.PutUint16(raw[10:], balloonStatTotalMemory)
	binary.LittleEndian.PutUint64(raw[12:], 512<<20)
	binary.LittleEndian.PutUint16(raw[20:], balloonStatOOMKill)
	binary.LittleEndian.PutUint64(raw[22:], 3)
	copy(mem[statsBase:], raw)
	writeTestDesc(mem, descBase, 0, statsBase, uint32(len(raw)), 0, 0)
	writeAvailEntry(mem, availBase, 0, 0)

	stats, err := dev.PollStats()
	if err != nil {
		t.Fatalf("PollStats(): %v", err)
	}
	if stats.AvailableMemory != 321<<20 {
		t.Fatalf("available_memory = %d, want %d", stats.AvailableMemory, uint64(321<<20))
	}
	if stats.TotalMemory != 512<<20 {
		t.Fatalf("total_memory = %d, want %d", stats.TotalMemory, uint64(512<<20))
	}
	if stats.OOMKill != 3 {
		t.Fatalf("oom_kill = %d, want 3", stats.OOMKill)
	}
	if stats.TargetMiB != 64 {
		t.Fatalf("target_mib = %d, want 64", stats.TargetMiB)
	}
}

func TestBalloonDeviceFeatures_WithStats(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{StatsPollingInterval: time.Second, DeflateOnOOM: true}, nil, nil)
	defer dev.Close()

	features := dev.DeviceFeatures()
	if features == 0 {
		t.Fatal("expected non-zero features")
	}
}

func TestBalloonDeviceID(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	if dev.DeviceID() != 5 {
		t.Fatalf("DeviceID = %d, want 5", dev.DeviceID())
	}
}

func TestBalloonDeviceConfigBytes(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	cfg := dev.ConfigBytes()
	if len(cfg) != 16 {
		t.Fatalf("ConfigBytes len = %d, want 16", len(cfg))
	}
}

func TestBalloonDeviceSetTargetMiB(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	if err := dev.SetTargetMiB(2); err != nil {
		t.Fatalf("SetTargetMiB: %v", err)
	}
	if balloonPagesToMiB(uint64(dev.targetPages)) != 2 {
		t.Fatalf("TargetMiB = %d, want 128", balloonPagesToMiB(uint64(dev.targetPages)))
	}
}

func TestBalloonDeviceDeflateOnOOM(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{DeflateOnOOM: true}, nil, nil)
	defer dev.Close()

	if !dev.deflateOnOOM {
		t.Fatal("deflateOnOOM should be true")
	}
	dev2 := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{DeflateOnOOM: false}, nil, nil)
	defer dev2.Close()
	if dev2.deflateOnOOM {
		t.Fatal("deflateOnOOM should be false")
	}
}

func TestBalloonDeviceSetStatsPollingInterval(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	dev.SetStatsPollingInterval(5 * time.Second)
	if dev.StatsPollingInterval() != 5*time.Second {
		t.Fatalf("StatsPollingInterval = %v, want 5s", dev.StatsPollingInterval())
	}
}

func TestBalloonDeviceAutoConservativeConfig(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{AutoConservative: true}, nil, nil)
	defer dev.Close()

	if !dev.autoConservative {
		t.Fatal("autoConservative should be true")
	}
}

func TestBalloonDeviceActualPages(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	if dev.actualPages != 0 {
		t.Fatalf("initial ActualPages = %d, want 0", dev.actualPages)
	}
}

func TestBalloonHelpers(t *testing.T) {
	if mibToBalloonPages(1) != 256 {
		t.Fatalf("mibToBalloonPages(1) = %d, want 256", mibToBalloonPages(1))
	}
	if balloonPagesToMiB(256) != 1 {
		t.Fatalf("balloonPagesToMiB(256) = %d, want 1", balloonPagesToMiB(256))
	}
	if minUint64(10, 20) != 10 {
		t.Fatal("minUint64(10, 20) should be 10")
	}
	if minUint64(20, 10) != 10 {
		t.Fatal("minUint64(20, 10) should be 10")
	}
}

func TestBalloonDeviceNextPollInterval(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{
		StatsPollingInterval: 3 * time.Second,
	}, nil, nil)
	defer dev.Close()

	interval := dev.nextPollInterval()
	if interval != 3*time.Second {
		t.Fatalf("nextPollInterval = %v, want 3s", interval)
	}
}

func TestBalloonDeviceNextPollInterval_AutoConservative(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{
		AutoConservative: true,
	}, nil, nil)
	defer dev.Close()

	interval := dev.nextPollInterval()
	if interval != balloonAutoInterval {
		t.Fatalf("nextPollInterval = %v, want %v", interval, balloonAutoInterval)
	}
}

func TestBalloonDeviceNextPollInterval_NoPoll(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	interval := dev.nextPollInterval()
	if interval != 0 {
		t.Fatalf("nextPollInterval = %v, want 0", interval)
	}
}

func TestBalloonConservativePolicy_NoPressureNoHighWater(t *testing.T) {
	mem := make([]byte, 256<<20) // 256 MiB
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{
		AutoConservative: true,
	}, nil, nil)
	defer dev.Close()

	// No pressure, available within normal range
	stats := BalloonStats{
		TotalMemory:     200 << 20,
		AvailableMemory: 100 << 20, // 100 MiB available
	}
	dev.applyConservativePolicy(stats)
}

func TestBalloonConservativePolicy_Pressure(t *testing.T) {
	mem := make([]byte, 256<<20) // 256 MiB
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{
		AutoConservative: true,
	}, nil, nil)
	defer dev.Close()

	// Set a target first
	dev.SetTargetMiB(64)

	// Simulate pressure via low available memory
	stats := BalloonStats{
		TotalMemory:     200 << 20,
		AvailableMemory: 10 << 20, // 10 MiB - below reserve
	}
	dev.applyConservativePolicy(stats)
}

func TestBalloonConservativePolicy_ZeroStats(t *testing.T) {
	mem := make([]byte, 256<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{
		AutoConservative: true,
	}, nil, nil)
	defer dev.Close()

	// Zero stats should be a no-op
	stats := BalloonStats{}
	dev.applyConservativePolicy(stats)
}

func TestBalloonConservativePolicy_NotAutoConservative(t *testing.T) {
	mem := make([]byte, 256<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	// Should return early when not in auto-conservative mode
	stats := BalloonStats{
		TotalMemory:     200 << 20,
		AvailableMemory: 10 << 20,
	}
	dev.applyConservativePolicy(stats)
}

func TestBalloonSnapshotPagesLocked(t *testing.T) {
	mem := make([]byte, 4<<20)
	dev := NewBalloonDevice(mem, 0x1000, 5, BalloonDeviceConfig{}, nil, nil)
	defer dev.Close()

	dev.mu.Lock()
	pages := dev.snapshotPagesLocked()
	dev.mu.Unlock()
	if len(pages) != 0 {
		t.Fatalf("expected empty snapshot pages, got %d", len(pages))
	}
}
