package vmmserver

import (
	"bytes"
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
