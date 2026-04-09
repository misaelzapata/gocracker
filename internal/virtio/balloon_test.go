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
