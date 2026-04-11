package virtio

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	gclog "github.com/gocracker/gocracker/internal/log"
	"golang.org/x/sys/unix"
)

const (
	DeviceIDBalloon = 5

	balloonFeatureStatsVQ      = 1 << 1
	balloonFeatureDeflateOnOOM = 1 << 2

	balloonQueueInflate = 0
	balloonQueueDeflate = 1
	balloonQueueStats   = 2

	balloonPFNShift       = 12
	balloonPageSize       = 1 << balloonPFNShift
	balloonStatsEntrySize = 10
	balloonAutoInterval   = 5 * time.Second
	balloonAutoStepMiB    = 64

	balloonStatSwapIn = iota
	balloonStatSwapOut
	balloonStatMajorFaults
	balloonStatMinorFaults
	balloonStatFreeMemory
	balloonStatTotalMemory
	balloonStatAvailableMemory
	balloonStatDiskCaches
	balloonStatHugetlbAllocations
	balloonStatHugetlbFailures
	balloonStatOOMKill
	balloonStatAllocStall
	balloonStatAsyncScan
	balloonStatDirectScan
	balloonStatAsyncReclaim
	balloonStatDirectReclaim
)

type BalloonDeviceConfig struct {
	AmountMiB             uint64
	DeflateOnOOM          bool
	StatsPollingInterval  time.Duration
	AutoConservative      bool
	SnapshotPages         []uint32
}

type BalloonStats struct {
	TargetPages     uint64
	ActualPages     uint64
	TargetMiB       uint64
	ActualMiB       uint64
	SwapIn          uint64
	SwapOut         uint64
	MajorFaults     uint64
	MinorFaults     uint64
	FreeMemory      uint64
	TotalMemory     uint64
	AvailableMemory uint64
	DiskCaches      uint64
	HugetlbAllocs   uint64
	HugetlbFailures uint64
	OOMKill         uint64
	AllocStall      uint64
	AsyncScan       uint64
	DirectScan      uint64
	AsyncReclaim    uint64
	DirectReclaim   uint64
	UpdatedAt       time.Time
}

type BalloonDevice struct {
	*Transport

	mu sync.Mutex

	mem []byte

	targetPages uint32
	actualPages uint32

	deflateOnOOM         bool
	statsPollingInterval time.Duration
	statsEnabled         bool
	autoConservative     bool

	ballooned []bool
	stats     BalloonStats
	lastAuto  BalloonStats

	stopCh chan struct{}
	wakeCh chan struct{}
}

func NewBalloonDevice(mem []byte, basePA uint64, irq uint8, cfg BalloonDeviceConfig, dirty *DirtyTracker, irqFn func(bool)) *BalloonDevice {
	if cfg.AutoConservative && cfg.StatsPollingInterval <= 0 {
		cfg.StatsPollingInterval = balloonAutoInterval
	}
	d := &BalloonDevice{
		mem:                  mem,
		targetPages:          mibToBalloonPages(cfg.AmountMiB),
		deflateOnOOM:         cfg.DeflateOnOOM,
		statsPollingInterval: cfg.StatsPollingInterval,
		statsEnabled:         cfg.StatsPollingInterval > 0,
		autoConservative:     cfg.AutoConservative,
		ballooned:            make([]bool, len(mem)/balloonPageSize),
		stopCh:               make(chan struct{}),
		wakeCh:               make(chan struct{}, 1),
	}
	d.Transport = NewTransport(d, mem, basePA, irq, dirty, irqFn)
	if len(cfg.SnapshotPages) > 0 {
		d.restoreSnapshotPages(cfg.SnapshotPages)
	}
	if d.statsEnabled || d.autoConservative {
		go d.pollLoop()
	}
	return d
}

func (d *BalloonDevice) DeviceID() uint32 { return DeviceIDBalloon }

func (d *BalloonDevice) DeviceFeatures() uint64 {
	features := uint64(0)
	if d.statsEnabled {
		features |= balloonFeatureStatsVQ
	}
	if d.deflateOnOOM {
		features |= balloonFeatureDeflateOnOOM
	}
	return features
}

func (d *BalloonDevice) ConfigBytes() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := make([]byte, 16)
	binary.LittleEndian.PutUint32(cfg[0:], d.targetPages)
	binary.LittleEndian.PutUint32(cfg[4:], d.actualPages)
	return cfg
}

