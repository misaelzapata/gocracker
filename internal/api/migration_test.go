package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMigrationEndpointURL(t *testing.T) {
	got, err := migrationEndpointURL("http://localhost:8080/api")
	if err != nil {
		t.Fatalf("migrationEndpointURL: %v", err)
	}
	if want := "http://localhost:8080/api/migrations/load"; got != want {
		t.Fatalf("migrationEndpointURL = %q, want %q", got, want)
	}
}

func TestHandleMigrateVM_RequiresDestinationURL(t *testing.T) {
	srv := New()
	req := httptest.NewRequest(http.MethodPost, "/vms/vm1/migrate", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleMigrateVMRejectsCrossArchDestinationBeforePrepare(t *testing.T) {
	sourceArch := runtime.GOARCH
	destArch := "arm64"
	if sourceArch == destArch {
		destArch = "amd64"
	}
	prepareCalls := 0
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_ = json.NewEncoder(w).Encode(InstanceInfo{
				ID:         "dest",
				State:      "running=0",
				AppName:    "gocracker",
				VMMVersion: "0.2.0",
				HostArch:   destArch,
			})
		case "/migrations/prepare":
			prepareCalls++
			http.Error(w, "should not prepare cross-arch migration", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer dest.Close()

	srv := New()
	handle := newFakeHandle("vm1")
	handle.cfg.Arch = sourceArch
	srv.vms["vm1"] = &vmEntry{handle: handle, createdAt: time.Now()}

	reqBody := fmt.Sprintf(`{"destination_url":%q}`, dest.URL)
	req := httptest.NewRequest(http.MethodPost, "/vms/vm1/migrate", bytes.NewBufferString(reqBody))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	if prepareCalls != 0 {
		t.Fatalf("prepareCalls = %d, want 0", prepareCalls)
	}
	if !strings.Contains(rec.Body.String(), "same-arch only") {
		t.Fatalf("body = %q, want same-arch rejection", rec.Body.String())
	}
}

func TestWriteAndReadMigrationBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(`{"id":"vm1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mem.bin"), []byte("memory"), 0600); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	reqPart, err := writer.CreateFormField("request")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(reqPart).Encode(MigrationLoadRequest{VMID: "vm2", Resume: true}); err != nil {
		t.Fatal(err)
	}
	bundlePart, err := writer.CreateFormFile("bundle", "bundle.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarGz(bundlePart, dir); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/migrations/load", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	loadReq, bundleDir, err := readMigrationUpload(req)
	if err != nil {
		t.Fatalf("readMigrationUpload: %v", err)
	}
	defer os.RemoveAll(bundleDir)

	if loadReq.VMID != "vm2" || !loadReq.Resume {
		t.Fatalf("loadReq = %+v", loadReq)
	}
	for _, rel := range []string{"snapshot.json", "mem.bin"} {
		if _, err := os.Stat(filepath.Join(bundleDir, rel)); err != nil {
			t.Fatalf("bundle missing %s: %v", rel, err)
		}
	}
}
