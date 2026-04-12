package buildserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocracker/gocracker/internal/dockerfile"
	"github.com/gocracker/gocracker/internal/oci"
)

func TestHandleBuildValidatesRequest(t *testing.T) {
	srv := New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/build", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/build", strings.NewReader("{"))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleBuildFromImageSuccess(t *testing.T) {
	origPull := pullImage
	origExtract := extractToDir
	pullImage = func(opts oci.PullOptions) (*oci.PulledImage, error) {
		return &oci.PulledImage{Config: oci.ImageConfig{Env: []string{"A=B"}}}, nil
	}
	extractToDir = func(_ *oci.PulledImage, dir string) error {
		return os.WriteFile(filepath.Join(dir, "ok"), []byte("ok"), 0o644)
	}
	defer func() {
		pullImage = origPull
		extractToDir = origExtract
	}()

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"image":"alpine:latest","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	New().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp BuildResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if resp.RootfsDir != outputDir {
		t.Fatalf("RootfsDir = %q", resp.RootfsDir)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "ok")); err != nil {
		t.Fatalf("expected output artifact: %v", err)
	}
}

func TestHandleBuildFromDockerfileSuccess(t *testing.T) {
	origBuild := buildDockerfile
	buildDockerfile = func(opts dockerfile.BuildOptions) (*dockerfile.BuildResult, error) {
		if opts.OutputDir == "" || opts.DockerfilePath == "" {
			t.Fatalf("opts = %+v", opts)
		}
		return &dockerfile.BuildResult{Config: oci.ImageConfig{WorkingDir: "/app"}}, nil
	}
	defer func() { buildDockerfile = origBuild }()

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"dockerfile":"/tmp/Dockerfile","context":"/tmp","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	New().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBuildReportsWorkerError(t *testing.T) {
	origPull := pullImage
	pullImage = func(oci.PullOptions) (*oci.PulledImage, error) {
		return nil, errors.New("pull failed")
	}
	defer func() { pullImage = origPull }()

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"image":"alpine:latest","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	New().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pull failed") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestClearDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := clearDirectory(dir); err != nil {
		t.Fatalf("clearDirectory() error = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
	if err := clearDirectory(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("clearDirectory(missing) error = %v", err)
	}
}

func TestClientBuildSuccessAndError(t *testing.T) {
	mode := "ok"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "ok":
			_ = json.NewEncoder(w).Encode(BuildResponse{RootfsDir: "/tmp/rootfs"})
		case "api-error":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(apiError{FaultMessage: "bad request"})
		default:
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
		}
	}))
	defer server.Close()

	client := &Client{httpClient: server.Client(), baseURL: server.URL}
	resp, err := client.Build(context.Background(), BuildRequest{OutputDir: "/tmp/out"})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if resp.RootfsDir != "/tmp/rootfs" {
		t.Fatalf("RootfsDir = %q", resp.RootfsDir)
	}

	mode = "api-error"
	if _, err := client.Build(context.Background(), BuildRequest{OutputDir: "/tmp/out"}); err == nil || err.Error() != "bad request" {
		t.Fatalf("Build(api-error) err = %v", err)
	}

	mode = "status-error"
	if _, err := client.Build(context.Background(), BuildRequest{OutputDir: "/tmp/out"}); err == nil || !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Fatalf("Build(status-error) err = %v", err)
	}
}

func jsonString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
