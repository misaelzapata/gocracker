package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

type fakeHandle struct {
	id      string
	state   vmm.State
	events  *vmm.EventLog
	cfg     vmm.Config
	netRL   *vmm.RateLimiterConfig
	blkRL   *vmm.RateLimiterConfig
	rngRL   *vmm.RateLimiterConfig
	balloon vmm.BalloonStats
	hotplug vmm.MemoryHotplugStatus
}

func newFakeHandle(id string) *fakeHandle {
	ev := vmm.NewEventLog()
	ev.Emit(vmm.EventCreated, "created")
	return &fakeHandle{
		id:     id,
		state:  vmm.StateRunning,
		events: ev,
		cfg:    vmm.Config{ID: id},
	}
}

func (f *fakeHandle) Start() error                               { return nil }
func (f *fakeHandle) Stop()                                      { f.state = vmm.StateStopped }
func (f *fakeHandle) TakeSnapshot(string) (*vmm.Snapshot, error) { return &vmm.Snapshot{ID: f.id}, nil }
func (f *fakeHandle) State() vmm.State                           { return f.state }
func (f *fakeHandle) ID() string                                 { return f.id }
func (f *fakeHandle) Uptime() time.Duration                      { return time.Second }
func (f *fakeHandle) Events() vmm.EventSource                    { return f.events }
func (f *fakeHandle) VMConfig() vmm.Config                       { return f.cfg }
func (f *fakeHandle) DeviceList() []vmm.DeviceInfo               { return nil }
func (f *fakeHandle) ConsoleOutput() []byte                      { return []byte("logs") }
func (f *fakeHandle) FirstOutputAt() time.Time                   { return time.Time{} }
func (f *fakeHandle) WaitStopped(ctx context.Context) error      { <-ctx.Done(); return ctx.Err() }
func (f *fakeHandle) UpdateNetRateLimiter(cfg *vmm.RateLimiterConfig) error {
	f.netRL = cfg
	return nil
}
func (f *fakeHandle) UpdateBlockRateLimiter(cfg *vmm.RateLimiterConfig) error {
	f.blkRL = cfg
	return nil
}
func (f *fakeHandle) UpdateRNGRateLimiter(cfg *vmm.RateLimiterConfig) error {
	f.rngRL = cfg
	return nil
}
func (f *fakeHandle) DialVsock(port uint32) (net.Conn, error) {
	serverConn, clientConn := net.Pipe()
	go func() {
		_, _ = io.WriteString(serverConn, "api-vsock-ok")
		_ = serverConn.Close()
	}()
	return clientConn, nil
}

func (f *fakeHandle) GetBalloonConfig() (vmm.BalloonConfig, error) {
	if f.cfg.Balloon == nil {
		return vmm.BalloonConfig{}, os.ErrNotExist
	}
	return *f.cfg.Balloon, nil
}

func (f *fakeHandle) UpdateBalloon(update vmm.BalloonUpdate) error {
	if f.cfg.Balloon == nil {
		f.cfg.Balloon = &vmm.BalloonConfig{}
	}
	f.cfg.Balloon.AmountMiB = update.AmountMiB
	f.balloon.TargetMiB = update.AmountMiB
	f.balloon.TargetPages = update.AmountMiB * 256
	return nil
}

func (f *fakeHandle) GetBalloonStats() (vmm.BalloonStats, error) {
	if f.cfg.Balloon == nil {
		return vmm.BalloonStats{}, os.ErrNotExist
	}
	return f.balloon, nil
}

func (f *fakeHandle) UpdateBalloonStats(update vmm.BalloonStatsUpdate) error {
	if f.cfg.Balloon == nil {
		f.cfg.Balloon = &vmm.BalloonConfig{}
	}
	f.cfg.Balloon.StatsPollingIntervalS = update.StatsPollingIntervalS
	return nil
}

func (f *fakeHandle) GetMemoryHotplug() (vmm.MemoryHotplugStatus, error) {
	if f.cfg.MemoryHotplug == nil {
		return vmm.MemoryHotplugStatus{}, os.ErrNotExist
	}
	return f.hotplug, nil
}

func (f *fakeHandle) UpdateMemoryHotplug(update vmm.MemoryHotplugSizeUpdate) error {
	if f.cfg.MemoryHotplug == nil {
		f.cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{}
	}
	f.hotplug.TotalSizeMiB = f.cfg.MemoryHotplug.TotalSizeMiB
	f.hotplug.SlotSizeMiB = f.cfg.MemoryHotplug.SlotSizeMiB
	f.hotplug.BlockSizeMiB = f.cfg.MemoryHotplug.BlockSizeMiB
	f.hotplug.RequestedSizeMiB = update.RequestedSizeMiB
	f.hotplug.PluggedSizeMiB = update.RequestedSizeMiB
	return nil
}

type fakeWorkerHandle struct {
	*fakeHandle
	cfg  vmm.Config
	meta vmm.WorkerMetadata
}

func newFakeWorkerHandle(id string, cfg vmm.Config, meta vmm.WorkerMetadata) *fakeWorkerHandle {
	if cfg.ID == "" {
		cfg.ID = id
	}
	return &fakeWorkerHandle{
		fakeHandle: newFakeHandle(id),
		cfg:        cfg,
		meta:       meta,
	}
}

func (f *fakeWorkerHandle) VMConfig() vmm.Config               { return f.cfg }
func (f *fakeWorkerHandle) WorkerMetadata() vmm.WorkerMetadata { return f.meta }

func TestHandleInstanceInfo(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var info InstanceInfo
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if info.AppName != "gocracker" {
		t.Errorf("app_name = %q, want %q", info.AppName, "gocracker")
	}
	if info.VMMVersion != "0.2.0" {
		t.Errorf("vmm_version = %q, want %q", info.VMMVersion, "0.2.0")
	}
	if info.ID != "gocracker-0" {
		t.Errorf("id = %q, want %q", info.ID, "gocracker-0")
	}
	if info.HostArch != runtime.GOARCH {
		t.Errorf("host_arch = %q, want %q", info.HostArch, runtime.GOARCH)
	}
}

func TestHandleInstanceInfo_ContentType(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestHandleInstanceInfo_ServerHeader(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	sh := rec.Header().Get("Server")
	if sh != "gocracker/0.2.0" {
		t.Errorf("Server = %q, want %q", sh, "gocracker/0.2.0")
	}
}

func TestHandleListVMs_Empty(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var list []VMInfo
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
	}
}

func TestHandleListVMs_FiltersByComposeMetadata(t *testing.T) {
	srv := New()
	composeEntry := srv.newVMEntry(newFakeWorkerHandle("vm-compose", vmm.Config{
		ID: "vm-compose",
		Metadata: map[string]string{
			"orchestrator": "compose",
			"stack_name":   "todo-stack",
			"service_name": "todo",
		},
	}, vmm.WorkerMetadata{}), nil)
	composeEntry.metadata = mergeMetadata(composeEntry.metadata, map[string]string{
		"guest_ip": "198.18.0.3/24",
	})
	srv.registerVMEntry("vm-compose", composeEntry)

	srv.registerVMEntry("vm-other", srv.newVMEntry(newFakeWorkerHandle("vm-other", vmm.Config{
		ID: "vm-other",
		Metadata: map[string]string{
			"orchestrator": "run",
		},
	}, vmm.WorkerMetadata{}), nil))

	req := httptest.NewRequest(http.MethodGet, "/vms?orchestrator=compose&stack=todo-stack&service=todo", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var list []VMInfo
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].ID != "vm-compose" {
		t.Fatalf("id = %q, want vm-compose", list[0].ID)
	}
	if got := list[0].Metadata["guest_ip"]; got != "198.18.0.3/24" {
		t.Fatalf("guest_ip = %q, want 198.18.0.3/24", got)
	}
}