func (d *BalloonDevice) WriteConfig(offset uint32, data []byte) {
	if offset > 4 || len(data) == 0 {
		return
	}
	var actual uint32
	d.mu.Lock()
	actual = d.actualPages
	d.mu.Unlock()
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], actual)
	copy(raw[offset:], data)
	actual = binary.LittleEndian.Uint32(raw[:])

	d.mu.Lock()
	d.actualPages = actual
	d.refreshStatsLocked()
	d.mu.Unlock()
}

func (d *BalloonDevice) HandleQueue(idx uint32, q *Queue) {
	switch idx {
	case balloonQueueInflate:
		_ = d.handlePFNQueue(q, true)
	case balloonQueueDeflate:
		_ = d.handlePFNQueue(q, false)
	}
}

func (d *BalloonDevice) HandleQueueNotify(idx uint32, q *Queue) bool {
	switch idx {
	case balloonQueueInflate:
		return d.handlePFNQueue(q, true)
	case balloonQueueDeflate:
		return d.handlePFNQueue(q, false)
	case balloonQueueStats:
		// The driver keeps one stats buffer pending for the device to consume
		// later when the host polls statistics.
		return false
	default:
		return false
	}
}

func (d *BalloonDevice) Close() error {
	select {
	case <-d.stopCh:
	default:
		close(d.stopCh)
	}
	return nil
}

func (d *BalloonDevice) GetConfig() BalloonDeviceConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return BalloonDeviceConfig{
		AmountMiB:            balloonPagesToMiB(uint64(d.targetPages)),
		DeflateOnOOM:         d.deflateOnOOM,
		StatsPollingInterval: d.statsPollingInterval,
		AutoConservative:     d.autoConservative,
		SnapshotPages:        d.snapshotPagesLocked(),
	}
}

func (d *BalloonDevice) SetTargetMiB(amountMiB uint64) error {
	maxMiB := uint64(len(d.mem)) >> 20
	if amountMiB > maxMiB {
		return fmt.Errorf("balloon target %d MiB exceeds guest memory %d MiB", amountMiB, maxMiB)
	}
	d.mu.Lock()
	d.targetPages = mibToBalloonPages(amountMiB)
	d.stats.TargetPages = uint64(d.targetPages)
	d.stats.TargetMiB = amountMiB
	d.mu.Unlock()
	d.signalConfigChange()
	return nil
}

func (d *BalloonDevice) SetStatsPollingInterval(interval time.Duration) error {
	if interval < 0 {
		return fmt.Errorf("stats polling interval must be non-negative")
	}
	d.mu.Lock()
	d.statsPollingInterval = interval
	d.statsEnabled = interval > 0
	d.mu.Unlock()
	d.wakePoller()
	return nil
}

func (d *BalloonDevice) StatsPollingInterval() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statsPollingInterval
}

func (d *BalloonDevice) StatsEnabled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statsEnabled
}

func (d *BalloonDevice) Stats() BalloonStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.stats
	out.TargetPages = uint64(d.targetPages)
	out.ActualPages = uint64(d.actualPages)
	out.TargetMiB = balloonPagesToMiB(uint64(d.targetPages))
	out.ActualMiB = balloonPagesToMiB(uint64(d.actualPages))
	return out
}

func (d *BalloonDevice) SnapshotPages() []uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshotPagesLocked()
}

func (d *BalloonDevice) PollStats() (BalloonStats, error) {
	d.mu.Lock()
	statsEnabled := d.statsEnabled
	d.mu.Unlock()
	if !statsEnabled {
		return BalloonStats{}, fmt.Errorf("balloon statistics are not enabled")
	}
	q := d.Transport.Queue(balloonQueueStats)
	if q == nil || !q.Ready {
		stats := d.Stats()
		if stats.UpdatedAt.IsZero() {
			return BalloonStats{}, fmt.Errorf("balloon statistics queue is not ready")
		}
		return stats, nil
	}

	var (
		polled bool
		stats  BalloonStats
		pollErr error
	)
	consumed, err := q.ConsumeAvail(func(head uint16) {
		polled = true
		chain, err := q.WalkChain(head)
		if err != nil {
			pollErr = err
			_ = q.PushUsed(uint32(head), 0)
			return
		}
		stats, pollErr = d.readStatsBuffer(q, chain)
		_ = q.PushUsed(uint32(head), 0)
	})
	if err != nil {
		return BalloonStats{}, err
	}
	if !consumed || !polled {
		stats = d.Stats()
		if stats.UpdatedAt.IsZero() {
			return BalloonStats{}, fmt.Errorf("balloon statistics are not available yet")
		}
		return stats, nil
	}
	if pollErr != nil {
		return BalloonStats{}, pollErr
	}
	d.Transport.SetInterruptStat(1)
	d.Transport.SignalIRQ(true)
	return stats, nil
}

