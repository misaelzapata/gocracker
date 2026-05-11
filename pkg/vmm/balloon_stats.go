//go:build linux

package vmm

import (
	"fmt"
	"net"
	"time"

	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/internal/virtio"
)

const balloonAutoInterval = 5 * time.Second
const balloonAutoStepMiB = 64

func (m *VM) readGuestMemoryStats(cfg Config) (guestexec.MemoryStats, error) {
	if cfg.Exec == nil || !cfg.Exec.Enabled {
		return guestexec.MemoryStats{}, fmt.Errorf("guest exec agent is not enabled")
	}
	conn, err := m.DialVsock(execAgentPort(cfg))
	if err != nil {
		return guestexec.MemoryStats{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := guestexec.Encode(conn, guestexec.Request{Mode: guestexec.ModeMemoryStats}); err != nil {
		return guestexec.MemoryStats{}, err
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		if err == net.ErrClosed {
			return guestexec.MemoryStats{}, fmt.Errorf("guest memory stats connection closed")
		}
		return guestexec.MemoryStats{}, err
	}
	if resp.Error != "" {
		return guestexec.MemoryStats{}, fmt.Errorf("%s", resp.Error)
	}
	if resp.MemoryStats == nil {
		return guestexec.MemoryStats{}, fmt.Errorf("guest memory stats missing from response")
	}
	return *resp.MemoryStats, nil
}

func mergeBalloonStats(base virtio.BalloonStats, extra guestexec.MemoryStats) virtio.BalloonStats {
	base.SwapIn = extra.SwapIn
	base.SwapOut = extra.SwapOut
	base.MajorFaults = extra.MajorFaults
	base.MinorFaults = extra.MinorFaults
	base.FreeMemory = extra.FreeMemory
	base.TotalMemory = extra.TotalMemory
	base.AvailableMemory = extra.AvailableMemory
	base.DiskCaches = extra.DiskCaches
	base.OOMKill = extra.OOMKill
	base.AllocStall = extra.AllocStall
	base.AsyncScan = extra.AsyncScan
	base.DirectScan = extra.DirectScan
	base.AsyncReclaim = extra.AsyncReclaim
	base.DirectReclaim = extra.DirectReclaim
	base.UpdatedAt = time.Now()
	return base
}

func execAgentPort(cfg Config) uint32 {
	if cfg.Exec != nil && cfg.Exec.Enabled && cfg.Exec.VsockPort != 0 {
		return cfg.Exec.VsockPort
	}
	return guestexec.DefaultVsockPort
}

func (m *VM) balloonAutoLoop() {
	ticker := time.NewTicker(balloonAutoInterval)
	defer ticker.Stop()

	var last BalloonStats
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
		}

		stats, err := m.GetBalloonStats()
		if err != nil {
			continue
		}
		nextTargetMiB, ok := nextConservativeBalloonTarget(stats, last, m.cfg.MemMB)
		last = stats
		if !ok || nextTargetMiB == stats.TargetMiB {
			continue
		}
		_ = m.UpdateBalloon(BalloonUpdate{AmountMiB: nextTargetMiB})
	}
}

func nextConservativeBalloonTarget(stats, last BalloonStats, baseMemMiB uint64) (uint64, bool) {
	if stats.TotalMemory == 0 && stats.AvailableMemory == 0 {
		return 0, false
	}

	reserveMiB := uint64(128)
	if baseMemMiB <= 512 {
		reserveMiB = 64
	}
	highWaterMiB := reserveMiB * 2
	availableMiB := stats.AvailableMemory >> 20
	currentTargetMiB := stats.TargetMiB
	pressure := stats.OOMKill > last.OOMKill || stats.AllocStall > last.AllocStall || availableMiB < reserveMiB

	var nextTargetMiB uint64
	switch {
	case pressure:
		if currentTargetMiB == 0 {
			return 0, false
		}
		if currentTargetMiB > balloonAutoStepMiB {
			nextTargetMiB = currentTargetMiB - balloonAutoStepMiB
		}
	case availableMiB > highWaterMiB+balloonAutoStepMiB:
		headroom := availableMiB - highWaterMiB
		if headroom > balloonAutoStepMiB {
			headroom = balloonAutoStepMiB
		}
		nextTargetMiB = currentTargetMiB + headroom
	default:
		return 0, false
	}

	maxTargetMiB := uint64(0)
	if baseMemMiB > reserveMiB {
		maxTargetMiB = baseMemMiB - reserveMiB
	}
	if nextTargetMiB > maxTargetMiB {
		nextTargetMiB = maxTargetMiB
	}
	return nextTargetMiB, nextTargetMiB != currentTargetMiB
}
