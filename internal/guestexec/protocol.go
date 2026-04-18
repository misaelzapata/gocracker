package guestexec

import (
	"encoding/json"
	"fmt"
	"io"
)

const DefaultVsockPort = 10022

const (
	ModeExec        = "exec"
	ModeStream      = "stream"
	ModeMemoryStats = "memory_stats"
	ModeMemoryHotplugGet    = "memory_hotplug_get"
	ModeMemoryHotplugUpdate = "memory_hotplug_update"
	// ModeResize applies TIOCSWINSZ to the currently active PTY master inside
	// the guest. The host sends this via a short-lived separate vsock connection
	// when the user resizes the terminal window mid-session. Columns and Rows
	// from the Request are used; Command is not required.
	ModeResize = "resize"
)

type Config struct {
	Enabled   bool   `json:"enabled,omitempty"`
	VsockPort uint32 `json:"vsock_port,omitempty"`
}

func (c Config) Port() uint32 {
	if c.VsockPort != 0 {
		return c.VsockPort
	}
	return DefaultVsockPort
}

type Request struct {
	Mode    string   `json:"mode"`
	Command []string `json:"command,omitempty"`
	Columns int      `json:"columns,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	// Stdin is consumed by the guest process on stdin (one-shot exec only).
	Stdin string `json:"stdin,omitempty"`
	// Env is appended to the guest process environment as KEY=VALUE.
	Env []string `json:"env,omitempty"`
	// WorkDir overrides the guest working directory for this single exec.
	WorkDir string `json:"workdir,omitempty"`
	// Memory hotplug configuration/state for in-guest control.
	MemoryHotplugBaseAddr     uint64 `json:"memory_hotplug_base_addr,omitempty"`
	MemoryHotplugTotalBytes   uint64 `json:"memory_hotplug_total_bytes,omitempty"`
	MemoryHotplugBlockBytes   uint64 `json:"memory_hotplug_block_bytes,omitempty"`
	MemoryHotplugTargetBytes  uint64 `json:"memory_hotplug_target_bytes,omitempty"`
}

func (r Request) Validate() error {
	switch r.Mode {
	case ModeExec:
		if len(r.Command) == 0 {
			return fmt.Errorf("command is required")
		}
		return nil
	case ModeStream:
		return nil
	case ModeMemoryStats:
		return nil
	case ModeMemoryHotplugGet:
		if r.MemoryHotplugBaseAddr == 0 {
			return fmt.Errorf("memory_hotplug_base_addr is required")
		}
		if r.MemoryHotplugTotalBytes == 0 {
			return fmt.Errorf("memory_hotplug_total_bytes is required")
		}
		return nil
	case ModeMemoryHotplugUpdate:
		if r.MemoryHotplugBaseAddr == 0 {
			return fmt.Errorf("memory_hotplug_base_addr is required")
		}
		if r.MemoryHotplugTotalBytes == 0 {
			return fmt.Errorf("memory_hotplug_total_bytes is required")
		}
		return nil
	case ModeResize:
		if r.Columns <= 0 || r.Rows <= 0 {
			return fmt.Errorf("resize requires columns > 0 and rows > 0")
		}
		return nil
	default:
		return fmt.Errorf("unsupported exec mode %q", r.Mode)
	}
}

type MemoryStats struct {
	SwapIn          uint64 `json:"swap_in,omitempty"`
	SwapOut         uint64 `json:"swap_out,omitempty"`
	MajorFaults     uint64 `json:"major_faults,omitempty"`
	MinorFaults     uint64 `json:"minor_faults,omitempty"`
	FreeMemory      uint64 `json:"free_memory,omitempty"`
	TotalMemory     uint64 `json:"total_memory,omitempty"`
	AvailableMemory uint64 `json:"available_memory,omitempty"`
	DiskCaches      uint64 `json:"disk_caches,omitempty"`
	OOMKill         uint64 `json:"oom_kill,omitempty"`
	AllocStall      uint64 `json:"alloc_stall,omitempty"`
	AsyncScan       uint64 `json:"async_scan,omitempty"`
	DirectScan      uint64 `json:"direct_scan,omitempty"`
	AsyncReclaim    uint64 `json:"async_reclaim,omitempty"`
	DirectReclaim   uint64 `json:"direct_reclaim,omitempty"`
}

type MemoryHotplug struct {
	BlockSizeBytes uint64 `json:"block_size_bytes,omitempty"`
	RequestedBytes uint64 `json:"requested_bytes,omitempty"`
	PluggedBytes   uint64 `json:"plugged_bytes,omitempty"`
	OnlineBlocks   uint64 `json:"online_blocks,omitempty"`
	PresentBlocks  uint64 `json:"present_blocks,omitempty"`
}

type Response struct {
	OK          bool         `json:"ok,omitempty"`
	Error       string       `json:"error,omitempty"`
	Stdout      string       `json:"stdout,omitempty"`
	Stderr      string       `json:"stderr,omitempty"`
	ExitCode    int          `json:"exit_code,omitempty"`
	MemoryStats *MemoryStats `json:"memory_stats,omitempty"`
	MemoryHotplug *MemoryHotplug `json:"memory_hotplug,omitempty"`
}

func Encode(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func Decode(r io.Reader, value any) error {
	return json.NewDecoder(r).Decode(value)
}