func (d *BalloonDevice) handlePFNQueue(q *Queue, inflate bool) bool {
	processed := false
	if err := q.IterAvail(func(head uint16) {
		processed = true
		chain, err := q.WalkChain(head)
		if err != nil {
			gclog.VMM.Warn("virtio-balloon invalid PFN descriptor chain", "head", head, "error", err)
			_ = q.PushUsed(uint32(head), 0)
			return
		}
		for _, desc := range chain {
			if desc.Flags&DescFlagWrite != 0 {
				continue
			}
			buf := make([]byte, desc.Len)
			if err := q.GuestRead(desc.Addr, buf); err != nil {
				gclog.VMM.Warn("virtio-balloon PFN guest read failed", "head", head, "error", err)
				break
			}
			for off := 0; off+4 <= len(buf); off += 4 {
				pfn := binary.LittleEndian.Uint32(buf[off:])
				if inflate {
					d.inflatePFN(pfn)
				} else {
					d.deflatePFN(pfn)
				}
			}
		}
		_ = q.PushUsed(uint32(head), 0)
	}); err != nil {
		gclog.VMM.Warn("virtio-balloon PFN queue iteration failed", "error", err)
	}
	if processed {
		d.refreshDynamicStats()
	}
	return processed
}

func (d *BalloonDevice) inflatePFN(pfn uint32) {
	index := int(pfn)
	if index < 0 || index >= len(d.ballooned) {
		return
	}

	d.mu.Lock()
	if d.ballooned[index] {
		d.mu.Unlock()
		return
	}
	d.ballooned[index] = true
	d.actualPages++
	d.refreshStatsLocked()
	d.mu.Unlock()

	start := uint64(pfn) << balloonPFNShift
	end := start + balloonPageSize
	if end > uint64(len(d.mem)) {
		end = uint64(len(d.mem))
	}
	if start >= end {
		return
	}
	if err := unix.Madvise(d.mem[start:end], unix.MADV_DONTNEED); err != nil {
		gclog.VMM.Warn("virtio-balloon reclaim page failed", "pfn", pfn, "error", err)
	}
}

func (d *BalloonDevice) deflatePFN(pfn uint32) {
	index := int(pfn)
	if index < 0 || index >= len(d.ballooned) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ballooned[index] {
		return
	}
	d.ballooned[index] = false
	if d.actualPages > 0 {
		d.actualPages--
	}
	d.refreshStatsLocked()
}

func (d *BalloonDevice) restoreSnapshotPages(pfns []uint32) {
	for _, pfn := range pfns {
		d.inflatePFN(pfn)
	}
}

func (d *BalloonDevice) refreshDynamicStats() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshStatsLocked()
}

func (d *BalloonDevice) refreshStatsLocked() {
	d.stats.TargetPages = uint64(d.targetPages)
	d.stats.ActualPages = uint64(d.actualPages)
	d.stats.TargetMiB = balloonPagesToMiB(uint64(d.targetPages))
	d.stats.ActualMiB = balloonPagesToMiB(uint64(d.actualPages))
	if d.stats.UpdatedAt.IsZero() {
		d.stats.UpdatedAt = time.Now()
	}
}

