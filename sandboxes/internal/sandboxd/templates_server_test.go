package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gocracker/gocracker/sandboxes/internal/templates"
)

// fakeTemplateLifecycle satisfies Lifecycle + TemplateLifecycle for
// HTTP server tests. Same shape as fakePoolLifecycle.
type fakeTemplateLifecycle struct {
	*fakeLifecycle
	createCalls    int
	createErr      error
	createArg      CreateTemplateRequest
	createResult   CreateTemplateResponse
	getResult      templates.Template
	getErr         error
	listResult     []templates.Template
	deleteCalls    int
	deleteErr      error
}

func (f *fakeTemplateLifecycle) CreateTemplate(_ context.Context, req CreateTemplateRequest) (CreateTemplateResponse, error) {
	f.createCalls++
	f.createArg = req
	if f.createErr != nil {
		return CreateTemplateResponse{}, f.createErr
	}
	return f.createResult, nil
}

func (f *fakeTemplateLifecycle) GetTemplate(_ string) (templates.Template, error) {
	if f.getErr != nil {
		return templates.Template{}, f.getErr
	}
	return f.getResult, nil
}

func (f *fakeTemplateLifecycle) ListTemplates() []templates.Template {
	return f.listResult
}

func (f *fakeTemplateLifecycle) DeleteTemplate(_ string) error {
	f.deleteCalls++
	return f.deleteErr
}

func newTemplateTestServer(t *testing.T) (*httptest.Server, *fakeTemplateLifecycle, *Store) {
	t.Helper()
	store, _ := NewStore("")
	base := &fakeLifecycle{store: store}
	tl := &fakeTemplateLifecycle{fakeLifecycle: base}
	srv := httptest.NewServer((&Server{Lifecycle: tl, Store: store}).Handler())
	t.Cleanup(srv.Close)
	return srv, tl, store
}

func TestServer_CreateTemplate_Happy(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.createResult = CreateTemplateResponse{
		Template: templates.Template{ID: "tmpl-x", State: templates.StateReady, SnapshotDir: "/snap"},
		CacheHit: false,
	}
	body, _ := json.Marshal(CreateTemplateRequest{Image: "alpine:3.20", KernelPath: "/k"})
	resp, err := http.Post(srv.URL+"/templates", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d, want 201", resp.StatusCode)
	}
	var got CreateTemplateResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Template.ID != "tmpl-x" || got.CacheHit {
		t.Errorf("got=%+v", got)
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls=%d, want 1", fake.createCalls)
	}
}

func TestServer_CreateTemplate_BadJSON(t *testing.T) {
	srv, _, _ := newTemplateTestServer(t)
	resp, _ := http.Post(srv.URL+"/templates", "application/json", bytes.NewReader([]byte("garbage")))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestServer_CreateTemplate_InvalidSpec(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.createErr = ErrInvalidRequest
	body, _ := json.Marshal(CreateTemplateRequest{Image: "i", KernelPath: "/k"})
	resp, _ := http.Post(srv.URL+"/templates", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestServer_CreateTemplate_TemplatesInvalidSpecError(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.createErr = templates.ErrInvalidSpec
	body, _ := json.Marshal(CreateTemplateRequest{Image: "i", KernelPath: "/k"})
	resp, _ := http.Post(srv.URL+"/templates", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (ErrInvalidSpec → 400)", resp.StatusCode)
	}
}

func TestServer_CreateTemplate_GenericError(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.createErr = errors.New("boom")
	body, _ := json.Marshal(CreateTemplateRequest{Image: "i", KernelPath: "/k"})
	resp, _ := http.Post(srv.URL+"/templates", "application/json", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", resp.StatusCode)
	}
}

func TestServer_GetTemplate_Happy(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.getResult = templates.Template{ID: "tmpl-x", State: templates.StateReady}
	resp, err := http.Get(srv.URL + "/templates/tmpl-x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var got templates.Template
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.ID != "tmpl-x" {
		t.Errorf("got=%+v", got)
	}
}

func TestServer_GetTemplate_NotFound(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.getErr = templates.ErrTemplateNotFound
	resp, _ := http.Get(srv.URL + "/templates/missing")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestServer_ListTemplates(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.listResult = []templates.Template{
		{ID: "a"}, {ID: "b"},
	}
	resp, _ := http.Get(srv.URL + "/templates")
	defer resp.Body.Close()
	var got ListTemplatesResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Templates) != 2 {
		t.Errorf("got %d templates, want 2", len(got.Templates))
	}
}

func TestServer_DeleteTemplate_Happy(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/templates/tmpl-x", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", resp.StatusCode)
	}
	if fake.deleteCalls != 1 {
		t.Errorf("deleteCalls=%d, want 1", fake.deleteCalls)
	}
}

func TestServer_DeleteTemplate_NotFound(t *testing.T) {
	srv, fake, _ := newTemplateTestServer(t)
	fake.deleteErr = templates.ErrTemplateNotFound
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/templates/missing", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestServer_TemplateRoutes_NotMountedWithoutTemplateLifecycle(t *testing.T) {
	store, _ := NewStore("")
	base := &fakeLifecycle{store: store}
	srv := httptest.NewServer((&Server{Lifecycle: base, Store: store}).Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/templates", "application/json", bytes.NewReader([]byte("{}")))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (route not mounted)", resp.StatusCode)
	}
}
