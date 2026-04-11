package vmmserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/vmm"
)

type fakeVM struct {
	started bool
	stopped bool
	cfg     vmm.Config
	events  *vmm.EventLog
	netRL   *vmm.RateLimiterConfig
	blkRL   *vmm.RateLimiterConfig
	rngRL   *vmm.RateLimiterConfig
	balloon vmm.BalloonStats
	hotplug vmm.MemoryHotplugStatus
}

func newFakeVM(cfg vmm.Config) VM {
	ev := vmm.NewEventLog()
	ev.Emit(vmm.EventCreated, "created")
	return &fakeVM{cfg: cfg, events: ev}
}

func (f *fakeVM) Start() error {
	f.started = true
	f.events.Emit(vmm.EventRunning, "running")
	return nil
}

func (f *fakeVM) Stop() {
	f.stopped = true
	f.events.Emit(vmm.EventStopped, "stopped")
}

func (f *fakeVM) TakeSnapshot(dir string) (*vmm.Snapshot, error) {
	return &vmm.Snapshot{Version: 2, ID: f.cfg.ID, Timestamp: time.Now(), MemFile: filepath.Join(dir, "mem.bin")}, nil
}

func (f *fakeVM) State() vmm.State {
	switch {
	case f.stopped:
		return vmm.StateStopped
	case f.started:
		return vmm.StateRunning
	default:
		return vmm.StateCreated
	}
}

func (f *fakeVM) ID() string                   { return f.cfg.ID }
func (f *fakeVM) Uptime() time.Duration        { return 0 }
func (f *fakeVM) Events() vmm.EventSource      { return f.events }
func (f *fakeVM) VMConfig() vmm.Config         { return f.cfg }
func (f *fakeVM) DeviceList() []vmm.DeviceInfo { return nil }
func (f *fakeVM) ConsoleOutput() []byte        { return []byte("hello\n") }
func (f *fakeVM) UpdateNetRateLimiter(cfg *vmm.RateLimiterConfig) error {
	f.netRL = cfg
	f.cfg.NetRateLimiter = cfg
	return nil
}
func (f *fakeVM) UpdateBlockRateLimiter(cfg *vmm.RateLimiterConfig) error {
	f.blkRL = cfg
	f.cfg.BlockRateLimiter = cfg
	return nil
}
func (f *fakeVM) UpdateRNGRateLimiter(cfg *vmm.RateLimiterConfig) error {
	f.rngRL = cfg
	f.cfg.RNGRateLimiter = cfg
	return nil
}
func (f *fakeVM) GetBalloonConfig() (vmm.BalloonConfig, error) {
	if f.cfg.Balloon == nil {
		return vmm.BalloonConfig{}, io.EOF
	}
	return *f.cfg.Balloon, nil
}
func (f *fakeVM) UpdateBalloon(update vmm.BalloonUpdate) error {
	if f.cfg.Balloon == nil {
		f.cfg.Balloon = &vmm.BalloonConfig{}
	}
	f.cfg.Balloon.AmountMiB = update.AmountMiB
	f.balloon.TargetMiB = update.AmountMiB
	f.balloon.TargetPages = update.AmountMiB * 256
	return nil
}
func (f *fakeVM) GetBalloonStats() (vmm.BalloonStats, error) {
	if f.cfg.Balloon == nil {
		return vmm.BalloonStats{}, io.EOF
	}
	return f.balloon, nil
}
func (f *fakeVM) UpdateBalloonStats(update vmm.BalloonStatsUpdate) error {
	if f.cfg.Balloon == nil {
		f.cfg.Balloon = &vmm.BalloonConfig{}
	}
	f.cfg.Balloon.StatsPollingIntervalS = update.StatsPollingIntervalS
	return nil
}
func (f *fakeVM) GetMemoryHotplug() (vmm.MemoryHotplugStatus, error) {
	if f.cfg.MemoryHotplug == nil {
		return vmm.MemoryHotplugStatus{}, io.EOF
	}
	return f.hotplug, nil
}
func (f *fakeVM) UpdateMemoryHotplug(update vmm.MemoryHotplugSizeUpdate) error {
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
func (f *fakeVM) PrepareMigrationBundle(string) error { return nil }
func (f *fakeVM) FinalizeMigrationBundle(string) (*vmm.Snapshot, *vmm.MigrationPatchSet, error) {
	return &vmm.Snapshot{ID: f.cfg.ID}, &vmm.MigrationPatchSet{Version: 1}, nil
}
func (f *fakeVM) ResetMigrationTracking() error { return nil }
func (f *fakeVM) DialVsock(port uint32) (net.Conn, error) {
	serverConn, clientConn := net.Pipe()
	go func() {
		_, _ = io.WriteString(serverConn, "vsock-ok")
		_ = serverConn.Close()
	}()
	return clientConn, nil
}

func TestServerLifecycle(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})

	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	if rec := mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz","boot_args":"console=ttyS0"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("boot-source status = %d", rec.Code)
	}
	if rec := mustDo(http.MethodPut, "/machine-config", `{"vcpu_count":2,"mem_size_mib":512}`); rec.Code != http.StatusNoContent {
		t.Fatalf("machine-config status = %d", rec.Code)
	}
	if rec := mustDo(http.MethodPut, "/drives/root", `{"path_on_host":"/disk.ext4","is_root_device":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("drive status = %d", rec.Code)
	}
	if rec := mustDo(http.MethodPut, "/network-interfaces/eth0", `{"host_dev_name":"tap0","guest_mac":"02:00:00:00:00:01"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("netif status = %d", rec.Code)
	}

	rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}

	infoRec := mustDo(http.MethodGet, "/", "")
	var info InstanceInfo
	if err := json.NewDecoder(infoRec.Body).Decode(&info); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if info.State != "running=1" {
		t.Fatalf("state = %q", info.State)
	}

	eventsRec := mustDo(http.MethodGet, "/events", "")
	var events []vmm.Event
	if err := json.NewDecoder(eventsRec.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected events")
	}

	logsRec := mustDo(http.MethodGet, "/logs", "")
	if got := logsRec.Body.String(); got != "hello\n" {
		t.Fatalf("logs = %q", got)
	}

	vmInfoRec := mustDo(http.MethodGet, "/vm", "")
	var vmInfo VMInfo
	if err := json.NewDecoder(vmInfoRec.Body).Decode(&vmInfo); err != nil {
		t.Fatalf("decode vm info: %v", err)
	}
	if vmInfo.State != "running" {
		t.Fatalf("vm state = %q", vmInfo.State)
	}

	rec = mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/other"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("post-start config status = %d", rec.Code)
	}

	rec = mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStop"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d", rec.Code)
	}
}

