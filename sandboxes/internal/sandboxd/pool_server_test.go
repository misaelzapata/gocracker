package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakePoolLifecycle implements both Lifecycle and PoolLifecycle for
// HTTP server tests. Backed by an in-memory store; no real pool spun
// up. Real pool integration is exercised in slice 8's pool-bench
// against KVM.
type fakePoolLifecycle struct {
	*fakeLifecycle
	registerCalls    int
	registerErr      error
	registerArg      CreatePoolRequest
	registerResult   PoolRegistration
	unregisterCalls  int
	unregisterErr    error
	listResult       []PoolRegistration
	leaseCalls       int
	leaseErr         error
	leaseArg         LeaseSandboxRequest
	leaseResult      Sandbox
	releaseCalls     int
	releaseErr       error
	recycleCalls     int
	recycleErr       error
	recycleArg       string
	recycleResult    Sandbox
}

func (f *fakePoolLifecycle) RegisterPool(_ context.Context, req CreatePoolRequest) (PoolRegistration, error) {
	f.registerCalls++
	f.registerArg = req
	if f.registerErr != nil {
		return PoolRegistration{}, f.registerErr
	}
	return f.registerResult, nil
}

func (f *fakePoolLifecycle) UnregisterPool(id string) error {
	f.unregisterCalls++
	return f.unregisterErr
}

func (f *fakePoolLifecycle) ListPools() []PoolRegistration {
	return f.listResult
}

func (f *fakePoolLifecycle) LeaseSandbox(_ context.Context, req LeaseSandboxRequest) (Sandbox, error) {
	f.leaseCalls++
	f.leaseArg = req
	if f.leaseErr != nil {
		return Sandbox{}, f.leaseErr
	}
	return f.leaseResult, nil
}

func (f *fakePoolLifecycle) ReleaseLeased(_ string) error {
	f.releaseCalls++
	return f.releaseErr
}

func (f *fakePoolLifecycle) RecycleLeased(_ context.Context, id string) (Sandbox, error) {
	f.recycleCalls++
	f.recycleArg = id
	if f.recycleErr != nil {
		return Sandbox{}, f.recycleErr
	}
	return f.recycleResult, nil
}

func newPoolTestServer(t *testing.T) (*httptest.Server, *fakePoolLifecycle, *Store) {
	t.Helper()
	store, _ := NewStore("")
	base := &fakeLifecycle{store: store}
	pool := &fakePoolLifecycle{fakeLifecycle: base}
	srv := httptest.NewServer((&Server{Lifecycle: pool, Store: store}).Handler())
	t.Cleanup(srv.Close)
	return srv, pool, store
}

func TestServer_RegisterPool_Happy(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.registerResult = PoolRegistration{
		TemplateID: "tmpl-x",
		Image:      "alpine:3.20",
		MinPaused:  4,
	}

	body, _ := json.Marshal(CreatePoolRequest{
		TemplateID: "tmpl-x",
		Image:      "alpine:3.20",
		KernelPath: "/k",
	})
	resp, err := http.Post(srv.URL+"/pools", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d, want 201", resp.StatusCode)
	}
	if fake.registerCalls != 1 {
		t.Errorf("registerCalls=%d, want 1", fake.registerCalls)
	}
	if fake.registerArg.TemplateID != "tmpl-x" {
		t.Errorf("registerArg.TemplateID=%q, want tmpl-x", fake.registerArg.TemplateID)
	}
}

