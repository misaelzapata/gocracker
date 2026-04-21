package sandboxd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeLifecycle satisfies Lifecycle without booting real VMs. The
// server tests focus on routing + payload shape; the real Manager
// is exercised via the live E2E in tests/integration.
type fakeLifecycle struct {
	store         *Store
	createErr     error
	deleteErr     error
	createCalls   int
	createOpts    CreateSandboxRequest
	createdResult Sandbox
}

func (f *fakeLifecycle) Create(req CreateSandboxRequest) (Sandbox, error) {
	f.createCalls++
	f.createOpts = req
	if f.createErr != nil {
		return Sandbox{}, f.createErr
	}
	sb := &Sandbox{
		ID:        "sb-fake-" + req.Image,
		State:     StateReady,
		Image:     req.Image,
		CreatedAt: time.Now().UTC(),
	}
	_ = f.store.Add(sb)
	snap, _ := f.store.Get(sb.ID)
	f.createdResult = snap
	return snap, nil
}

func (f *fakeLifecycle) Delete(id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.store.Remove(id); !ok {
		return ErrSandboxNotFound
	}
	return nil
}

func newTestServer(t *testing.T) (*httptest.Server, *fakeLifecycle, *Store) {
	t.Helper()
	store, _ := NewStore("")
	fake := &fakeLifecycle{store: store}
	srv := httptest.NewServer((&Server{Lifecycle: fake, Store: store}).Handler())
	t.Cleanup(srv.Close)
	return srv, fake, store
}

func TestServer_Healthz(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
}

func TestServer_CreateSandbox_Happy(t *testing.T) {
	srv, fake, _ := newTestServer(t)
	body, _ := json.Marshal(CreateSandboxRequest{Image: "alpine:3.20", KernelPath: "/k"})
	resp, err := http.Post(srv.URL+"/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d, want 201", resp.StatusCode)
	}
	if fake.createCalls != 1 {
		t.Fatalf("Create not invoked exactly once: %d", fake.createCalls)
	}
	if fake.createOpts.Image != "alpine:3.20" {
		t.Fatalf("Create opts image: got %q", fake.createOpts.Image)
	}
	var got CreateSandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Sandbox.ID != "sb-fake-alpine:3.20" {
		t.Fatalf("response sandbox ID: %q", got.Sandbox.ID)
	}
}

func TestServer_CreateSandbox_LifecycleError(t *testing.T) {
	srv, fake, _ := newTestServer(t)
	fake.createErr = errors.New("boom")
	body, _ := json.Marshal(CreateSandboxRequest{Image: "x", KernelPath: "/k"})
	resp, err := http.Post(srv.URL+"/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", resp.StatusCode)
	}
}

func TestServer_CreateSandbox_BadJSON(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/sandboxes", "application/json", bytes.NewReader([]byte("{not json")))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestServer_GetSandbox_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/sandboxes/missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
}

func TestServer_ListSandboxes(t *testing.T) {
	srv, _, store := newTestServer(t)
	_ = store.Add(&Sandbox{ID: "sb-1", State: StateReady, CreatedAt: time.Now()})
	_ = store.Add(&Sandbox{ID: "sb-2", State: StateReady, CreatedAt: time.Now().Add(time.Second)})
	resp, err := http.Get(srv.URL + "/sandboxes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got ListSandboxesResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Sandboxes) != 2 {
		t.Fatalf("expected 2, got %d", len(got.Sandboxes))
	}
}

func TestServer_DeleteSandbox(t *testing.T) {
	srv, fake, store := newTestServer(t)
	_ = store.Add(&Sandbox{ID: "sb-del", State: StateReady, CreatedAt: time.Now()})
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/sandboxes/sb-del", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}
	if _, ok := store.Get("sb-del"); ok {
		t.Fatal("expected sandbox gone")
	}
	_ = fake // suppress unused
}

func TestServer_DeleteSandbox_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/sandboxes/missing", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
}

// guard against route-shape regression.
func TestServer_PostNotAllowedOnGet(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/sandboxes/anything", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 for POST on GET-only route, got %d", resp.StatusCode)
	}
}
