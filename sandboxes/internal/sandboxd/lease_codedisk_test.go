//go:build linux

package sandboxd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gocracker/gocracker/pkg/container"
)

// TestHandleLeaseSandbox_ForwardsCodeDisks gates the Phase 3 wire
// shape: a JSON request carrying a code_disks array must arrive at
// PoolLifecycle.LeaseSandbox unchanged. The pool itself does not
// apply the disks yet (deferred — warm-resume + no hot-plug), but
// the handler must not silently drop the field.
func TestHandleLeaseSandbox_ForwardsCodeDisks(t *testing.T) {
	pl := &fakePoolLifecycle{
		fakeLifecycle: &fakeLifecycle{},
		leaseResult:   Sandbox{ID: "sb-0", State: StateReady, RuntimeID: "sb-0"},
	}
	srv := &Server{}

	body := map[string]any{
		"template_id": "tmpl-x",
		"code_disks": []map[string]any{
			{
				"host_path": "/srv/v1.ext4",
				"mount":     "/app",
				"fs_type":   "ext4",
				"read_only": true,
			},
		},
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/lease", bytes.NewReader(enc))
	rr := httptest.NewRecorder()
	srv.handleLeaseSandbox(pl)(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	if pl.leaseCalls != 1 {
		t.Fatalf("LeaseSandbox calls = %d, want 1", pl.leaseCalls)
	}

	if pl.leaseArg.TemplateID != "tmpl-x" {
		t.Errorf("TemplateID = %q, want tmpl-x", pl.leaseArg.TemplateID)
	}
	if got, want := len(pl.leaseArg.CodeDisks), 1; got != want {
		t.Fatalf("CodeDisks len = %d, want %d", got, want)
	}
	got := pl.leaseArg.CodeDisks[0]
	want := container.CodeDisk{
		HostPath: "/srv/v1.ext4",
		Mount:    "/app",
		FSType:   "ext4",
		ReadOnly: true,
	}
	if got != want {
		t.Errorf("CodeDisks[0] = %+v, want %+v", got, want)
	}
}

// TestHandleLeaseSandbox_NoCodeDisks confirms the field is omitempty
// on the wire — clients that don't pass code_disks shouldn't have
// the field synthesized by the server, and the pool path stays
// fast-path-clean.
func TestHandleLeaseSandbox_NoCodeDisks(t *testing.T) {
	pl := &fakePoolLifecycle{
		fakeLifecycle: &fakeLifecycle{},
		leaseResult:   Sandbox{ID: "sb-1", State: StateReady},
	}
	srv := &Server{}
	body := []byte(`{"template_id":"tmpl-y"}`)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/lease", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.handleLeaseSandbox(pl)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if len(pl.leaseArg.CodeDisks) != 0 {
		t.Errorf("CodeDisks should be empty by default, got %+v", pl.leaseArg.CodeDisks)
	}
}