func TestServer_RegisterPool_BadJSON(t *testing.T) {
	srv, _, _ := newPoolTestServer(t)
	resp, _ := http.Post(srv.URL+"/pools", "application/json", bytes.NewReader([]byte("garbage")))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestServer_RegisterPool_AlreadyExists(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.registerErr = ErrPoolAlreadyRegistered
	body, _ := json.Marshal(CreatePoolRequest{TemplateID: "x", Image: "i", KernelPath: "/k"})
	resp, _ := http.Post(srv.URL+"/pools", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", resp.StatusCode)
	}
}

func TestServer_RegisterPool_InvalidRequest(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.registerErr = ErrInvalidRequest
	body, _ := json.Marshal(CreatePoolRequest{TemplateID: "x", Image: "i", KernelPath: "/k"})
	resp, _ := http.Post(srv.URL+"/pools", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestServer_ListPools(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.listResult = []PoolRegistration{
		{TemplateID: "a"}, {TemplateID: "b"},
	}
	resp, err := http.Get(srv.URL + "/pools")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var got struct {
		Pools []PoolRegistration `json:"pools"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Pools) != 2 || got.Pools[0].TemplateID != "a" {
		t.Errorf("got pools=%+v, want [a, b]", got.Pools)
	}
}

func TestServer_UnregisterPool_Happy(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/pools/tmpl-x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", resp.StatusCode)
	}
	if fake.unregisterCalls != 1 {
		t.Errorf("unregisterCalls=%d, want 1", fake.unregisterCalls)
	}
}

func TestServer_UnregisterPool_NotFound(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.unregisterErr = ErrPoolNotFound
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/pools/missing", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestServer_LeaseSandbox_Happy(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.leaseResult = Sandbox{
		ID:      "sb-leased",
		State:   StateReady,
		Image:   "alpine:3.20",
		UDSPath: "/tmp/u.sock",
		GuestIP: "198.19.0.2/30",
	}
	body, _ := json.Marshal(LeaseSandboxRequest{TemplateID: "tmpl-x", Timeout: 2 * time.Second})
	resp, err := http.Post(srv.URL+"/sandboxes/lease", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d, want 201", resp.StatusCode)
	}
	var got CreateSandboxResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Sandbox.ID != "sb-leased" || got.Sandbox.GuestIP != "198.19.0.2/30" {
		t.Errorf("Sandbox=%+v", got.Sandbox)
	}
	if fake.leaseArg.TemplateID != "tmpl-x" {
		t.Errorf("leaseArg.TemplateID=%q, want tmpl-x", fake.leaseArg.TemplateID)
	}
}

func TestServer_LeaseSandbox_PoolNotFound(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.leaseErr = ErrPoolNotFound
	body, _ := json.Marshal(LeaseSandboxRequest{TemplateID: "missing"})
	resp, _ := http.Post(srv.URL+"/sandboxes/lease", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestServer_DeleteSandbox_RoutesLeasedToReleasedPath(t *testing.T) {
	srv, fake, store := newPoolTestServer(t)
	// Pre-populate a leased sandbox in the store (poolTemplateID set
	// → DELETE handler routes to ReleaseLeased).
	store.Add(&Sandbox{ID: "sb-leased", State: StateReady, poolTemplateID: "tmpl-x"})

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/sandboxes/sb-leased", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", resp.StatusCode)
	}
	if fake.releaseCalls != 1 {
		t.Errorf("releaseCalls=%d, want 1 (lease path)", fake.releaseCalls)
	}
	if fake.fakeLifecycle.deleteErr != nil {
		t.Skip("legacy delete path not exercised here")
	}
}

func TestServer_DeleteSandbox_RoutesColdToOriginalPath(t *testing.T) {
	srv, fake, store := newPoolTestServer(t)
	// Cold-booted sandbox: poolTemplateID empty → goes through Lifecycle.Delete.
	store.Add(&Sandbox{ID: "sb-cold", State: StateReady})

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/sandboxes/sb-cold", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", resp.StatusCode)
	}
	if fake.releaseCalls != 0 {
		t.Errorf("releaseCalls=%d, want 0 (cold path shouldn't call Release)", fake.releaseCalls)
	}
}

// Sanity: the optional PoolLifecycle interface is detected and
// the routes are mounted only when present.
func TestServer_PoolRoutes_NotMountedWithoutPoolLifecycle(t *testing.T) {
	store, _ := NewStore("")
	base := &fakeLifecycle{store: store}
	srv := httptest.NewServer((&Server{Lifecycle: base, Store: store}).Handler())
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/pools", "application/json", bytes.NewReader([]byte("{}")))
	defer resp.Body.Close()
	// Without PoolLifecycle the route isn't registered → 404.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (route not mounted)", resp.StatusCode)
	}
}

// confirm errors.Is wiring across status decisions.
func TestServer_RegisterPool_GenericErrorIs500(t *testing.T) {
	srv, fake, _ := newPoolTestServer(t)
	fake.registerErr = errors.New("boom")
	body, _ := json.Marshal(CreatePoolRequest{TemplateID: "x", Image: "i", KernelPath: "/k"})
	resp, _ := http.Post(srv.URL+"/pools", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", resp.StatusCode)
	}
}