func (d *BalloonDevice) readStatsBuffer(q *Queue, chain []Desc) (BalloonStats, error) {
	var raw []byte
	for _, desc := range chain {
		if desc.Flags&DescFlagWrite != 0 {
			continue
		}
		buf := make([]byte, desc.Len)
		if err := q.GuestRead(desc.Addr, buf); err != nil {
			return BalloonStats{}, err
		}
		raw = append(raw, buf...)
	}
	if len(raw) < balloonStatsEntrySize {
		return BalloonStats{}, fmt.Errorf("balloon statistics buffer too small")
	}
	stats := d.Stats()
	for off := 0; off+balloonStatsEntrySize <= len(raw); off += balloonStatsEntrySize {
		tag := binary.LittleEndian.Uint16(raw[off:])
		val := binary.LittleEndian.Uint64(raw[off+2:])
		switch tag {
		case balloonStatSwapIn:
			stats.SwapIn = val
		case balloonStatSwapOut:
			stats.SwapOut = val
		case balloonStatMajorFaults:
			stats.MajorFaults = val
		case balloonStatMinorFaults:
			stats.MinorFaults = val
		case balloonStatFreeMemory:
			stats.FreeMemory = val
		case balloonStatTotalMemory:
			stats.TotalMemory = val
		case balloonStatAvailableMemory:
			stats.AvailableMemory = val
		case balloonStatDiskCaches:
			stats.DiskCaches = val
		case balloonStatHugetlbAllocations:
			stats.HugetlbAllocs = val
		case balloonStatHugetlbFailures:
			stats.HugetlbFailures = val
		case balloonStatOOMKill:
			stats.OOMKill = val
		case balloonStatAllocStall:
			stats.AllocStall = val
		case balloonStatAsyncScan:
			stats.AsyncScan = val
		case balloonStatDirectScan:
			stats.DirectScan = val
		case balloonStatAsyncReclaim:
			stats.AsyncReclaim = val
		case balloonStatDirectReclaim:
			stats.DirectReclaim = val
		}
	}
	stats.UpdatedAt = time.Now()

	d.mu.Lock()
	d.stats = stats
	d.mu.Unlock()
	return stats, nil
}

func (d *BalloonDevice) signalConfigChange() {
	d.Transport.SetInterruptStat(2)
	d.Transport.SignalIRQ(true)
}

func (d *BalloonDevice) wakePoller() {
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}

func (d *BalloonDevice) pollLoop() {
	for {
		interval := d.nextPollInterval()
		if interval <= 0 {
			select {
			case <-d.stopCh:
				return
			case <-d.wakeCh:
				continue
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-d.stopCh:
			timer.Stop()
			return
		case <-d.wakeCh:
			if !timer.Stop() {
				<-timer.C
			}
			continue
		case <-timer.C:
			stats, err := d.PollStats()
			if err == nil {
				d.applyConservativePolicy(stats)
			}
		}
	}
}

func (d *BalloonDevice) nextPollInterval() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.statsPollingInterval > 0 {
		return d.statsPollingInterval
	}
	if d.autoConservative {
		return balloonAutoInterval
	}
	return 0
}

func (d *BalloonDevice) applyConservativePolicy(stats BalloonStats) {
	d.mu.Lock()
	if !d.autoConservative {
		d.mu.Unlock()
		return
	}
	currentTargetMiB := balloonPagesToMiB(uint64(d.targetPages))
	baseMemMiB := uint64(len(d.mem)) >> 20
	last := d.lastAuto
	d.lastAuto = stats
	d.mu.Unlock()

	if stats.TotalMemory == 0 && stats.AvailableMemory == 0 {
		return
	}

	reserveMiB := uint64(128)
	if baseMemMiB <= 512 {
		reserveMiB = 64
	}
	highWaterMiB := reserveMiB * 2
	availableMiB := stats.AvailableMemory >> 20
	pressure := stats.OOMKill > last.OOMKill || stats.AllocStall > last.AllocStall || availableMiB < reserveMiB

	var nextTargetMiB uint64
	switch {
	case pressure:
		if currentTargetMiB == 0 {
			return
		}
		if currentTargetMiB > balloonAutoStepMiB {
			nextTargetMiB = currentTargetMiB - balloonAutoStepMiB
		}
	case availableMiB > highWaterMiB+balloonAutoStepMiB:
		nextTargetMiB = currentTargetMiB + minUint64(balloonAutoStepMiB, availableMiB-highWaterMiB)
	default:
		return
	}

	maxTargetMiB := uint64(0)
	if baseMemMiB > reserveMiB {
		maxTargetMiB = baseMemMiB - reserveMiB
	}
	if nextTargetMiB > maxTargetMiB {
		nextTargetMiB = maxTargetMiB
	}
	if nextTargetMiB == currentTargetMiB {
		return
	}
	if err := d.SetTargetMiB(nextTargetMiB); err != nil {
		gclog.VMM.Warn("virtio-balloon auto policy update failed", "target_mib", nextTargetMiB, "error", err)
	}
}

func (d *BalloonDevice) snapshotPagesLocked() []uint32 {
	out := make([]uint32, 0, d.actualPages)
	for i, ballooned := range d.ballooned {
		if ballooned {
			out = append(out, uint32(i))
		}
	}
	return out
}

func mibToBalloonPages(miB uint64) uint32 {
	return uint32(miB * 256)
}

func balloonPagesToMiB(pages uint64) uint64 {
	return pages / 256
}

func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