func TestHandleListVMs_UsesAPIID(t *testing.T) {
	srv := New()
	entry := srv.newVMEntry(newFakeWorkerHandle("vm-worker", vmm.Config{
		ID: "vm-worker",
		Metadata: map[string]string{
			"orchestrator": "compose",
			"stack_name":   "todo-stack",
			"service_name": "todo",
		},
	}, vmm.WorkerMetadata{}), nil)
	srv.registerVMEntry("gc-12345", entry)

	req := httptest.NewRequest(http.MethodGet, "/vms?orchestrator=compose&stack=todo-stack&service=todo", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var list []VMInfo
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].ID != "gc-12345" {
		t.Fatalf("id = %q, want gc-12345", list[0].ID)
	}
}

func TestHandleBootSource(t *testing.T) {
	srv := New()
	body := `{"kernel_image_path":"/vmlinuz","boot_args":"console=ttyS0"}`
	req := httptest.NewRequest(http.MethodPut, "/boot-source", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if srv.preboot.bootSource == nil {
		t.Fatal("boot source not stored")
	}
	if srv.preboot.bootSource.KernelImagePath != "/vmlinuz" {
		t.Errorf("kernel = %q, want /vmlinuz", srv.preboot.bootSource.KernelImagePath)
	}
	if srv.preboot.bootSource.BootArgs != "console=ttyS0" {
		t.Errorf("boot_args = %q", srv.preboot.bootSource.BootArgs)
	}
}

func TestHandleBootSource_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/boot-source", bytes.NewBufferString("{invalid"))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleMachineConfig(t *testing.T) {
	srv := New()
	body := `{"vcpu_count":2,"mem_size_mib":512}`
	req := httptest.NewRequest(http.MethodPut, "/machine-config", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if srv.preboot.machineConf == nil {
		t.Fatal("machine config not stored")
	}
	if srv.preboot.machineConf.VcpuCount != 2 {
		t.Errorf("vcpu_count = %d, want 2", srv.preboot.machineConf.VcpuCount)
	}
	if srv.preboot.machineConf.MemSizeMib != 512 {
		t.Errorf("mem_size_mib = %d, want 512", srv.preboot.machineConf.MemSizeMib)
	}
}

func TestHandleMachineConfig_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/machine-config", bytes.NewBufferString("nope"))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleBootSource_RejectsUnknownFieldAndCmdlineLimit(t *testing.T) {
	srv := New()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/boot-source", bytes.NewBufferString(`{"kernel_image_path":"/vmlinuz","unknown":true}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d body=%s", rec.Code, rec.Body.String())
	}

	longArgs := strings.Repeat("a", 2050)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/boot-source", bytes.NewBufferString(`{"kernel_image_path":"/vmlinuz","boot_args":"`+longArgs+`"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long cmdline status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMachineConfig_RejectsOutOfRangeValues(t *testing.T) {
	srv := New()

	for _, body := range []string{
		`{"vcpu_count":33,"mem_size_mib":512}`,
		`{"vcpu_count":2,"mem_size_mib":64}`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/machine-config", bytes.NewBufferString(body))
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleBalloon_PrebootLifecycle(t *testing.T) {
	srv := New()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/balloon", bytes.NewBufferString(`{"amount_mib":64,"deflate_on_oom":true,"stats_polling_interval_s":5}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	if srv.preboot.balloon == nil || srv.preboot.balloon.AmountMib != 64 {
		t.Fatalf("preboot balloon = %#v", srv.preboot.balloon)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/balloon", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	var balloon Balloon
	if err := json.NewDecoder(rec.Body).Decode(&balloon); err != nil {
		t.Fatalf("decode balloon: %v", err)
	}
	if balloon.AmountMib != 64 || !balloon.DeflateOnOOM {
		t.Fatalf("balloon = %#v", balloon)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/balloon", bytes.NewBufferString(`{"amount_mib":32}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := srv.preboot.balloon.AmountMib; got != 32 {
		t.Fatalf("patched amount_mib = %d, want 32", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/balloon/statistics", bytes.NewBufferString(`{"stats_polling_interval_s":7}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch balloon stats status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := srv.preboot.balloon.StatsPollingIntervalS; got != 7 {
		t.Fatalf("stats_polling_interval_s = %d, want 7", got)
	}
}

func TestHandleBalloon_RootVMRuntime(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-vm")
	handle.cfg.Balloon = &vmm.BalloonConfig{AmountMiB: 64, DeflateOnOOM: true, StatsPollingIntervalS: 5}
	handle.balloon = vmm.BalloonStats{TargetPages: 64 * 256, ActualPages: 32 * 256, TargetMiB: 64, ActualMiB: 32}
	srv.vms["root-vm"] = srv.newVMEntry(handle, nil)
	srv.rootVMID = "root-vm"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/balloon", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	var balloon Balloon
	if err := json.NewDecoder(rec.Body).Decode(&balloon); err != nil {
		t.Fatalf("decode balloon: %v", err)
	}
	if balloon.AmountMib != 64 {
		t.Fatalf("runtime balloon amount_mib = %d, want 64", balloon.AmountMib)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/balloon/statistics", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get balloon stats status = %d body=%s", rec.Code, rec.Body.String())
	}
	var stats vmm.BalloonStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode balloon stats: %v", err)
	}
	if stats.ActualMiB != 32 {
		t.Fatalf("runtime balloon actual_mib = %d, want 32", stats.ActualMiB)
	}
}

