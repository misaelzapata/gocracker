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
	"time"

	"github.com/gocracker/gocracker/internal/dockerfile"
	"github.com/gocracker/gocracker/internal/oci"
)

func newTestServer(opts ...func(*Server)) *Server {
	s := New()
	for _, o := range opts {
		o(s)
	}
	return s
}

func TestHandleBuildMissingOutputDir(t *testing.T) {
	body := `{"image":"alpine:latest"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	New().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "output_dir") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleBuildBothImageAndDockerfile(t *testing.T) {
	body := `{"image":"alpine","dockerfile":"/Dockerfile","output_dir":"/tmp/out"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	New().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "exactly one") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleBuildNeitherImageNorDockerfile(t *testing.T) {
	body := `{"output_dir":"/tmp/out"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	New().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleBuildDockerfileError(t *testing.T) {
	srv := newTestServer(func(s *Server) {
		s.buildDockerfile = func(opts dockerfile.BuildOptions) (*dockerfile.BuildResult, error) {
			return nil, errors.New("dockerfile build failed")
		}
	})

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"dockerfile":"/tmp/Dockerfile","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dockerfile build failed") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleBuildImageWithCacheDir(t *testing.T) {
	srv := newTestServer(func(s *Server) {
		s.pullImage = func(opts oci.PullOptions) (*oci.PulledImage, error) {
			if !strings.Contains(opts.CacheDir, "custom-cache") {
				t.Fatalf("CacheDir = %q, want to contain custom-cache", opts.CacheDir)
			}
			return &oci.PulledImage{Config: oci.ImageConfig{}}, nil
		}
		s.extractToDir = func(_ *oci.PulledImage, dir string) error { return nil }
	})

	cacheDir := filepath.Join(t.TempDir(), "custom-cache")
	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"image":"alpine:latest","output_dir":` + jsonString(outputDir) + `,"cache_dir":` + jsonString(cacheDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestClearDirectoryEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := clearDirectory(dir); err != nil {
		t.Fatalf("clearDirectory(empty) = %v", err)
	}
}

func TestClearDirectoryNonexistent(t *testing.T) {
	if err := clearDirectory("/nonexistent/path/123"); err != nil {
		t.Fatalf("clearDirectory(missing) = %v", err)
	}
}

func TestNewClientAndBuildCancelled(t *testing.T) {
	client := NewClient("/nonexistent/socket")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Build(ctx, BuildRequest{OutputDir: "/tmp/out"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestServeHTTPSetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/build", nil)
	New().ServeHTTP(rec, req)
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

func TestHandleBuildExtractError(t *testing.T) {
	srv := newTestServer(func(s *Server) {
		s.pullImage = func(opts oci.PullOptions) (*oci.PulledImage, error) {
			return &oci.PulledImage{Config: oci.ImageConfig{}}, nil
		}
		s.extractToDir = func(_ *oci.PulledImage, dir string) error {
			return errors.New("extract failed")
		}
	})

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"image":"alpine:latest","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "extract failed") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleBuildClearsExistingOutput(t *testing.T) {
	srv := newTestServer(func(s *Server) {
		s.pullImage = func(opts oci.PullOptions) (*oci.PulledImage, error) {
			return &oci.PulledImage{Config: oci.ImageConfig{}}, nil
		}
		s.extractToDir = func(_ *oci.PulledImage, dir string) error { return nil }
	})

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	os.MkdirAll(outputDir, 0755)
	os.WriteFile(filepath.Join(outputDir, "old"), []byte("old"), 0644)

	body := `{"image":"alpine","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	// Old file should be gone
	if _, err := os.Stat(filepath.Join(outputDir, "old")); err == nil {
		t.Fatal("old file should have been cleared")
	}
}

func TestClientBuildInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer server.Close()
	client := &Client{httpClient: server.Client(), baseURL: server.URL}
	_, err := client.Build(context.Background(), BuildRequest{OutputDir: "/tmp/out"})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestBuildResponseJSON(t *testing.T) {
	resp := BuildResponse{RootfsDir: "/tmp/rootfs", Config: oci.ImageConfig{Env: []string{"A=B"}}}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "rootfs_dir") {
		t.Fatalf("JSON = %s", data)
	}
}

func TestListenUnixCreatesSocket(t *testing.T) {
	srv := New()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Start server in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenUnix(sockPath)
	}()
	// Ensure the server goroutine is cleaned up when the test ends by
	// closing the listener directly — removing the socket file does not
	// stop http.Serve.
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Error("ListenUnix goroutine did not exit within 2s")
		}
	})

	// Wait for socket to appear
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket %s did not appear within 3s", sockPath)
	}

	// Verify we can connect
	client := NewClient(sockPath)
	_, err := client.Build(context.Background(), BuildRequest{})
	// We expect an error (bad request) but the connection should work
	if err == nil {
		t.Log("unexpected success")
	}
}

func TestClearDirectoryWithSubdirs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "deep", "file.txt"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "root.txt"), []byte("y"), 0644)
	if err := clearDirectory(dir); err != nil {
		t.Fatalf("clearDirectory = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
}
