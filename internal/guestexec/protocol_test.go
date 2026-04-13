package guestexec

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigPort_Default(t *testing.T) {
	c := Config{}
	if got := c.Port(); got != DefaultVsockPort {
		t.Fatalf("Port() = %d, want %d", got, DefaultVsockPort)
	}
}

func TestConfigPort_Custom(t *testing.T) {
	c := Config{VsockPort: 9999}
	if got := c.Port(); got != 9999 {
		t.Fatalf("Port() = %d, want 9999", got)
	}
}

func TestConfigPort_EnabledNoPort(t *testing.T) {
	c := Config{Enabled: true}
	if got := c.Port(); got != DefaultVsockPort {
		t.Fatalf("Port() = %d, want %d", got, DefaultVsockPort)
	}
}

func TestConfigPort_EnabledWithPort(t *testing.T) {
	c := Config{Enabled: true, VsockPort: 5555}
	if got := c.Port(); got != 5555 {
		t.Fatalf("Port() = %d, want 5555", got)
	}
}

func TestRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     Request
		wantErr bool
		errMsg  string
	}{
		{
			name:    "exec valid",
			req:     Request{Mode: ModeExec, Command: []string{"ls", "-la"}},
			wantErr: false,
		},
		{
			name:    "exec empty command",
			req:     Request{Mode: ModeExec},
			wantErr: true,
			errMsg:  "command is required",
		},
		{
			name:    "stream valid",
			req:     Request{Mode: ModeStream},
			wantErr: false,
		},
		{
			name:    "memory_stats valid",
			req:     Request{Mode: ModeMemoryStats},
			wantErr: false,
		},
		{
			name: "memory_hotplug_get valid",
			req: Request{
				Mode:                    ModeMemoryHotplugGet,
				MemoryHotplugBaseAddr:   0x100000000,
				MemoryHotplugTotalBytes: 1 << 30,
			},
			wantErr: false,
		},
		{
			name: "memory_hotplug_get missing base",
			req: Request{
				Mode:                    ModeMemoryHotplugGet,
				MemoryHotplugTotalBytes: 1 << 30,
			},
			wantErr: true,
			errMsg:  "memory_hotplug_base_addr is required",
		},
		{
			name: "memory_hotplug_get missing total",
			req: Request{
				Mode:                  ModeMemoryHotplugGet,
				MemoryHotplugBaseAddr: 0x100000000,
			},
			wantErr: true,
			errMsg:  "memory_hotplug_total_bytes is required",
		},
		{
			name: "memory_hotplug_update valid",
			req: Request{
				Mode:                    ModeMemoryHotplugUpdate,
				MemoryHotplugBaseAddr:   0x100000000,
				MemoryHotplugTotalBytes: 1 << 30,
			},
			wantErr: false,
		},
		{
			name: "memory_hotplug_update missing base",
			req: Request{
				Mode:                    ModeMemoryHotplugUpdate,
				MemoryHotplugTotalBytes: 1 << 30,
			},
			wantErr: true,
			errMsg:  "memory_hotplug_base_addr is required",
		},
		{
			name: "memory_hotplug_update missing total",
			req: Request{
				Mode:                  ModeMemoryHotplugUpdate,
				MemoryHotplugBaseAddr: 0x100000000,
			},
			wantErr: true,
			errMsg:  "memory_hotplug_total_bytes is required",
		},
		{
			name:    "unknown mode",
			req:     Request{Mode: "bogus"},
			wantErr: true,
			errMsg:  "unsupported exec mode",
		},
		{
			name:    "empty mode",
			req:     Request{},
			wantErr: true,
			errMsg:  "unsupported exec mode",
		},
		{
			name:    "exec with env",
			req:     Request{Mode: ModeExec, Command: []string{"env"}, Env: []string{"A=1"}},
			wantErr: false,
		},
		{
			name:    "stream with columns and rows",
			req:     Request{Mode: ModeStream, Columns: 80, Rows: 24},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("error = %q, want to contain %q", err, tt.errMsg)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip_ExecRequest(t *testing.T) {
	req := Request{
		Mode:    ModeExec,
		Command: []string{"ls", "-la", "/tmp"},
		Env:     []string{"HOME=/root", "LANG=C"},
		WorkDir: "/app",
		Stdin:   "input data",
	}
	var buf bytes.Buffer
	if err := Encode(&buf, req); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded Request
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Mode != ModeExec {
		t.Fatalf("Mode = %q, want exec", decoded.Mode)
	}
	if len(decoded.Command) != 3 || decoded.Command[0] != "ls" {
		t.Fatalf("Command = %v", decoded.Command)
	}
	if len(decoded.Env) != 2 {
		t.Fatalf("Env = %v", decoded.Env)
	}
	if decoded.WorkDir != "/app" {
		t.Fatalf("WorkDir = %q", decoded.WorkDir)
	}
	if decoded.Stdin != "input data" {
		t.Fatalf("Stdin = %q", decoded.Stdin)
	}
}

func TestEncodeDecodeRoundTrip_StreamRequest(t *testing.T) {
	req := Request{
		Mode:    ModeStream,
		Columns: 120,
		Rows:    40,
	}
	var buf bytes.Buffer
	if err := Encode(&buf, req); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Request
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Mode != ModeStream || decoded.Columns != 120 || decoded.Rows != 40 {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestEncodeDecodeRoundTrip_MemoryStatsRequest(t *testing.T) {
	req := Request{Mode: ModeMemoryStats}
	var buf bytes.Buffer
	if err := Encode(&buf, req); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Request
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Mode != ModeMemoryStats {
		t.Fatalf("Mode = %q", decoded.Mode)
	}
}

func TestEncodeDecodeRoundTrip_HotplugRequest(t *testing.T) {
	req := Request{
		Mode:                     ModeMemoryHotplugUpdate,
		MemoryHotplugBaseAddr:    0x100000000,
		MemoryHotplugTotalBytes:  2 << 30,
		MemoryHotplugBlockBytes:  128 << 20,
		MemoryHotplugTargetBytes: 512 << 20,
	}
	var buf bytes.Buffer
	if err := Encode(&buf, req); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Request
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.MemoryHotplugBaseAddr != 0x100000000 {
		t.Fatalf("BaseAddr = %x", decoded.MemoryHotplugBaseAddr)
	}
	if decoded.MemoryHotplugTargetBytes != 512<<20 {
		t.Fatalf("TargetBytes = %d", decoded.MemoryHotplugTargetBytes)
	}
}

func TestDecodeResponse_WithExecResult(t *testing.T) {
	resp := Response{
		OK:       true,
		Stdout:   "hello world\n",
		Stderr:   "warning\n",
		ExitCode: 0,
	}
	var buf bytes.Buffer
	if err := Encode(&buf, resp); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Response
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !decoded.OK || decoded.Stdout != "hello world\n" || decoded.Stderr != "warning\n" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestDecodeResponse_WithError(t *testing.T) {
	resp := Response{Error: "command not found"}
	var buf bytes.Buffer
	if err := Encode(&buf, resp); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Response
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Error != "command not found" {
		t.Fatalf("Error = %q", decoded.Error)
	}
}

func TestDecodeResponse_WithMemoryStats(t *testing.T) {
	stats := &MemoryStats{
		FreeMemory:      1000,
		TotalMemory:     4000,
		AvailableMemory: 2000,
		OOMKill:         3,
		SwapIn:          10,
		SwapOut:         20,
	}
	resp := Response{OK: true, MemoryStats: stats}
	var buf bytes.Buffer
	if err := Encode(&buf, resp); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Response
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.MemoryStats == nil {
		t.Fatal("expected MemoryStats")
	}
	if decoded.MemoryStats.FreeMemory != 1000 || decoded.MemoryStats.OOMKill != 3 {
		t.Fatalf("MemoryStats = %+v", decoded.MemoryStats)
	}
}

func TestDecodeResponse_WithMemoryHotplug(t *testing.T) {
	hotplug := &MemoryHotplug{
		BlockSizeBytes: 128 << 20,
		RequestedBytes: 512 << 20,
		PluggedBytes:   256 << 20,
		OnlineBlocks:   2,
		PresentBlocks:  4,
	}
	resp := Response{OK: true, MemoryHotplug: hotplug}
	var buf bytes.Buffer
	if err := Encode(&buf, resp); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Response
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.MemoryHotplug == nil {
		t.Fatal("expected MemoryHotplug")
	}
	if decoded.MemoryHotplug.PluggedBytes != 256<<20 {
		t.Fatalf("PluggedBytes = %d", decoded.MemoryHotplug.PluggedBytes)
	}
}

func TestDecode_InvalidJSON(t *testing.T) {
	var req Request
	err := Decode(strings.NewReader("{not valid json}"), &req)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDecode_EmptyInput(t *testing.T) {
	var req Request
	err := Decode(strings.NewReader(""), &req)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestRequestJSON_RoundTrip(t *testing.T) {
	req := Request{
		Mode:    ModeExec,
		Command: []string{"cat", "/etc/hostname"},
		Env:     []string{"TERM=xterm"},
		WorkDir: "/home",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored Request
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.Mode != ModeExec || len(restored.Command) != 2 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestResponseJSON_RoundTrip(t *testing.T) {
	resp := Response{
		OK:       true,
		Stdout:   "data",
		ExitCode: 42,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored Response
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.ExitCode != 42 || restored.Stdout != "data" {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestMemoryStatsJSON_RoundTrip(t *testing.T) {
	stats := MemoryStats{
		SwapIn:          1,
		SwapOut:         2,
		MajorFaults:     3,
		MinorFaults:     4,
		FreeMemory:      5,
		TotalMemory:     6,
		AvailableMemory: 7,
		DiskCaches:      8,
		OOMKill:         9,
		AllocStall:      10,
		AsyncScan:       11,
		DirectScan:      12,
		AsyncReclaim:    13,
		DirectReclaim:   14,
	}
	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored MemoryStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.SwapIn != 1 || restored.DirectReclaim != 14 || restored.OOMKill != 9 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestMemoryHotplugJSON_RoundTrip(t *testing.T) {
	hp := MemoryHotplug{
		BlockSizeBytes: 128 << 20,
		RequestedBytes: 256 << 20,
		PluggedBytes:   128 << 20,
		OnlineBlocks:   1,
		PresentBlocks:  2,
	}
	data, err := json.Marshal(hp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored MemoryHotplug
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.BlockSizeBytes != 128<<20 || restored.OnlineBlocks != 1 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestConfigJSON_RoundTrip(t *testing.T) {
	cfg := Config{Enabled: true, VsockPort: 5555}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored Config
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !restored.Enabled || restored.VsockPort != 5555 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestModeConstants(t *testing.T) {
	if ModeExec != "exec" {
		t.Fatalf("ModeExec = %q", ModeExec)
	}
	if ModeStream != "stream" {
		t.Fatalf("ModeStream = %q", ModeStream)
	}
	if ModeMemoryStats != "memory_stats" {
		t.Fatalf("ModeMemoryStats = %q", ModeMemoryStats)
	}
	if ModeMemoryHotplugGet != "memory_hotplug_get" {
		t.Fatalf("ModeMemoryHotplugGet = %q", ModeMemoryHotplugGet)
	}
	if ModeMemoryHotplugUpdate != "memory_hotplug_update" {
		t.Fatalf("ModeMemoryHotplugUpdate = %q", ModeMemoryHotplugUpdate)
	}
}

func TestDefaultVsockPort(t *testing.T) {
	if DefaultVsockPort != 10022 {
		t.Fatalf("DefaultVsockPort = %d, want 10022", DefaultVsockPort)
	}
}

func TestEncodeDecodeRoundTrip_NilFields(t *testing.T) {
	// Request with nil Command and Env
	req := Request{Mode: ModeStream}
	var buf bytes.Buffer
	if err := Encode(&buf, req); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Request
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Command != nil {
		t.Fatalf("Command should be nil, got %v", decoded.Command)
	}
}

func TestEncodeDecodeRoundTrip_NonZeroExitCode(t *testing.T) {
	resp := Response{ExitCode: 127, Stderr: "not found"}
	var buf bytes.Buffer
	if err := Encode(&buf, resp); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Response
	if err := Decode(&buf, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.ExitCode != 127 {
		t.Fatalf("ExitCode = %d, want 127", decoded.ExitCode)
	}
}