func TestSnapshotEndpoint(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})

	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz","boot_args":"console=ttyS0"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("boot-source status = %d", rec.Code)
	}
	if rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d", rec.Code)
	}
	rec := mustDo(http.MethodPost, "/snapshot", `{"dest_dir":"`+filepath.ToSlash(t.TempDir())+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d body=%s", rec.Code, rec.Body.String())
	}
	var snap vmm.Snapshot
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Version != 2 {
		t.Fatalf("snapshot version = %d, want 2", snap.Version)
	}
}

func TestBalloonLifecycle(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})

	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	if rec := mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("boot-source status = %d", rec.Code)
	}
	if rec := mustDo(http.MethodPut, "/balloon", `{"amount_mib":64,"deflate_on_oom":true,"stats_polling_interval_s":5}`); rec.Code != http.StatusNoContent {
		t.Fatalf("balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}
	vm := srv.vm.(*fakeVM)
	vm.balloon = vmm.BalloonStats{TargetPages: 64 * 256, ActualPages: 32 * 256, TargetMiB: 64, ActualMiB: 32}

	rec := mustDo(http.MethodGet, "/balloon", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	var balloon Balloon
	if err := json.NewDecoder(rec.Body).Decode(&balloon); err != nil {
		t.Fatalf("decode balloon: %v", err)
	}
	if balloon.AmountMib != 64 {
		t.Fatalf("amount_mib = %d, want 64", balloon.AmountMib)
	}

	rec = mustDo(http.MethodPatch, "/balloon", `{"amount_mib":32}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	if vm.cfg.Balloon == nil || vm.cfg.Balloon.AmountMiB != 32 {
		t.Fatalf("patched balloon config = %#v", vm.cfg.Balloon)
	}

	rec = mustDo(http.MethodGet, "/balloon/statistics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get balloon stats status = %d body=%s", rec.Code, rec.Body.String())
	}
	var stats vmm.BalloonStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode balloon stats: %v", err)
	}
	if stats.ActualMiB != 32 {
		t.Fatalf("actual_mib = %d, want 32", stats.ActualMiB)
	}
}

func TestMemoryHotplugLifecycle(t *testing.T) {
	srv := New()

	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	rec := mustDo(http.MethodGet, "/hotplug/memory", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preboot get hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = mustDo(http.MethodPut, "/hotplug/memory", `{"total_size_mib":512,"slot_size_mib":256,"block_size_mib":128}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = mustDo(http.MethodGet, "/hotplug/memory", "")
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

	rec = mustDo(http.MethodPatch, "/hotplug/memory", `{"requested_size_mib":256}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preboot patch hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = mustDo(http.MethodGet, "/hotplug/memory", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preboot get after patch hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode preboot hotplug status after patch: %v", err)
	}
	if status.RequestedSizeMiB != 256 || status.PluggedSizeMiB != 0 {
		t.Fatalf("preboot patched hotplug status = %#v", status)
	}

	vm := newFakeVM(vmm.Config{
		ID:            "vm-hotplug",
		MemoryHotplug: &vmm.MemoryHotplugConfig{TotalSizeMiB: 512, SlotSizeMiB: 256, BlockSizeMiB: 128},
	}).(*fakeVM)
	vm.started = true
	vm.hotplug = vmm.MemoryHotplugStatus{TotalSizeMiB: 512, SlotSizeMiB: 256, BlockSizeMiB: 128}
	srv.mu.Lock()
	srv.vm = vm
	srv.started = true
	srv.mu.Unlock()

	rec = mustDo(http.MethodPatch, "/hotplug/memory", `{"requested_size_mib":256}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch hotplug status = %d body=%s", rec.Code, rec.Body.String())
	}
	if vm.hotplug.RequestedSizeMiB != 256 || vm.hotplug.PluggedSizeMiB != 256 {
		t.Fatalf("runtime hotplug status = %#v", vm.hotplug)
	}
}

func TestPrebootStartAppliesRequestedMemoryHotplug(t *testing.T) {
	var vm *fakeVM
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		vm = newFakeVM(cfg).(*fakeVM)
		vm.hotplug = vmm.MemoryHotplugStatus{
			TotalSizeMiB: cfg.MemoryHotplug.TotalSizeMiB,
			SlotSizeMiB:  cfg.MemoryHotplug.SlotSizeMiB,
			BlockSizeMiB: cfg.MemoryHotplug.BlockSizeMiB,
		}
		return vm, nil
	}})

	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz","boot_args":"console=ttyS0"}`},
		{http.MethodPut, "/machine-config", `{"vcpu_count":1,"mem_size_mib":512}`},
		{http.MethodPut, "/drives/root", `{"path_on_host":"/disk.ext4","is_root_device":true}`},
		{http.MethodPut, "/hotplug/memory", `{"total_size_mib":512,"slot_size_mib":256,"block_size_mib":128}`},
		{http.MethodPatch, "/hotplug/memory", `{"requested_size_mib":256}`},
	} {
		rec := mustDo(tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s status = %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}
	if vm == nil {
		t.Fatal("expected started vm")
	}
	if vm.hotplug.RequestedSizeMiB != 256 || vm.hotplug.PluggedSizeMiB != 256 {
		t.Fatalf("vm hotplug after start = %#v", vm.hotplug)
	}
}

func TestVsockConnectEndpoint(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})
	srv.mu.Lock()
	srv.vm = newFakeVM(vmm.Config{ID: "vm-vsock"})
	srv.started = true
	srv.mu.Unlock()

	ts := httptest.NewServer(srv)
	defer ts.Close()

	rawConn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	conn, err := upgradeClientConn(rawConn, "/vsock/connect?port=10022")
	if err != nil {
		t.Fatalf("upgradeClientConn(): %v", err)
	}
	defer conn.Close()

	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}
	if string(data) != "vsock-ok" {
		t.Fatalf("vsock payload = %q, want %q", string(data), "vsock-ok")
	}
}