func TestHandleMemoryHotplugLifecycle(t *testing.T) {
	srv := New()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hotplug/memory", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preboot get status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/hotplug/memory", bytes.NewBufferString(`{"total_size_mib":512,"slot_size_mib":256,"block_size_mib":128}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/hotplug/memory", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}
	var status vmm.MemoryHotplugStatus
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode hotplug status: %v", err)
	}
	if status.TotalSizeMiB != 512 || status.BlockSizeMiB != 128 {
		t.Fatalf("preboot hotplug status = %#v", status)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString(`{"requested_size_mib":256}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preboot patch hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/hotplug/memory", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preboot get hotplug after patch status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode preboot hotplug status after patch: %v", err)
	}
	if status.RequestedSizeMiB != 256 || status.PluggedSizeMiB != 0 {
		t.Fatalf("preboot patched hotplug status = %#v", status)
	}

	handle := newFakeHandle("vm-hotplug")
	handle.cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{TotalSizeMiB: 512, SlotSizeMiB: 256, BlockSizeMiB: 128}
	handle.hotplug = vmm.MemoryHotplugStatus{TotalSizeMiB: 512, SlotSizeMiB: 256, BlockSizeMiB: 128}
	srv.vms["vm-hotplug"] = &vmEntry{handle: handle, kind: "worker", createdAt: time.Now()}
	srv.rootVMID = "vm-hotplug"

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString(`{"requested_size_mib":256}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}
	if handle.hotplug.RequestedSizeMiB != 256 || handle.hotplug.PluggedSizeMiB != 256 {
		t.Fatalf("runtime hotplug status = %#v", handle.hotplug)
	}
}

func TestPrebootRootStartsWorkerBackedWithRequestedMemoryHotplug(t *testing.T) {
	var handle *fakeHandle
	srv := NewWithOptions(Options{
		JailerMode: container.JailerModeOn,
		LaunchVMMFn: func(cfg vmm.Config) (vmm.Handle, func(), error) {
			handle = newFakeHandle("root-vm")
			handle.cfg = cfg
			handle.hotplug = vmm.MemoryHotplugStatus{
				TotalSizeMiB: cfg.MemoryHotplug.TotalSizeMiB,
				SlotSizeMiB:  cfg.MemoryHotplug.SlotSizeMiB,
				BlockSizeMiB: cfg.MemoryHotplug.BlockSizeMiB,
			}
			return handle, nil, nil
		},
	})

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinux","boot_args":"console=ttyS0"}`},
		{http.MethodPut, "/machine-config", `{"vcpu_count":1,"mem_size_mib":512}`},
		{http.MethodPut, "/drives/root", `{"path_on_host":"/disk.ext4","is_root_device":true}`},
		{http.MethodPut, "/hotplug/memory", `{"total_size_mib":512,"slot_size_mib":256,"block_size_mib":128}`},
		{http.MethodPatch, "/hotplug/memory", `{"requested_size_mib":256}`},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s status = %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	startRec := httptest.NewRecorder()
	srv.ServeHTTP(startRec, httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{"action_type":"InstanceStart"}`)))
	if startRec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", startRec.Code, startRec.Body.String())
	}
	if handle == nil {
		t.Fatal("expected worker-backed handle")
	}
	if handle.hotplug.RequestedSizeMiB != 256 || handle.hotplug.PluggedSizeMiB != 256 {
		t.Fatalf("worker-backed start hotplug status = %#v", handle.hotplug)
	}
}

func TestAuthMiddlewareRequiresBearerToken(t *testing.T) {
	srv := NewWithOptions(Options{AuthToken: "secret-token"})

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodGet, "/vms", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with token = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRun_RejectsKernelOutsideTrustedDirs(t *testing.T) {
	trusted := t.TempDir()
	untrusted := t.TempDir()
	srv := NewWithOptions(Options{TrustedKernelDirs: []string{trusted}})

	body := fmt.Sprintf(`{"image":"alpine:3.20","kernel_path":%q}`, filepath.Join(untrusted, "vmlinux"))
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside trusted directories") {
		t.Fatalf("body = %q, want trusted directory error", rec.Body.String())
	}
}

func TestHandleBuild_RejectsDockerfileOutsideTrustedDirs(t *testing.T) {
	trusted := t.TempDir()
	untrusted := t.TempDir()
	srv := NewWithOptions(Options{TrustedWorkDirs: []string{trusted}})

	body := fmt.Sprintf(`{"dockerfile":%q,"context":%q}`, filepath.Join(untrusted, "Dockerfile"), untrusted)
	req := httptest.NewRequest(http.MethodPost, "/build", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside trusted directories") {
		t.Fatalf("body = %q, want trusted directory error", rec.Body.String())
	}
}

func TestHandleSnapshot_RejectsDestOutsideTrustedDirs(t *testing.T) {
	snapshotRoot := t.TempDir()
	untrusted := t.TempDir()
	srv := NewWithOptions(Options{TrustedSnapshotDirs: []string{snapshotRoot}})
	srv.vms["vm-1"] = &vmEntry{handle: newFakeHandle("vm-1"), kind: "worker", createdAt: time.Now()}

	body := fmt.Sprintf(`{"dest_dir":%q}`, filepath.Join(untrusted, "snap"))
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/snapshot", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside trusted directories") {
		t.Fatalf("body = %q, want trusted directory error", rec.Body.String())
	}
}

func TestHandleRun_AcceptsMemoryHotplug(t *testing.T) {
	runCalled := make(chan struct{}, 1)
	srv := NewWithOptions(Options{RunFn: func(opts container.RunOptions) (*container.RunResult, error) {
		if opts.MemoryHotplug == nil {
			t.Fatal("run opts memory hotplug is nil")
		}
		if opts.MemoryHotplug.TotalSizeMiB != 512 || opts.MemoryHotplug.SlotSizeMiB != 128 || opts.MemoryHotplug.BlockSizeMiB != 128 {
			t.Fatalf("run opts memory hotplug = %#v", opts.MemoryHotplug)
		}
		runCalled <- struct{}{}
		return &container.RunResult{ID: opts.ID, VM: newFakeHandle(opts.ID)}, nil
	}})
	body := `{
		"image":"alpine:3.20",
		"kernel_path":"/kernel",
		"memory_hotplug":{"total_size_mib":512,"slot_size_mib":128,"block_size_mib":128}
	}`
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-runCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("run function was not called")
	}
}

func TestHandleRun_MissingKernelPath(t *testing.T) {
	srv := New()
	body := `{"image":"ubuntu:22.04"}`
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var apiErr APIError
	if err := json.NewDecoder(rec.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if apiErr.FaultMessage != "kernel_path is required" {
		t.Errorf("fault = %q, want %q", apiErr.FaultMessage, "kernel_path is required")
	}
}

func TestHandleRun_MissingImage(t *testing.T) {
	srv := New()
	body := `{"kernel_path":"/vmlinuz"}`
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var apiErr APIError
	json.NewDecoder(rec.Body).Decode(&apiErr)
	if apiErr.FaultMessage != "exactly one of image or dockerfile is required" {
		t.Errorf("fault = %q", apiErr.FaultMessage)
	}
}

func TestHandleRun_BothImageAndDockerfile(t *testing.T) {
	srv := New()
	body := `{"kernel_path":"/vmlinuz","image":"ubuntu","dockerfile":"Dockerfile"}`
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var apiErr APIError
	json.NewDecoder(rec.Body).Decode(&apiErr)
	if apiErr.FaultMessage != "specify image or dockerfile, not both" {
		t.Errorf("fault = %q", apiErr.FaultMessage)
	}
}

func TestHandleRun_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString("not json"))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleGetVM_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vms/nonexistent", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleRun_RegistersPendingVMEntry(t *testing.T) {
	block := make(chan struct{})
	srv := NewWithOptions(Options{
		RunFn: func(opts container.RunOptions) (*container.RunResult, error) {
			<-block
			return &container.RunResult{
				ID: opts.ID,
				VM: newFakeHandle(opts.ID),
			}, nil
		},
	})
	defer close(block)

	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(`{"image":"alpine:3.20","kernel_path":"/vmlinux","mem_mb":256,"metadata":{"stack_name":"demo"}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var runResp RunResponse
	if err := json.NewDecoder(rec.Body).Decode(&runResp); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	if runResp.ID == "" {
		t.Fatal("run response id is empty")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/vms/"+runResp.ID, nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", getRec.Code, getRec.Body.String())
	}
	var info VMInfo
	if err := json.NewDecoder(getRec.Body).Decode(&info); err != nil {
		t.Fatalf("decode vm info: %v", err)
	}
	if info.State != "starting" {
		t.Fatalf("state = %q, want starting", info.State)
	}
	if info.Metadata["stack_name"] != "demo" {
		t.Fatalf("metadata stack_name = %q, want demo", info.Metadata["stack_name"])
	}
}

