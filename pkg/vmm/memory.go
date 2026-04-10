package vmm

import "time"

type BalloonAutoMode string

const (
	BalloonAutoOff          BalloonAutoMode = "off"
	BalloonAutoConservative BalloonAutoMode = "conservative"
)

type BalloonConfig struct {
	AmountMiB             uint64          `json:"amount_mib,omitempty"`
	DeflateOnOOM          bool            `json:"deflate_on_oom,omitempty"`
	StatsPollingIntervalS int             `json:"stats_polling_interval_s,omitempty"`
	FreePageHinting       bool            `json:"free_page_hinting,omitempty"`
	FreePageReporting     bool            `json:"free_page_reporting,omitempty"`
	Auto                  BalloonAutoMode `json:"auto,omitempty"`

	// SnapshotPages persists the current ballooned PFNs so snapshot/restore can
	// re-apply host-side reclaim on the restored guest RAM.
	SnapshotPages []uint32 `json:"snapshot_pages,omitempty"`
}

type BalloonUpdate struct {
	AmountMiB uint64 `json:"amount_mib"`
}

type BalloonStatsUpdate struct {
	StatsPollingIntervalS int `json:"stats_polling_interval_s"`
}

type BalloonStats struct {
	TargetPages     uint64    `json:"target_pages"`
	ActualPages     uint64    `json:"actual_pages"`
	TargetMiB       uint64    `json:"target_mib"`
	ActualMiB       uint64    `json:"actual_mib"`
	SwapIn          uint64    `json:"swap_in,omitempty"`
	SwapOut         uint64    `json:"swap_out,omitempty"`
	MajorFaults     uint64    `json:"major_faults,omitempty"`
	MinorFaults     uint64    `json:"minor_faults,omitempty"`
	FreeMemory      uint64    `json:"free_memory,omitempty"`
	TotalMemory     uint64    `json:"total_memory,omitempty"`
	AvailableMemory uint64    `json:"available_memory,omitempty"`
	DiskCaches      uint64    `json:"disk_caches,omitempty"`
	HugetlbAllocs   uint64    `json:"hugetlb_allocations,omitempty"`
	HugetlbFailures uint64    `json:"hugetlb_failures,omitempty"`
	OOMKill         uint64    `json:"oom_kill,omitempty"`
	AllocStall      uint64    `json:"alloc_stall,omitempty"`
	AsyncScan       uint64    `json:"async_scan,omitempty"`
	DirectScan      uint64    `json:"direct_scan,omitempty"`
	AsyncReclaim    uint64    `json:"async_reclaim,omitempty"`
	DirectReclaim   uint64    `json:"direct_reclaim,omitempty"`
	UpdatedAt       time.Time `json:"-"`
}

type MemoryHotplugConfig struct {
	TotalSizeMiB uint64 `json:"total_size_mib,omitempty"`
	SlotSizeMiB  uint64 `json:"slot_size_mib,omitempty"`
	BlockSizeMiB uint64 `json:"block_size_mib,omitempty"`
}

type MemoryHotplugSizeUpdate struct {
	RequestedSizeMiB uint64 `json:"requested_size_mib,omitempty"`
}

type MemoryHotplugStatus struct {
	TotalSizeMiB     uint64 `json:"total_size_mib,omitempty"`
	SlotSizeMiB      uint64 `json:"slot_size_mib,omitempty"`
	BlockSizeMiB     uint64 `json:"block_size_mib,omitempty"`
	PluggedSizeMiB   uint64 `json:"plugged_size_mib,omitempty"`
	RequestedSizeMiB uint64 `json:"requested_size_mib,omitempty"`
}

type BalloonController interface {
	GetBalloonConfig() (BalloonConfig, error)
	UpdateBalloon(BalloonUpdate) error
	GetBalloonStats() (BalloonStats, error)
	UpdateBalloonStats(BalloonStatsUpdate) error
}

type MemoryHotplugController interface {
	GetMemoryHotplug() (MemoryHotplugStatus, error)
	UpdateMemoryHotplug(MemoryHotplugSizeUpdate) error
}