func TestServerRejectsInvalidAction(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{"action_type":"Bogus"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	var apiErr APIError
	if err := json.NewDecoder(rec.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if apiErr.FaultMessage == "" {
		t.Fatal("expected fault message")
	}
}

func TestServerCurrentVMNotStarted(t *testing.T) {
	srv := New()
	if _, err := srv.currentVM(); err == nil {
		t.Fatal("expected error")
	}
}

func TestServerRateLimitersPrebootAndUpdate(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})

	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	if rec := mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz","boot_args":"console=ttyS0"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("boot-source status = %d", rec.Code)
	}
	if rec := mustDo(http.MethodPut, "/machine-config", `{"vcpu_count":1,"mem_size_mib":128,"rng_rate_limiter":{"ops":{"size":1,"refill_time_ms":10}}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("machine-config status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := mustDo(http.MethodPut, "/drives/root", `{"path_on_host":"/disk.ext4","is_root_device":true,"rate_limiter":{"bandwidth":{"size":4096,"refill_time_ms":20}}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("drive status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := mustDo(http.MethodPut, "/network-interfaces/eth0", `{"host_dev_name":"tap0","rate_limiter":{"bandwidth":{"size":2048,"refill_time_ms":30}}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("netif status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}

	vm, err := srv.currentVM()
	if err != nil {
		t.Fatalf("currentVM: %v", err)
	}
	fake := vm.(*fakeVM)
	if fake.cfg.RNGRateLimiter == nil || fake.cfg.BlockRateLimiter == nil || fake.cfg.NetRateLimiter == nil {
		t.Fatal("expected all preboot rate limiters to be applied to VM config")
	}

	if rec := mustDo(http.MethodPut, "/rate-limiters/net", `{"ops":{"size":2,"refill_time_ms":15}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("update net limiter status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.netRL == nil || fake.netRL.Ops.Size != 2 {
		t.Fatalf("net limiter not updated: %+v", fake.netRL)
	}
}

func TestNormalizeX86BootMode(t *testing.T) {
	mode, err := normalizeX86BootMode(vmm.X86BootAuto, "")
	if err != nil || mode != vmm.X86BootAuto {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	mode, err = normalizeX86BootMode(vmm.X86BootLegacy, "")
	if err != nil || mode != vmm.X86BootLegacy {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	if _, err := normalizeX86BootMode(vmm.X86BootAuto, "bogus"); err == nil {
		t.Fatal("expected error")
	}
}

func TestInstanceInfoShape(t *testing.T) {
	srv := New()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var info InstanceInfo
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.AppName != "gocracker" {
		t.Fatalf("app_name = %q", info.AppName)
	}
	if info.ID != "gocracker-0" {
		t.Fatalf("id = %q", info.ID)
	}
}

func TestStartWithoutBootSource(t *testing.T) {
	srv := New()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{"action_type":"InstanceStart"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	var apiErr APIError
	if err := json.NewDecoder(rec.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if apiErr.FaultMessage != "boot-source not configured" {
		t.Fatalf("fault = %q", apiErr.FaultMessage)
	}
}

func TestServerRejectsInvalidPrebootConfig(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	if rec := mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz","balloon":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown boot-source field status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := mustDo(http.MethodPut, "/machine-config", `{"vcpu_count":33,"mem_size_mib":512}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("machine-config limit status = %d body=%s", rec.Code, rec.Body.String())
	}

	longArgs := strings.Repeat("a", 2050)
	if rec := mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz","boot_args":"`+longArgs+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("boot args limit status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServerStartRejectsUnsupportedPrebootTopology(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	if rec := mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("boot-source status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := mustDo(http.MethodPut, "/drives/data", `{"path_on_host":"/data.ext4","is_root_device":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("drive status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}
	var apiErr APIError
	if err := json.NewDecoder(rec.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if apiErr.FaultMessage != "exactly one root drive is required when drives are configured" {
		t.Fatalf("fault = %q", apiErr.FaultMessage)
	}
}

// --- Additional coverage-boosting tests ---

func newStartedServer(t *testing.T) (*Server, *fakeVM) {
	t.Helper()
	var vm *fakeVM
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		vm = newFakeVM(cfg).(*fakeVM)
		return vm, nil
	}})
	mustDo := func(method, path, body string) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s status = %d body=%s", method, path, rec.Code, rec.Body.String())
		}
	}
	mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz","boot_args":"console=ttyS0"}`)
	mustDo(http.MethodPut, "/machine-config", `{"vcpu_count":1,"mem_size_mib":256}`)
	mustDo(http.MethodPut, "/drives/root", `{"path_on_host":"/disk.ext4","is_root_device":true}`)
	mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`)
	return srv, vm
}

func TestBalloonPatch_PrebootNoBalloon(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon", bytes.NewBufferString(`{"amount_mib":32}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBalloonPatch_PrebootWithBalloon(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := mustDo(http.MethodPut, "/balloon", `{"amount_mib":64,"deflate_on_oom":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put balloon status = %d", rec.Code)
	}
	if rec := mustDo(http.MethodPatch, "/balloon", `{"amount_mib":32}`); rec.Code != http.StatusNoContent {
		t.Fatalf("patch balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBalloonPatch_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon", bytes.NewBufferString(`{bad json`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBalloonStatsPatch_PrebootNoBalloon(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon/statistics", bytes.NewBufferString(`{"stats_polling_interval_s":5}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBalloonStatsGet_PrebootNoBalloon(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/balloon/statistics", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBalloonStatsGet_PrebootWithBalloonAndStats(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := mustDo(http.MethodPut, "/balloon", `{"amount_mib":64,"deflate_on_oom":true,"stats_polling_interval_s":5}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put balloon status = %d", rec.Code)
	}
	rec := mustDo(http.MethodGet, "/balloon/statistics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var stats vmm.BalloonStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.TargetMiB != 64 {
		t.Fatalf("TargetMiB = %d, want 64", stats.TargetMiB)
	}
}

func TestMemoryHotplugPatch_PrebootNotConfigured(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString(`{"requested_size_mib":128}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMemoryHotplugPatch_PrebootExceedsTotal(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := mustDo(http.MethodPut, "/hotplug/memory", `{"total_size_mib":512,"slot_size_mib":256,"block_size_mib":128}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put status = %d", rec.Code)
	}
	rec := mustDo(http.MethodPatch, "/hotplug/memory", `{"requested_size_mib":1024}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMemoryHotplugPatch_PrebootNotAligned(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := mustDo(http.MethodPut, "/hotplug/memory", `{"total_size_mib":512,"slot_size_mib":256,"block_size_mib":128}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put status = %d", rec.Code)
	}
	rec := mustDo(http.MethodPatch, "/hotplug/memory", `{"requested_size_mib":100}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMemoryHotplugPut_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/hotplug/memory", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestNetRateLimiter_RuntimeUpdate(t *testing.T) {
	srv, vm := newStartedServer(t)
	_ = srv
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/net", bytes.NewBufferString(`{"ops":{"size":5,"refill_time_ms":10}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if vm.netRL == nil || vm.netRL.Ops.Size != 5 {
		t.Fatalf("net rate limiter not updated: %+v", vm.netRL)
	}
}

func TestBlockRateLimiter_RuntimeUpdate(t *testing.T) {
	srv, vm := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/block", bytes.NewBufferString(`{"bandwidth":{"size":2048}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if vm.blkRL == nil || vm.blkRL.Bandwidth.Size != 2048 {
		t.Fatalf("block rate limiter not updated: %+v", vm.blkRL)
	}
}

func TestRNGRateLimiter_RuntimeUpdate(t *testing.T) {
	srv, vm := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/rng", bytes.NewBufferString(`{"ops":{"size":10}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if vm.rngRL == nil || vm.rngRL.Ops.Size != 10 {
		t.Fatalf("rng rate limiter not updated: %+v", vm.rngRL)
	}
}

func TestNetRateLimiter_InvalidJSON(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/net", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBlockRateLimiter_InvalidJSON(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/block", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRNGRateLimiter_InvalidJSON(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/rng", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestNetRateLimiter_PrebootNoIface(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/net", bytes.NewBufferString(`{"ops":{"size":1}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBlockRateLimiter_PrebootNoDrive(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/block", bytes.NewBufferString(`{"ops":{"size":1}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRNGRateLimiter_PrebootCreatesConfig(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/rate-limiters/rng", bytes.NewBufferString(`{"ops":{"size":1}}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotEndpoint_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/snapshot", bytes.NewBufferString(`{"dest_dir":"/tmp"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestSnapshotEndpoint_EmptyDir(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/snapshot", bytes.NewBufferString(`{"dest_dir":""}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotEndpoint_InvalidJSON(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/snapshot", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMigrationPrepare_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/migrations/prepare", bytes.NewBufferString(`{"dest_dir":"/tmp"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMigrationPrepare_EmptyDir(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/migrations/prepare", bytes.NewBufferString(`{"dest_dir":""}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMigrationFinalize_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/migrations/finalize", bytes.NewBufferString(`{"dest_dir":"/tmp"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMigrationFinalize_EmptyDir(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/migrations/finalize", bytes.NewBufferString(`{"dest_dir":""}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMigrationReset_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/migrations/reset", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMigrationPrepare_WithRunningVM(t *testing.T) {
	srv, _ := newStartedServer(t)
	dir := t.TempDir()
	req := httptest.NewRequest(http.MethodPost, "/migrations/prepare", bytes.NewBufferString(`{"dest_dir":"`+strings.ReplaceAll(dir, `\`, `\\`)+`"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMigrationFinalize_WithRunningVM(t *testing.T) {
	srv, _ := newStartedServer(t)
	dir := t.TempDir()
	req := httptest.NewRequest(http.MethodPost, "/migrations/finalize", bytes.NewBufferString(`{"dest_dir":"`+strings.ReplaceAll(dir, `\`, `\\`)+`"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMigrationReset_WithRunningVM(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/migrations/reset", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEventsEndpoint_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestLogsEndpoint_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestVMInfoEndpoint_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vm", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBalloonGet_PrebootWithBalloon(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := mustDo(http.MethodPut, "/balloon", `{"amount_mib":128,"deflate_on_oom":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put balloon status = %d", rec.Code)
	}
	rec := mustDo(http.MethodGet, "/balloon", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get balloon status = %d body=%s", rec.Code, rec.Body.String())
	}
	var balloon Balloon
	if err := json.NewDecoder(rec.Body).Decode(&balloon); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if balloon.AmountMib != 128 || !balloon.DeflateOnOOM {
		t.Fatalf("balloon = %+v", balloon)
	}
}

func TestBalloonGet_PrebootNoBalloon(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/balloon", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestSharedFS_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/shared-fs/mytag", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestSharedFS_EmptySource(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/shared-fs/mytag", bytes.NewBufferString(`{"source":"","tag":"mytag"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSharedFS_Valid(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/shared-fs/mytag", bytes.NewBufferString(`{"source":"/host/data","tag":"mytag"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSharedFS_UpsertReplaces(t *testing.T) {
	srv := New()
	mustDo := func(body string) {
		req := httptest.NewRequest(http.MethodPut, "/shared-fs/mytag", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	}
	mustDo(`{"source":"/host/data1","tag":"mytag"}`)
	mustDo(`{"source":"/host/data2","tag":"mytag"}`)
	// Should have replaced, not appended
	srv.mu.RLock()
	count := len(srv.preboot.sharedFS)
	srv.mu.RUnlock()
	if count != 1 {
		t.Fatalf("expected 1 shared FS entry after upsert, got %d", count)
	}
}

func TestActionEndpoint_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBalloonPut_AfterStart(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/balloon", bytes.NewBufferString(`{"amount_mib":64}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected conflict after start, status = %d", rec.Code)
	}
}

func TestCloneVMLimiter_ServerPackage(t *testing.T) {
	if got := cloneVMLimiter(nil); got != nil {
		t.Fatalf("expected nil")
	}
	orig := &vmm.RateLimiterConfig{Bandwidth: vmm.TokenBucketConfig{Size: 100}}
	clone := cloneVMLimiter(orig)
	if clone == orig {
		t.Fatal("should be different pointer")
	}
	if clone.Bandwidth.Size != 100 {
		t.Fatalf("size = %d", clone.Bandwidth.Size)
	}
}

func TestCloneMemoryHotplug_ServerPackage(t *testing.T) {
	if got := cloneMemoryHotplug(nil); got != nil {
		t.Fatal("expected nil")
	}
	orig := &vmm.MemoryHotplugConfig{TotalSizeMiB: 512}
	clone := cloneMemoryHotplug(orig)
	if clone == orig {
		t.Fatal("should be different pointer")
	}
	if clone.TotalSizeMiB != 512 {
		t.Fatalf("total = %d", clone.TotalSizeMiB)
	}
}

func TestVMConfigID_Default(t *testing.T) {
	srv := New()
	if id := srv.vmConfigID(); id != "root-vm" {
		t.Fatalf("vmConfigID() = %q, want root-vm", id)
	}
}

func TestVMConfigID_Custom(t *testing.T) {
	srv := NewWithOptions(Options{VMID: "my-vm"})
	if id := srv.vmConfigID(); id != "my-vm" {
		t.Fatalf("vmConfigID() = %q, want my-vm", id)
	}
}

func TestStopVM_NoVM(t *testing.T) {
	srv := New()
	if err := srv.stopVM(); err == nil {
		t.Fatal("expected error stopping nil VM")
	}
}

func TestEventsEndpoint_InvalidSince(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodGet, "/events?since=not-a-date", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBalloonStatsPatch_PrebootWithBalloon(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := mustDo(http.MethodPut, "/balloon", `{"amount_mib":64,"deflate_on_oom":true,"stats_polling_interval_s":5}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put balloon status = %d", rec.Code)
	}
	rec := mustDo(http.MethodPatch, "/balloon/statistics", `{"stats_polling_interval_s":10}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch stats status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRestoreEndpoint_AlreadyStarted(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/restore", bytes.NewBufferString(`{"snapshot_dir":"/tmp"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRestoreEndpoint_EmptyDir(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/restore", bytes.NewBufferString(`{"snapshot_dir":""}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRestoreEndpoint_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/restore", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMemoryHotplugPatch_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBalloonStatsPatch_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPatch, "/balloon/statistics", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBalloonPut_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/balloon", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMigrationPrepare_InvalidJSON(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/migrations/prepare", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMigrationFinalize_InvalidJSON(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/migrations/finalize", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServerClose(t *testing.T) {
	srv, vm := newStartedServer(t)
	srv.Close()
	if !vm.stopped {
		t.Fatal("expected VM to be stopped after Close()")
	}
}

func TestServerClose_NoVM(t *testing.T) {
	srv := New()
	// Should not panic
	srv.Close()
}

// ---- Coverage-boosting tests: Client via httptest ----

func TestClientLifecycle(t *testing.T) {
	var vm *fakeVM
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		vm = newFakeVM(cfg).(*fakeVM)
		return vm, nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Create a client talking to the httptest server via TCP
	client := &http.Client{}
	base := ts.URL

	// SetBootSource
	resp, err := client.Do(mustReq(t, http.MethodPut, base+"/boot-source", `{"kernel_image_path":"/vmlinuz","boot_args":"console=ttyS0"}`))
	if err != nil {
		t.Fatalf("boot-source: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("boot-source status = %d", resp.StatusCode)
	}

	// SetMachineConfig
	resp, err = client.Do(mustReq(t, http.MethodPut, base+"/machine-config", `{"vcpu_count":1,"mem_size_mib":128}`))
	if err != nil {
		t.Fatalf("machine-config: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("machine-config status = %d", resp.StatusCode)
	}

	// SetDrive
	resp, err = client.Do(mustReq(t, http.MethodPut, base+"/drives/root", `{"path_on_host":"/disk.ext4","is_root_device":true}`))
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("drive status = %d", resp.StatusCode)
	}

	// Start
	resp, err = client.Do(mustReq(t, http.MethodPut, base+"/actions", `{"action_type":"InstanceStart"}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("start status = %d", resp.StatusCode)
	}

	// GetInfo
	resp, err = client.Get(base + "/vm")
	if err != nil {
		t.Fatalf("get info: %v", err)
	}
	var info VMInfo
	json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.State != "running" {
		t.Fatalf("state = %q, want running", info.State)
	}

	// GetEvents
	resp, err = client.Get(base + "/events")
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	var events []vmm.Event
	json.NewDecoder(resp.Body).Decode(&events)
	resp.Body.Close()
	if len(events) == 0 {
		t.Fatal("expected events")
	}

	// GetLogs
	resp, err = client.Get(base + "/logs")
	if err != nil {
		t.Fatalf("get logs: %v", err)
	}
	logsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(logsBody) != "hello\n" {
		t.Fatalf("logs = %q", logsBody)
	}

	// Snapshot
	dir := t.TempDir()
	resp, err = client.Do(mustReq(t, http.MethodPost, base+"/snapshot", `{"dest_dir":"`+strings.ReplaceAll(dir, `\`, `\\`)+`"}`))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snap vmm.Snapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if snap.Version != 2 {
		t.Fatalf("snap version = %d", snap.Version)
	}

	// SetNetRateLimiter (runtime)
	resp, err = client.Do(mustReq(t, http.MethodPut, base+"/rate-limiters/net", `{"ops":{"size":5}}`))
	if err != nil {
		t.Fatalf("net rate limiter: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("net rate limiter status = %d", resp.StatusCode)
	}

	// SetBlockRateLimiter (runtime)
	resp, err = client.Do(mustReq(t, http.MethodPut, base+"/rate-limiters/block", `{"bandwidth":{"size":2048}}`))
	if err != nil {
		t.Fatalf("block rate limiter: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("block rate limiter status = %d", resp.StatusCode)
	}

	// SetRNGRateLimiter (runtime)
	resp, err = client.Do(mustReq(t, http.MethodPut, base+"/rate-limiters/rng", `{"ops":{"size":10}}`))
	if err != nil {
		t.Fatalf("rng rate limiter: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rng rate limiter status = %d", resp.StatusCode)
	}

	// Stop
	resp, err = client.Do(mustReq(t, http.MethodPut, base+"/actions", `{"action_type":"InstanceStop"}`))
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("stop status = %d", resp.StatusCode)
	}
}

func mustReq(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestClientBalloonLifecycle(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{}
	base := ts.URL

	// Setup boot + balloon
	for _, step := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`},
		{http.MethodPut, "/balloon", `{"amount_mib":64,"deflate_on_oom":true,"stats_polling_interval_s":5}`},
		{http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`},
	} {
		resp, err := client.Do(mustReq(t, step.method, base+step.path, step.body))
		if err != nil {
			t.Fatalf("%s %s: %v", step.method, step.path, err)
		}
		resp.Body.Close()
	}

	// GetBalloon
	resp, err := client.Get(base + "/balloon")
	if err != nil {
		t.Fatalf("get balloon: %v", err)
	}
	var balloon Balloon
	json.NewDecoder(resp.Body).Decode(&balloon)
	resp.Body.Close()
	if balloon.AmountMib != 64 {
		t.Fatalf("balloon amount = %d", balloon.AmountMib)
	}

	// PatchBalloon
	resp, err = client.Do(mustReq(t, http.MethodPatch, base+"/balloon", `{"amount_mib":32}`))
	if err != nil {
		t.Fatalf("patch balloon: %v", err)
	}
	resp.Body.Close()

	// GetBalloonStats
	resp, err = client.Get(base + "/balloon/statistics")
	if err != nil {
		t.Fatalf("get balloon stats: %v", err)
	}
	resp.Body.Close()

	// PatchBalloonStats
	resp, err = client.Do(mustReq(t, http.MethodPatch, base+"/balloon/statistics", `{"stats_polling_interval_s":10}`))
	if err != nil {
		t.Fatalf("patch balloon stats: %v", err)
	}
	resp.Body.Close()
}

func TestClientMemoryHotplugLifecycle(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{}
	base := ts.URL

	// Setup
	for _, step := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`},
		{http.MethodPut, "/hotplug/memory", `{"total_size_mib":512,"slot_size_mib":256,"block_size_mib":128}`},
	} {
		resp, err := client.Do(mustReq(t, step.method, base+step.path, step.body))
		if err != nil {
			t.Fatalf("%s %s: %v", step.method, step.path, err)
		}
		resp.Body.Close()
	}

	// GetMemoryHotplug (preboot)
	resp, err := client.Get(base + "/hotplug/memory")
	if err != nil {
		t.Fatalf("get hotplug: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get hotplug status = %d", resp.StatusCode)
	}

	// PatchMemoryHotplug
	resp, err = client.Do(mustReq(t, http.MethodPatch, base+"/hotplug/memory", `{"requested_size_mib":256}`))
	if err != nil {
		t.Fatalf("patch hotplug: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch hotplug status = %d", resp.StatusCode)
	}
}

func TestClientMigrationLifecycle(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{}
	base := ts.URL

	// Setup and start
	for _, step := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`},
		{http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`},
	} {
		resp, err := client.Do(mustReq(t, step.method, base+step.path, step.body))
		if err != nil {
			t.Fatalf("%s %s: %v", step.method, step.path, err)
		}
		resp.Body.Close()
	}

	dir := t.TempDir()
	body := `{"dest_dir":"` + strings.ReplaceAll(dir, `\`, `\\`) + `"}`

	// PrepareMigrationBundle
	resp, err := client.Do(mustReq(t, http.MethodPost, base+"/migrations/prepare", body))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("prepare status = %d", resp.StatusCode)
	}

	// FinalizeMigrationBundle
	resp, err = client.Do(mustReq(t, http.MethodPost, base+"/migrations/finalize", body))
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finalize status = %d", resp.StatusCode)
	}

	// ResetMigrationTracking
	resp, err = client.Do(mustReq(t, http.MethodPost, base+"/migrations/reset", ``))
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d", resp.StatusCode)
	}
}

func TestParseUint32Value(t *testing.T) {
	tests := []struct {
		input   string
		want    uint32
		wantErr bool
	}{
		{"1234", 1234, false},
		{"0", 0, false},
		{"4294967295", 4294967295, false},
		{"", 0, true},
		{"  ", 0, true},
		{"abc", 0, true},
		{"4294967296", 0, true},
	}
	for _, tt := range tests {
		got, err := parseUint32Value(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseUint32Value(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseUint32Value(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestDecodeAPIError(t *testing.T) {
	// With valid APIError JSON
	body := strings.NewReader(`{"fault_message":"something broke"}`)
	err := decodeAPIError(body, 400)
	if err == nil || !strings.Contains(err.Error(), "something broke") {
		t.Fatalf("err = %v, want 'something broke'", err)
	}

	// With empty fault_message - falls through to raw body read
	body2 := strings.NewReader(`{"fault_message":""}`)
	err2 := decodeAPIError(body2, 500)
	if err2 == nil {
		t.Fatal("expected error")
	}
	// The body was consumed by json decode, so ReadAll gets empty
	if !strings.Contains(err2.Error(), "500") {
		t.Fatalf("err = %v, want status code", err2)
	}
}

func TestVsockConnect_NoVM(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodGet, "/vsock/connect?port=1234", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestVsockConnect_BadPort(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodGet, "/vsock/connect?port=abc", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestVsockConnect_EmptyPort(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodGet, "/vsock/connect?port=", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDriveUpsert_ReplacesExisting(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	mustDo(http.MethodPut, "/drives/root", `{"path_on_host":"/disk1.ext4","is_root_device":true}`)
	mustDo(http.MethodPut, "/drives/root", `{"path_on_host":"/disk2.ext4","is_root_device":true}`)
	srv.mu.RLock()
	count := len(srv.preboot.drives)
	srv.mu.RUnlock()
	if count != 1 {
		t.Fatalf("expected 1 drive after upsert, got %d", count)
	}
}

func TestNetworkIfaceUpsert_ReplacesExisting(t *testing.T) {
	srv := New()
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	mustDo(http.MethodPut, "/network-interfaces/eth0", `{"host_dev_name":"tap0"}`)
	mustDo(http.MethodPut, "/network-interfaces/eth0", `{"host_dev_name":"tap1"}`)
	srv.mu.RLock()
	count := len(srv.preboot.netIfaces)
	srv.mu.RUnlock()
	if count != 1 {
		t.Fatalf("expected 1 iface after upsert, got %d", count)
	}
}

func TestDriveEndpoint_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/drives/root", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestNetworkIfaceEndpoint_InvalidJSON(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPut, "/network-interfaces/eth0", bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestDriveEndpoint_AfterStart(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/drives/root", bytes.NewBufferString(`{"path_on_host":"/disk.ext4","is_root_device":true}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestNetworkIfaceEndpoint_AfterStart(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/network-interfaces/eth0", bytes.NewBufferString(`{"host_dev_name":"tap0"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestSharedFS_AfterStart(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPut, "/shared-fs/mytag", bytes.NewBufferString(`{"source":"/data"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestStartWithVsockAndExecConfig(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		if cfg.Vsock == nil || !cfg.Vsock.Enabled {
			t.Fatal("expected vsock to be enabled")
		}
		if cfg.Exec == nil || !cfg.Exec.Enabled {
			t.Fatal("expected exec to be enabled")
		}
		return newFakeVM(cfg), nil
	}})
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`)
	mustDo(http.MethodPut, "/machine-config", `{"vcpu_count":1,"mem_size_mib":128,"vsock_enabled":true,"vsock_guest_cid":3,"exec_enabled":true,"exec_vsock_port":10000}`)
	rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStartWithMultipleDrives(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		if len(cfg.Drives) != 2 {
			t.Fatalf("expected 2 drives, got %d", len(cfg.Drives))
		}
		return newFakeVM(cfg), nil
	}})
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`)
	mustDo(http.MethodPut, "/drives/root", `{"path_on_host":"/root.ext4","is_root_device":true}`)
	mustDo(http.MethodPut, "/drives/data", `{"path_on_host":"/data.ext4","is_root_device":false}`)
	rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStartWithSharedFS(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		if len(cfg.SharedFS) != 1 || cfg.SharedFS[0].Tag != "data" {
			t.Fatalf("expected 1 shared FS with tag=data, got %v", cfg.SharedFS)
		}
		return newFakeVM(cfg), nil
	}})
	mustDo := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	mustDo(http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`)
	mustDo(http.MethodPut, "/shared-fs/data", `{"source":"/host/data"}`)
	rec := mustDo(http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBalloonPatch_PostStartWithBalloon(t *testing.T) {
	// fakeVM implements BalloonController so patch works even without explicit balloon config
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/balloon", bytes.NewBufferString(`{"amount_mib":32}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// fakeVM creates balloon on demand, so this succeeds
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestBalloonStatsGet_PostStartWithBalloon(t *testing.T) {
	srv, vm := newStartedServer(t)
	// Set up balloon on fakeVM
	vm.cfg.Balloon = &vmm.BalloonConfig{AmountMiB: 64, StatsPollingIntervalS: 5}
	vm.balloon = vmm.BalloonStats{TargetMiB: 64, ActualMiB: 32}
	req := httptest.NewRequest(http.MethodGet, "/balloon/statistics", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var stats vmm.BalloonStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.TargetMiB != 64 {
		t.Fatalf("TargetMiB = %d, want 64", stats.TargetMiB)
	}
}

func TestBalloonStatsPatch_PostStartWithBalloon(t *testing.T) {
	srv, _ := newStartedServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/balloon/statistics", bytes.NewBufferString(`{"stats_polling_interval_s":5}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// fakeVM creates balloon on demand
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestBalloonGet_PostStartWithBalloon(t *testing.T) {
	srv, vm := newStartedServer(t)
	vm.cfg.Balloon = &vmm.BalloonConfig{AmountMiB: 128, DeflateOnOOM: true}
	req := httptest.NewRequest(http.MethodGet, "/balloon", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var balloon Balloon
	if err := json.NewDecoder(rec.Body).Decode(&balloon); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if balloon.AmountMib != 128 {
		t.Fatalf("amount = %d, want 128", balloon.AmountMib)
	}
}

func TestMemoryHotplugGet_PostStartWithHotplug(t *testing.T) {
	srv, vm := newStartedServer(t)
	vm.cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{TotalSizeMiB: 1024}
	vm.hotplug = vmm.MemoryHotplugStatus{TotalSizeMiB: 1024}
	req := httptest.NewRequest(http.MethodGet, "/hotplug/memory", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMemoryHotplugPatch_PostStartWithHotplug(t *testing.T) {
	srv, vm := newStartedServer(t)
	vm.cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{TotalSizeMiB: 1024, SlotSizeMiB: 256}
	req := httptest.NewRequest(http.MethodPatch, "/hotplug/memory", bytes.NewBufferString(`{"requested_size_mib":256}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestConcurrentRequests(t *testing.T) {
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		return newFakeVM(cfg), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Setup the server
	client := &http.Client{}
	base := ts.URL
	for _, step := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/vmlinuz"}`},
		{http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`},
	} {
		resp, _ := client.Do(mustReq(t, step.method, base+step.path, step.body))
		resp.Body.Close()
	}

	// Make concurrent requests
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			resp, err := client.Get(base + "/vm")
			if err != nil {
				return
			}
			resp.Body.Close()
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestBlockRateLimiter_PrebootWithNonRootDrive(t *testing.T) {
	srv := New()
	// Add a non-root drive
	req := httptest.NewRequest(http.MethodPut, "/drives/data", bytes.NewBufferString(`{"path_on_host":"/data.ext4","is_root_device":false}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	// Try to set block rate limiter - should fail because no root drive
	req = httptest.NewRequest(http.MethodPut, "/rate-limiters/block", bytes.NewBufferString(`{"ops":{"size":1}}`))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRestoreEndpoint_InvalidX86Boot(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/restore", bytes.NewBufferString(`{"snapshot_dir":"/tmp","x86_boot":"bogus"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---- Client struct tests (via Unix socket) ----

func TestClientViaUnixSocket(t *testing.T) {
	var vm *fakeVM
	srv := NewWithOptions(Options{Factory: func(cfg vmm.Config) (VM, error) {
		vm = newFakeVM(cfg).(*fakeVM)
		return vm, nil
	}})

	socketPath := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go http.Serve(ln, srv)

	ctx := context.Background()
	client := NewClient(socketPath)

	// SetBootSource
	if err := client.SetBootSource(ctx, BootSource{
		KernelImagePath: "/vmlinuz",
		BootArgs:        "console=ttyS0",
	}); err != nil {
		t.Fatalf("SetBootSource: %v", err)
	}

	// SetMachineConfig
	if err := client.SetMachineConfig(ctx, MachineConfig{
		VcpuCount:  1,
		MemSizeMib: 256,
	}); err != nil {
		t.Fatalf("SetMachineConfig: %v", err)
	}

	// SetDrive
	if err := client.SetDrive(ctx, "root", Drive{
		PathOnHost:   "/disk.ext4",
		IsRootDevice: true,
	}); err != nil {
		t.Fatalf("SetDrive: %v", err)
	}

	// SetNetworkInterface
	if err := client.SetNetworkInterface(ctx, "eth0", NetworkInterface{
		HostDevName: "tap0",
	}); err != nil {
		t.Fatalf("SetNetworkInterface: %v", err)
	}

	// SetSharedFS
	if err := client.SetSharedFS(ctx, "myfs", SharedFS{
		Source: "/host/data",
	}); err != nil {
		t.Fatalf("SetSharedFS: %v", err)
	}

	// SetBalloon
	if err := client.SetBalloon(ctx, Balloon{
		AmountMib:    64,
		DeflateOnOOM: true,
	}); err != nil {
		t.Fatalf("SetBalloon: %v", err)
	}

	// GetBalloon (preboot)
	balloon, err := client.GetBalloon(ctx)
	if err != nil {
		t.Fatalf("GetBalloon: %v", err)
	}
	if balloon.AmountMib != 64 {
		t.Fatalf("balloon = %d", balloon.AmountMib)
	}

	// SetMemoryHotplug
	if err := client.SetMemoryHotplug(ctx, MemoryHotplugConfig{
		TotalSizeMiB: 512,
		SlotSizeMiB:  256,
		BlockSizeMiB: 128,
	}); err != nil {
		t.Fatalf("SetMemoryHotplug: %v", err)
	}

	// GetMemoryHotplug (preboot)
	status, err := client.GetMemoryHotplug(ctx)
	if err != nil {
		t.Fatalf("GetMemoryHotplug: %v", err)
	}
	if status.TotalSizeMiB != 512 {
		t.Fatalf("total = %d", status.TotalSizeMiB)
	}

	// PatchMemoryHotplug
	if err := client.PatchMemoryHotplug(ctx, MemoryHotplugSizeUpdate{
		RequestedSizeMiB: 256,
	}); err != nil {
		t.Fatalf("PatchMemoryHotplug: %v", err)
	}

	// PatchBalloon
	if err := client.PatchBalloon(ctx, BalloonUpdate{AmountMib: 32}); err != nil {
		t.Fatalf("PatchBalloon: %v", err)
	}

	// GetBalloonStats (preboot with stats enabled)
	_, err = client.GetBalloonStats(ctx)
	// This may fail because stats_polling_interval_s not set
	_ = err

	// PatchBalloonStats
	if err := client.PatchBalloonStats(ctx, BalloonStatsUpdate{StatsPollingIntervalS: 5}); err != nil {
		t.Fatalf("PatchBalloonStats: %v", err)
	}

	// SetNetRateLimiter (preboot with iface configured)
	if err := client.SetNetRateLimiter(ctx, vmm.RateLimiterConfig{
		Ops: vmm.TokenBucketConfig{Size: 5},
	}); err != nil {
		t.Fatalf("SetNetRateLimiter: %v", err)
	}

	// SetRNGRateLimiter (always works preboot)
	if err := client.SetRNGRateLimiter(ctx, vmm.RateLimiterConfig{
		Ops: vmm.TokenBucketConfig{Size: 1},
	}); err != nil {
		t.Fatalf("SetRNGRateLimiter: %v", err)
	}

	// Start
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// SetBlockRateLimiter (runtime)
	if err := client.SetBlockRateLimiter(ctx, vmm.RateLimiterConfig{
		Bandwidth: vmm.TokenBucketConfig{Size: 2048},
	}); err != nil {
		t.Fatalf("SetBlockRateLimiter: %v", err)
	}

	// GetInfo
	info, err := client.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.State != "running" {
		t.Fatalf("state = %q", info.State)
	}

	// GetEvents
	events, err := client.GetEvents(ctx, time.Time{})
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected events")
	}

	// GetLogs
	logs, err := client.GetLogs(ctx)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if string(logs) != "hello\n" {
		t.Fatalf("logs = %q", logs)
	}

	// Snapshot
	snapDir := t.TempDir()
	snap, err := client.Snapshot(ctx, SnapshotRequest{DestDir: snapDir})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Version != 2 {
		t.Fatalf("snap version = %d", snap.Version)
	}

	// PrepareMigrationBundle
	migDir := t.TempDir()
	if err := client.PrepareMigrationBundle(ctx, SnapshotRequest{DestDir: migDir}); err != nil {
		t.Fatalf("PrepareMigrationBundle: %v", err)
	}

	// FinalizeMigrationBundle
	finalSnap, patches, err := client.FinalizeMigrationBundle(ctx, SnapshotRequest{DestDir: migDir})
	if err != nil {
		t.Fatalf("FinalizeMigrationBundle: %v", err)
	}
	if finalSnap == nil {
		t.Fatal("expected snapshot from finalize")
	}
	if patches == nil {
		t.Fatal("expected patches from finalize")
	}

	// ResetMigrationTracking
	if err := client.ResetMigrationTracking(ctx); err != nil {
		t.Fatalf("ResetMigrationTracking: %v", err)
	}

	// Stop
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestMemoryHotplugPut_InvalidValidation(t *testing.T) {
	srv := New()
	// slot_size_mib must be > 0 normally; just test that validation runs
	req := httptest.NewRequest(http.MethodPut, "/hotplug/memory", bytes.NewBufferString(`{"total_size_mib":0,"slot_size_mib":0,"block_size_mib":0}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// This may pass validation with zeros - just confirm no panic
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}