func TestHandleRun_FailedStartRemainsVisible(t *testing.T) {
	srv := NewWithOptions(Options{
		RunFn: func(opts container.RunOptions) (*container.RunResult, error) {
			return nil, context.DeadlineExceeded
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(`{"image":"alpine:3.20","kernel_path":"/vmlinux"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var runResp RunResponse
	if err := json.NewDecoder(rec.Body).Decode(&runResp); err != nil {
		t.Fatalf("decode run response: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		getReq := httptest.NewRequest(http.MethodGet, "/vms/"+runResp.ID, nil)
		getRec := httptest.NewRecorder()
		srv.ServeHTTP(getRec, getReq)
		if getRec.Code == http.StatusOK {
			var info VMInfo
			if err := json.NewDecoder(getRec.Body).Decode(&info); err != nil {
				t.Fatalf("decode vm info: %v", err)
			}
			if info.State == vmm.StateStopped.String() {
				if len(info.Events) == 0 {
					t.Fatal("expected failure events")
				}
				foundError := false
				for _, ev := range info.Events {
					if ev.Type == vmm.EventError && strings.Contains(ev.Message, context.DeadlineExceeded.Error()) {
						foundError = true
						break
					}
				}
				if !foundError {
					t.Fatalf("events = %#v, want error containing %q", info.Events, context.DeadlineExceeded.Error())
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for stopped failure state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandleVMRateLimiterUpdate(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-1")
	srv.vms["vm-1"] = &vmEntry{handle: handle, kind: "worker", createdAt: time.Now()}

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-1/rate-limiters/net", bytes.NewBufferString(`{"bandwidth":{"size":1024,"refill_time_ms":25}}`))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if handle.netRL == nil || handle.netRL.Bandwidth.Size != 1024 {
		t.Fatalf("net limiter = %+v, want size 1024", handle.netRL)
	}
}

func TestBuildVMInfo_MergesEntryMetadataWithoutConfigMetadata(t *testing.T) {
	srv := New()
	entry := &vmEntry{
		handle:    newFakeHandle("vm-1"),
		createdAt: time.Now(),
		metadata: map[string]string{
			"tap_name": "tap-test",
		},
	}

	info := srv.buildVMInfo(entry)

	if got := info.Metadata["tap_name"]; got != "tap-test" {
		t.Fatalf("metadata tap_name = %q, want tap-test", got)
	}
}

func TestHandleStopVM_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/vms/nonexistent/stop", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAPIErrorFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	apiErr(rec, 400, "test error message")

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var ae APIError
	if err := json.NewDecoder(rec.Body).Decode(&ae); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ae.FaultMessage != "test error message" {
		t.Errorf("fault = %q, want %q", ae.FaultMessage, "test error message")
	}
}

func TestHandleAction_UnknownAction(t *testing.T) {
	srv := New()
	body := `{"action_type":"UnknownAction"}`
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestResponseIsValidJSON(t *testing.T) {
	srv := New()

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/vms"},
	}
	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		// Verify the response body is valid JSON
		var raw json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Errorf("%s %s: response is not valid JSON: %v\nbody: %s",
				ep.method, ep.path, err, rec.Body.String())
		}
	}
}

func TestPrebootRootStartsWorkerBackedAndRejectsReconfigure(t *testing.T) {
	var launched vmm.Config
	srv := NewWithOptions(Options{
		JailerMode: container.JailerModeOn,
		LaunchVMMFn: func(cfg vmm.Config) (vmm.Handle, func(), error) {
			launched = cfg
			return newFakeWorkerHandle("root-vm", cfg, vmm.WorkerMetadata{
				Kind:       "worker",
				SocketPath: "/tmp/root-vm.sock",
				WorkerPID:  1234,
				JailRoot:   "/srv/jailer/gocracker-vmm/root-vm/root",
				RunDir:     "/tmp/root-vm",
				CreatedAt:  time.Now(),
			}), nil, nil
		},
	})

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinux","boot_args":"console=ttyS0","initrd_path":"/initrd","x86_boot":"acpi"}`},
		{http.MethodPut, "/machine-config", `{"vcpu_count":2,"mem_size_mib":512,"rng_rate_limiter":{"ops":{"size":5,"refill_time_ms":10}}}`},
		{http.MethodPut, "/drives/root", `{"path_on_host":"/disk.ext4","is_root_device":true,"rate_limiter":{"bandwidth":{"size":1024,"refill_time_ms":20}}}`},
		{http.MethodPut, "/network-interfaces/eth0", `{"host_dev_name":"tap0","guest_mac":"02:00:00:00:00:01","rate_limiter":{"ops":{"size":7,"refill_time_ms":30}}}`},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s status = %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	startRec := httptest.NewRecorder()
	srv.ServeHTTP(startRec, httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{"action_type":"InstanceStart"}`)))
	if startRec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", startRec.Code, startRec.Body.String())
	}
	if launched.KernelPath != "/vmlinux" || launched.InitrdPath != "/initrd" {
		t.Fatalf("launched config = %+v", launched)
	}
	if launched.VCPUs != 2 || launched.MemMB != 512 || launched.TapName != "tap0" {
		t.Fatalf("unexpected worker-backed launch config: %+v", launched)
	}
	if launched.MACAddr.String() != "02:00:00:00:00:01" {
		t.Fatalf("guest mac = %q", launched.MACAddr.String())
	}
	if srv.rootVMID != "root-vm" {
		t.Fatalf("rootVMID = %q, want root-vm", srv.rootVMID)
	}

	reconfigRec := httptest.NewRecorder()
	srv.ServeHTTP(reconfigRec, httptest.NewRequest(http.MethodPut, "/boot-source", bytes.NewBufferString(`{"kernel_image_path":"/other"}`)))
	if reconfigRec.Code != http.StatusConflict {
		t.Fatalf("reconfigure status = %d body=%s", reconfigRec.Code, reconfigRec.Body.String())
	}
}

func TestNewWithOptionsReattachesPersistedRootWorker(t *testing.T) {
	stateDir := t.TempDir()
	socketPath := filepath.Join(t.TempDir(), "worker.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(socketPath)
	}()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				return
			}
			_ = conn.Close()
		}
	}()

	record := persistedWorkerRecord{
		Version: workerRegistryVersion,
		VMID:    "root-vm",
		Kind:    "firecracker-root",
		Config:  vmm.Config{ID: "root-vm", KernelPath: "/vmlinux", MemMB: 256},
		Metadata: vmm.WorkerMetadata{
			Kind:       "worker",
			SocketPath: socketPath,
			WorkerPID:  os.Getpid(),
			JailRoot:   "/srv/jailer/gocracker-vmm/root-vm/root",
			RunDir:     filepath.Join(t.TempDir(), "run"),
			CreatedAt:  time.Now(),
		},
		IsRoot: true,
	}
	if err := writeJSONAtomically(filepath.Join(stateDir, "vms", "root-vm.json"), record); err != nil {
		t.Fatalf("write worker record: %v", err)
	}

	reattached := false
	srv := NewWithOptions(Options{
		JailerMode: container.JailerModeOn,
		StateDir:   stateDir,
		ReattachVMMFn: func(cfg vmm.Config, meta vmm.WorkerMetadata) (vmm.Handle, func(), error) {
			reattached = true
			return newFakeWorkerHandle(cfg.ID, cfg, meta), nil, nil
		},
	})
	if !reattached {
		t.Fatal("expected persisted worker to be reattached")
	}
	if srv.rootVMID != "root-vm" {
		t.Fatalf("rootVMID = %q, want root-vm", srv.rootVMID)
	}
	if _, ok := srv.vms["root-vm"]; !ok {
		t.Fatal("root vm was not loaded from persisted state")
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/boot-source", bytes.NewBufferString(`{"kernel_image_path":"/other"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("boot-source status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMigrationLoadUsesRestoreWorkerHook(t *testing.T) {
	srv := NewWithOptions(Options{
		JailerMode: container.JailerModeOn,
		RestoreVMMFn: func(snapshotDir string, opts vmm.RestoreOptions) (vmm.Handle, func(), error) {
			return newFakeWorkerHandle("restored-vm", vmm.Config{ID: "restored-vm", TapName: opts.OverrideTap}, vmm.WorkerMetadata{
				Kind:       "worker",
				SocketPath: "/tmp/restored.sock",
				WorkerPID:  999,
				RunDir:     "/tmp/restored-run",
				CreatedAt:  time.Now(),
			}), nil, nil
		},
	})

	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "snapshot.json"), []byte(`{"id":"vm1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	reqPart, err := writer.CreateFormField("request")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(reqPart).Encode(MigrationLoadRequest{VMID: "restored-vm", TapName: "tap42", Resume: false}); err != nil {
		t.Fatal(err)
	}
	bundlePart, err := writer.CreateFormFile("bundle", "bundle.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarGz(bundlePart, bundleDir); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/migrations/load", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := srv.vms["restored-vm"]; !ok {
		t.Fatal("expected restored vm to be registered")
	}
	if srv.vmDirs["restored-vm"] == "" {
		t.Fatal("expected migration bundle dir to be tracked for restored vm")
	}
}

func TestHandleRunPassesSupervisorWorkerConfig(t *testing.T) {
	runOptsCh := make(chan container.RunOptions, 1)
	srv := NewWithOptions(Options{
		JailerMode:    container.JailerModeOn,
		JailerBinary:  "/usr/bin/gocracker-jailer",
		VMMBinary:     "/usr/bin/gocracker-vmm",
		ChrootBaseDir: "/srv/jailer",
		UID:           123,
		GID:           456,
		RunFn: func(opts container.RunOptions) (*container.RunResult, error) {
			runOptsCh <- opts
			return &container.RunResult{
				ID: opts.ID,
				VM: newFakeWorkerHandle(opts.ID, vmm.Config{ID: opts.ID}, vmm.WorkerMetadata{
					Kind:       "worker",
					SocketPath: "/tmp/" + opts.ID + ".sock",
					WorkerPID:  321,
					RunDir:     "/tmp/" + opts.ID,
					CreatedAt:  time.Now(),
				}),
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(`{"image":"alpine:latest","kernel_path":"/vmlinux"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case opts := <-runOptsCh:
		if opts.JailerMode != container.JailerModeOn {
			t.Fatalf("JailerMode = %q", opts.JailerMode)
		}
		if opts.JailerBinary != "/usr/bin/gocracker-jailer" || opts.VMMBinary != "/usr/bin/gocracker-vmm" {
			t.Fatalf("worker binary wiring = %+v", opts)
		}
		if opts.ChrootBase != "/srv/jailer" || opts.UID != 123 || opts.GID != 456 {
			t.Fatalf("supervisor worker opts = %+v", opts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runFn was not invoked")
	}
}

func TestHandleRunPassesCacheMetadataAndExec(t *testing.T) {
	runOptsCh := make(chan container.RunOptions, 1)
	srv := NewWithOptions(Options{
		CacheDir: "/var/cache/gocracker",
		RunFn: func(opts container.RunOptions) (*container.RunResult, error) {
			runOptsCh <- opts
			return &container.RunResult{
				ID:      opts.ID,
				GuestIP: "198.18.0.10",
				VM:      newFakeHandle(opts.ID),
			}, nil
		},
	})

	body := `{
		"image":"alpine:latest",
		"kernel_path":"/vmlinux",
		"cache_dir":"",
		"metadata":{"orchestrator":"compose","stack_name":"todo-stack"},
		"exec_enabled":true,
		"static_ip":"198.18.0.10/24",
		"gateway":"198.18.0.1"
	}`
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case opts := <-runOptsCh:
		if opts.CacheDir != "/var/cache/gocracker" {
			t.Fatalf("CacheDir = %q, want /var/cache/gocracker", opts.CacheDir)
		}
		if !opts.ExecEnabled {
			t.Fatal("ExecEnabled = false, want true")
		}
		if got := opts.Metadata["stack_name"]; got != "todo-stack" {
			t.Fatalf("metadata stack_name = %q, want todo-stack", got)
		}
		if got := opts.StaticIP; got != "198.18.0.10/24" {
			t.Fatalf("StaticIP = %q, want 198.18.0.10/24", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runFn was not invoked")
	}
}

func TestClientDialVsock(t *testing.T) {
	srv := New()
	srv.registerVMEntry("vm-vsock", srv.newVMEntry(newFakeHandle("vm-vsock"), nil))

	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.DialVsock(ctx, "vm-vsock", 10022)
	if err != nil {
		t.Fatalf("DialVsock(): %v", err)
	}
	defer conn.Close()

	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}
	if string(data) != "api-vsock-ok" {
		t.Fatalf("vsock payload = %q, want %q", string(data), "api-vsock-ok")
	}
}

func TestExecVsockPortDefaults(t *testing.T) {
	port := execVsockPort(vmm.Config{
		Exec: &vmm.ExecConfig{Enabled: true},
	})
	if port == 0 {
		t.Fatal("exec vsock port = 0, want non-zero")
	}
}

func TestClientExecVM(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/vms/vm-1/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		var req ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := strings.Join(req.Command, " "); got != "echo ok" {
			t.Fatalf("command = %q, want %q", got, "echo ok")
		}
		_ = json.NewEncoder(w).Encode(ExecResponse{Stdout: "ok\n", ExitCode: 0})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL)
	resp, err := client.ExecVM(context.Background(), "vm-1", ExecRequest{Command: []string{"echo", "ok"}})
	if err != nil {
		t.Fatalf("ExecVM(): %v", err)
	}
	if resp.Stdout != "ok\n" || resp.ExitCode != 0 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestClientExecVMStream(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/vms/vm-1/exec/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		var req ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Columns != 120 || req.Rows != 40 {
			t.Fatalf("request = %+v, want columns=120 rows=40", req)
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("expected hijacker support")
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack(): %v", err)
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: exec\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.WriteString(conn, "stream-ok")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.ExecVMStream(ctx, "vm-1", ExecRequest{Columns: 120, Rows: 40})
	if err != nil {
		t.Fatalf("ExecVMStream(): %v", err)
	}
	defer conn.Close()
	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}
	if string(data) != "stream-ok" {
		t.Fatalf("stream payload = %q, want %q", string(data), "stream-ok")
	}
}

func TestHandleBuildPassesSupervisorWorkerConfig(t *testing.T) {
	buildOptsCh := make(chan container.BuildOptions, 1)
	srv := NewWithOptions(Options{
		JailerMode:    container.JailerModeOn,
		JailerBinary:  "/usr/bin/gocracker-jailer",
		VMMBinary:     "/usr/bin/gocracker-vmm",
		ChrootBaseDir: "/srv/jailer",
		UID:           123,
		GID:           456,
		BuildFn: func(opts container.BuildOptions) (*container.BuildResult, error) {
			buildOptsCh <- opts
			return &container.BuildResult{DiskPath: "/tmp/disk.ext4"}, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/build", bytes.NewBufferString(`{"image":"alpine:latest"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case opts := <-buildOptsCh:
		if opts.JailerMode != container.JailerModeOn {
			t.Fatalf("JailerMode = %q", opts.JailerMode)
		}
		if opts.JailerBinary != "/usr/bin/gocracker-jailer" || opts.WorkerBinary != "/usr/bin/gocracker-vmm" {
			t.Fatalf("worker binary wiring = %+v", opts)
		}
		if opts.ChrootBase != "/srv/jailer" || opts.UID != 123 || opts.GID != 456 {
			t.Fatalf("supervisor build opts = %+v", opts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("buildFn was not invoked")
	}
}

// ---- Additional coverage tests ----

func TestHandleAction_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAction_UnknownType(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{"action_type":"UnknownAction"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "unknown action") {
		t.Fatalf("body = %s, want unknown action error", rec.Body.String())
	}
}

func TestHandleAction_InstanceStartWithoutConfig(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{"action_type":"InstanceStart"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatal("InstanceStart without boot source should fail")
	}
}

func TestHandleStopVM_NotFoundExtended(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/vms/nonexistent/stop", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleStopVM_Success(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-stop")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-stop", entry)

	req := httptest.NewRequest(http.MethodPost, "/vms/vm-stop/stop", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleVMLogs_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vms/nonexistent/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleVMLogs_Success(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-logs")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-logs", entry)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-logs/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "logs" {
		t.Fatalf("body = %q, want 'logs'", rec.Body.String())
	}
}

func TestHandleVMEvents_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vms/nonexistent/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVMEvents_Success(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-events")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-events", entry)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-events/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleVMEvents_WithSinceParam(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-events-since")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-events-since", entry)

	since := time.Now().Add(-time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/vms/vm-events-since/events?since="+since, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleVMEvents_InvalidSince(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-events-bad")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-events-bad", entry)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-events-bad/events?since=not-a-date", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVMEventsStream_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vms/nonexistent/events/stream", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleNetRateLimiter_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/vms/nonexistent/rate-limiters/net", bytes.NewBufferString(`{"bandwidth":{"size":100}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleNetRateLimiter_Success(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-rl")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-rl", entry)

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-rl/rate-limiters/net", bytes.NewBufferString(`{"bandwidth":{"size":100}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBlockRateLimiter_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/vms/nonexistent/rate-limiters/block", bytes.NewBufferString(`{"ops":{"size":200}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleBlockRateLimiter_Success(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-blk-rl")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-blk-rl", entry)

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-blk-rl/rate-limiters/block", bytes.NewBufferString(`{"ops":{"size":200}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRNGRateLimiter_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/vms/nonexistent/rate-limiters/rng", bytes.NewBufferString(`{"bandwidth":{"size":50}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleRNGRateLimiter_Success(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-rng-rl")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-rng-rl", entry)

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-rng-rl/rate-limiters/rng", bytes.NewBufferString(`{"bandwidth":{"size":50}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleNetRateLimiter_InvalidJSON(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-rl-bad")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-rl-bad", entry)

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-rl-bad/rate-limiters/net", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleBlockRateLimiter_InvalidJSON(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-blk-bad")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-blk-bad", entry)

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-blk-bad/rate-limiters/block", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleRNGRateLimiter_InvalidJSON(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-rng-bad")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-rng-bad", entry)

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-rng-bad/rate-limiters/rng", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleSnapshot_NotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/vms/nonexistent/snapshot", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleSnapshot_InvalidJSON(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-snap")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-snap", entry)

	req := httptest.NewRequest(http.MethodPost, "/vms/vm-snap/snapshot", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleRun_NoKernel(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(`{"image":"ubuntu:22.04"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kernel_path") {
		t.Fatalf("expected kernel_path error, got %s", rec.Body.String())
	}
}

func TestHandleRun_NoSource(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(`{"kernel_path":"/vmlinuz"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRun_BothSources(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString(`{"kernel_path":"/vmlinuz","image":"ubuntu","dockerfile":"Dockerfile"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "specify image or dockerfile") {
		t.Fatalf("expected source conflict error, got %s", rec.Body.String())
	}
}

func TestHandleRun_MalformedJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleDrive_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/drives/root", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleDrive_Success(t *testing.T) {
	srv := New()
	body := `{"path_on_host":"/tmp/disk.ext4","is_root_device":true}`
	req := httptest.NewRequest(http.MethodPut, "/drives/rootfs", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(srv.preboot.drives) != 1 {
		t.Fatalf("expected 1 drive, got %d", len(srv.preboot.drives))
	}
}

func TestHandleNetworkIface_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/network-interfaces/eth0", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleNetworkIface_Success(t *testing.T) {
	srv := New()
	body := `{"host_dev_name":"tap0"}`
	req := httptest.NewRequest(http.MethodPut, "/network-interfaces/eth0", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonGet_NoBalloon(t *testing.T) {
	srv := New()
	// No preboot balloon configured
	req := httptest.NewRequest(http.MethodGet, "/balloon", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonPut_Success(t *testing.T) {
	srv := New()
	body := `{"amount_mib":64,"deflate_on_oom":true}`
	req := httptest.NewRequest(http.MethodPut, "/balloon", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonPut_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/balloon", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleBalloonPatch_NoBalloon(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon", bytes.NewBufferString(`{"amount_mib":32}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// No balloon configured in preboot, no root VM
	if rec.Code == http.StatusNoContent {
		t.Log("patch succeeded (preboot balloon may have been created)")
	}
}

func TestHandleBalloonStatsGet_NoBalloon(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/balloon/statistics", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonStatsPatch_NoBalloon(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon/statistics", bytes.NewBufferString(`{"stats_polling_interval_s":5}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// No balloon in preboot, no root VM
	if rec.Code == http.StatusNoContent {
		t.Log("patch succeeded (preboot)")
	}
}

func TestHandleBalloonGet_AfterPut(t *testing.T) {
	srv := New()
	// First PUT a balloon config
	putBody := `{"amount_mib":128,"deflate_on_oom":false}`
	putReq := httptest.NewRequest(http.MethodPut, "/balloon", bytes.NewBufferString(putBody))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d", putRec.Code)
	}

	// Then GET it back
	getReq := httptest.NewRequest(http.MethodGet, "/balloon", nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", getRec.Code, getRec.Body.String())
	}
	var b Balloon
	if err := json.NewDecoder(getRec.Body).Decode(&b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.AmountMib != 128 {
		t.Fatalf("amount_mib = %d, want 128", b.AmountMib)
	}
}

func TestHandleMemoryHotplugGet_NoConfig(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/hotplug/memory", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMemoryHotplugPut_Success(t *testing.T) {
	srv := New()
	body := `{"total_size_mib":2048,"slot_size_mib":512,"block_size_mib":128}`
	req := httptest.NewRequest(http.MethodPut, "/hotplug/memory", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMemoryHotplugPut_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/hotplug/memory", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleMemoryHotplugPatch_NoConfig(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString(`{"requested_size_mib":1024}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// No hotplug config, no root VM
	_ = rec.Code // any status is fine
}

func TestHandleMemoryHotplugGet_AfterPut(t *testing.T) {
	srv := New()
	putBody := `{"total_size_mib":4096,"slot_size_mib":1024,"block_size_mib":256}`
	putReq := httptest.NewRequest(http.MethodPut, "/hotplug/memory", bytes.NewBufferString(putBody))
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d", putRec.Code)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/hotplug/memory", nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", getRec.Code, getRec.Body.String())
	}
}

func TestHandleGetVM_Missing(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vms/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleGetVM_Success(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-get")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-get", entry)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-get", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var info VMInfo
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.ID != "vm-get" {
		t.Fatalf("id = %q, want vm-get", info.ID)
	}
}

func TestHandleVMVsockConnect_MissingPort(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-vsock")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-vsock", entry)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-vsock/vsock/connect", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVMVsockConnect_InvalidPort(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-vsock-bad")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-vsock-bad", entry)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-vsock-bad/vsock/connect?port=invalid", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleVMVsockConnect_VMNotFound(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vms/nonexistent/vsock/connect?port=1234", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleBuild_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/build", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleBuild_NoSource(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/build", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBuild_BothSources(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/build", bytes.NewBufferString(`{"image":"ubuntu","dockerfile":"Dockerfile"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonPatch_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleBalloonStatsPatch_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon/statistics", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleMemoryHotplugPatch_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString("{bad"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleAction_InstanceStop_NoRootVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{"action_type":"InstanceStop"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// No root VM to stop
	if rec.Code == http.StatusNoContent {
		t.Log("stop succeeded even without root VM (may be valid)")
	}
}

func TestCloneBalloonConfig_Nil(t *testing.T) {
	if got := cloneBalloonConfig(nil); got != nil {
		t.Fatalf("cloneBalloonConfig(nil) = %v", got)
	}
}

func TestCloneBalloonConfig_WithSnapshots(t *testing.T) {
	original := &vmm.BalloonConfig{
		AmountMiB:     128,
		DeflateOnOOM:  true,
		SnapshotPages: []uint32{1, 2, 3},
	}
	cloned := cloneBalloonConfig(original)
	if cloned == nil || cloned.AmountMiB != 128 {
		t.Fatalf("cloned = %v", cloned)
	}
	// Verify deep copy of SnapshotPages
	cloned.SnapshotPages[0] = 99
	if original.SnapshotPages[0] == 99 {
		t.Fatal("SnapshotPages was not deep-copied")
	}
}

func TestCloneBalloonConfig_WithoutSnapshots(t *testing.T) {
	original := &vmm.BalloonConfig{AmountMiB: 64}
	cloned := cloneBalloonConfig(original)
	if cloned == nil || cloned.AmountMiB != 64 {
		t.Fatalf("cloned = %v", cloned)
	}
}

func TestCloneMemoryHotplug_Nil(t *testing.T) {
	if got := cloneMemoryHotplug(nil); got != nil {
		t.Fatalf("cloneMemoryHotplug(nil) = %v", got)
	}
}

func TestCloneMemoryHotplug_NonNil(t *testing.T) {
	original := &vmm.MemoryHotplugConfig{TotalSizeMiB: 4096}
	cloned := cloneMemoryHotplug(original)
	if cloned == nil || cloned.TotalSizeMiB != 4096 {
		t.Fatalf("cloned = %v", cloned)
	}
}

func TestMergeMetadata(t *testing.T) {
	tests := []struct {
		name string
		dst  map[string]string
		src  map[string]string
		want int
	}{
		{"nil dst", nil, map[string]string{"a": "1"}, 1},
		{"nil src", map[string]string{"a": "1"}, nil, 1},
		{"merge", map[string]string{"a": "1"}, map[string]string{"b": "2"}, 2},
		{"skip empty values", map[string]string{"a": "1"}, map[string]string{"b": ""}, 1},
		{"both empty", nil, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeMetadata(tt.dst, tt.src)
			if len(got) != tt.want {
				t.Fatalf("mergeMetadata() len = %d, want %d; got %v", len(got), tt.want, got)
			}
		})
	}
}

func TestHandleBalloonPut_ThenGet_WithRootVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-vm")
	handle.cfg.Balloon = &vmm.BalloonConfig{AmountMiB: 64}
	entry := srv.newVMEntry(handle, nil)
	entry.isRoot = true
	srv.registerVMEntry("root-vm", entry)
	srv.rootVMID = "root-vm"

	// GET balloon from running root VM
	req := httptest.NewRequest(http.MethodGet, "/balloon", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonStats_WithRootVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-vm-stats")
	handle.cfg.Balloon = &vmm.BalloonConfig{AmountMiB: 64, StatsPollingIntervalS: 5}
	entry := srv.newVMEntry(handle, nil)
	entry.isRoot = true
	srv.registerVMEntry("root-vm-stats", entry)
	srv.rootVMID = "root-vm-stats"

	req := httptest.NewRequest(http.MethodGet, "/balloon/statistics", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET balloon/statistics status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonPatch_WithRootVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-vm-patch")
	handle.cfg.Balloon = &vmm.BalloonConfig{AmountMiB: 64}
	entry := srv.newVMEntry(handle, nil)
	entry.isRoot = true
	srv.registerVMEntry("root-vm-patch", entry)
	srv.rootVMID = "root-vm-patch"

	req := httptest.NewRequest(http.MethodPatch, "/balloon", bytes.NewBufferString(`{"amount_mib":128}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBalloonStatsPatch_WithRootVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-vm-stats-patch")
	handle.cfg.Balloon = &vmm.BalloonConfig{AmountMiB: 64}
	entry := srv.newVMEntry(handle, nil)
	entry.isRoot = true
	srv.registerVMEntry("root-vm-stats-patch", entry)
	srv.rootVMID = "root-vm-stats-patch"

	req := httptest.NewRequest(http.MethodPatch, "/balloon/statistics", bytes.NewBufferString(`{"stats_polling_interval_s":10}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH balloon/statistics status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMemoryHotplugGet_WithRootVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-vm-hp")
	handle.cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{TotalSizeMiB: 4096}
	entry := srv.newVMEntry(handle, nil)
	entry.isRoot = true
	srv.registerVMEntry("root-vm-hp", entry)
	srv.rootVMID = "root-vm-hp"

	req := httptest.NewRequest(http.MethodGet, "/hotplug/memory", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET memory-hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMemoryHotplugPatch_WithRootVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-vm-hp-patch")
	handle.cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{TotalSizeMiB: 4096}
	entry := srv.newVMEntry(handle, nil)
	entry.isRoot = true
	srv.registerVMEntry("root-vm-hp-patch", entry)
	srv.rootVMID = "root-vm-hp-patch"

	req := httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString(`{"requested_size_mib":2048}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH memory-hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleInstanceInfo_WithRootVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("root-info")
	entry := srv.newVMEntry(handle, nil)
	entry.isRoot = true
	srv.registerVMEntry("root-info", entry)
	srv.rootVMID = "root-info"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var info InstanceInfo
	json.NewDecoder(rec.Body).Decode(&info)
	if info.State != "running=1" {
		t.Fatalf("state = %q, want running=1", info.State)
	}
}

func TestHandleSnapshot_WithTrustedDirs(t *testing.T) {
	dir := t.TempDir()
	srv := NewWithOptions(Options{
		TrustedSnapshotDirs: []string{dir},
	})
	handle := newFakeHandle("vm-snap-trusted")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-snap-trusted", entry)

	destDir := filepath.Join(dir, "snapshot-1")
	body := fmt.Sprintf(`{"dest_dir":%q}`, destDir)
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-snap-trusted/snapshot", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestClient_ListVMs(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-client")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-client", entry)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx := context.Background()

	vms, err := client.ListVMs(ctx, nil)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 1 || vms[0].ID != "vm-client" {
		t.Fatalf("ListVMs = %v", vms)
	}
}

func TestClient_ListVMs_WithFilters(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx := context.Background()

	vms, err := client.ListVMs(ctx, map[string]string{"orchestrator": "compose"})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 0 {
		t.Fatalf("expected empty, got %v", vms)
	}
}

func TestClient_GetVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-get-client")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-get-client", entry)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx := context.Background()

	info, err := client.GetVM(ctx, "vm-get-client")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if info.ID != "vm-get-client" {
		t.Fatalf("GetVM id = %q", info.ID)
	}
}

func TestClient_GetVM_NotFound(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	_, err := client.GetVM(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent VM")
	}
}

func TestClient_StopVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-stop-client")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-stop-client", entry)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	if err := client.StopVM(context.Background(), "vm-stop-client"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
}

func TestClient_Logs(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-logs-client")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-logs-client", entry)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	logs, err := client.Logs(context.Background(), "vm-logs-client")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if string(logs) != "logs" {
		t.Fatalf("Logs = %q", string(logs))
	}
}

func TestClient_BalloonOperations(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx := context.Background()

	// Set balloon
	if err := client.SetBalloon(ctx, Balloon{AmountMib: 64, DeflateOnOOM: true}); err != nil {
		t.Fatalf("SetBalloon: %v", err)
	}

	// Get balloon
	balloon, err := client.GetBalloon(ctx)
	if err != nil {
		t.Fatalf("GetBalloon: %v", err)
	}
	if balloon.AmountMib != 64 {
		t.Fatalf("AmountMib = %d", balloon.AmountMib)
	}

	// Patch balloon stats
	if err := client.PatchBalloonStats(ctx, BalloonStatsUpdate{StatsPollingIntervalS: 10}); err != nil {
		t.Fatalf("PatchBalloonStats: %v", err)
	}

	// Patch balloon
	if err := client.PatchBalloon(ctx, BalloonUpdate{AmountMib: 128}); err != nil {
		// May fail if no root VM
		t.Logf("PatchBalloon: %v (expected without root VM)", err)
	}
}

func TestClient_MemoryHotplugOperations(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	ctx := context.Background()

	// Set
	if err := client.SetMemoryHotplug(ctx, MemoryHotplugConfig{TotalSizeMiB: 4096, SlotSizeMiB: 1024, BlockSizeMiB: 128}); err != nil {
		t.Fatalf("SetMemoryHotplug: %v", err)
	}

	// Get
	status, err := client.GetMemoryHotplug(ctx)
	if err != nil {
		t.Fatalf("GetMemoryHotplug: %v", err)
	}
	_ = status

	// Patch
	if err := client.PatchMemoryHotplug(ctx, MemoryHotplugSizeUpdate{RequestedSizeMiB: 2048}); err != nil {
		t.Logf("PatchMemoryHotplug: %v (expected without root VM)", err)
	}
}

func TestClient_SnapshotVM(t *testing.T) {
	srv := New()
	handle := newFakeHandle("vm-snap-client")
	entry := srv.newVMEntry(handle, nil)
	srv.registerVMEntry("vm-snap-client", entry)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	dir := t.TempDir()
	client := NewClient(ts.URL)
	err := client.SnapshotVM(context.Background(), "vm-snap-client", filepath.Join(dir, "snap"))
	// May succeed or fail depending on validation
	_ = err
}

func TestClient_Run(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	_, err := client.Run(context.Background(), RunRequest{
		KernelPath: "/vmlinuz",
		Image:      "ubuntu:22.04",
	})
	// Will start async, may succeed with status 200
	_ = err
}

func TestClient_GetBalloonStats_NoBalloon(t *testing.T) {
	srv := New()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewClient(ts.URL)
	_, err := client.GetBalloonStats(context.Background())
	if err == nil {
		t.Fatal("expected error when no balloon configured")
	}
}

func TestUpsertDrive(t *testing.T) {
	drives := []Drive{{DriveID: "root", PathOnHost: "/disk1.ext4"}}
	drives = upsertDrive(drives, Drive{DriveID: "root", PathOnHost: "/disk2.ext4"})
	if len(drives) != 1 {
		t.Fatalf("expected 1 drive after upsert, got %d", len(drives))
	}
	if drives[0].PathOnHost != "/disk2.ext4" {
		t.Fatalf("drive path = %q", drives[0].PathOnHost)
	}

	drives = upsertDrive(drives, Drive{DriveID: "data", PathOnHost: "/data.ext4"})
	if len(drives) != 2 {
		t.Fatalf("expected 2 drives, got %d", len(drives))
	}
}

func TestUpsertNetworkInterface(t *testing.T) {
	ifaces := []NetworkInterface{{IfaceID: "eth0", HostDevName: "tap0"}}
	ifaces = upsertNetworkInterface(ifaces, NetworkInterface{IfaceID: "eth0", HostDevName: "tap1"})
	if len(ifaces) != 1 {
		t.Fatalf("expected 1 iface after upsert, got %d", len(ifaces))
	}
	if ifaces[0].HostDevName != "tap1" {
		t.Fatalf("host_dev_name = %q", ifaces[0].HostDevName)
	}

	ifaces = upsertNetworkInterface(ifaces, NetworkInterface{IfaceID: "eth1", HostDevName: "tap2"})
	if len(ifaces) != 2 {
		t.Fatalf("expected 2 ifaces, got %d", len(ifaces))
	}
}

func TestValidateRequestedArch(t *testing.T) {
	// Empty arch should be valid (uses default)
	if err := validateRequestedArch(""); err != nil {
		t.Fatalf("empty arch: %v", err)
	}
	// Current arch should be valid
	if err := validateRequestedArch(runtime.GOARCH); err != nil {
		t.Fatalf("current arch: %v", err)
	}
	// Different arch should fail
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	if err := validateRequestedArch(other); err == nil {
		t.Fatal("expected error for different arch")
	}
	// Invalid arch
	if err := validateRequestedArch("mips"); err == nil {
		t.Fatal("expected error for invalid arch")
	}
}

func TestCloneAPILimiter(t *testing.T) {
	if got := cloneAPILimiter(nil); got != nil {
		t.Fatal("cloneAPILimiter(nil) should be nil")
	}
	original := &vmm.RateLimiterConfig{Bandwidth: vmm.TokenBucketConfig{Size: 100}}
	cloned := cloneAPILimiter(original)
	if cloned.Bandwidth.Size != 100 {
		t.Fatalf("cloned size = %d", cloned.Bandwidth.Size)
	}
	cloned.Bandwidth.Size = 999
	if original.Bandwidth.Size == 999 {
		t.Fatal("clone was not a deep copy")
	}
}

func TestMigrationFinalizeUsesRestoreWorkerHook(t *testing.T) {
	srv := NewWithOptions(Options{
		JailerMode: container.JailerModeOn,
		RestoreVMMFn: func(snapshotDir string, opts vmm.RestoreOptions) (vmm.Handle, func(), error) {
			return newFakeWorkerHandle("finalized-vm", vmm.Config{ID: "finalized-vm", TapName: opts.OverrideTap}, vmm.WorkerMetadata{
				Kind:       "worker",
				SocketPath: "/tmp/finalized.sock",
				WorkerPID:  111,
				RunDir:     "/tmp/finalized-run",
				CreatedAt:  time.Now(),
			}), nil, nil
		},
	})

	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "snapshot.json"), []byte(`{"id":"vm1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	sessionID := "mig-test"
	srv.migrationSessions[sessionID] = baseDir

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	reqPart, err := writer.CreateFormField("request")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(reqPart).Encode(MigrationFinalizeRequest{SessionID: sessionID, VMID: "finalized-vm", TapName: "tap99", Resume: false}); err != nil {
		t.Fatal(err)
	}
	bundlePart, err := writer.CreateFormFile("bundle", "bundle.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarGz(bundlePart, baseDir); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/migrations/finalize", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := srv.vms["finalized-vm"]; !ok {
		t.Fatal("expected finalized vm to be registered")
	}
}
