package buildserver

import (
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
	srv := newTestServer(func(s *Server) {
		s.pullImage = func(opts oci.PullOptions) (*oci.PulledImage, error) {
			return &oci.PulledImage{Config: oci.ImageConfig{Env: []string{"A=B"}}}, nil
		}
		s.extractToDir = func(_ *oci.PulledImage, dir string) error {
			return os.WriteFile(filepath.Join(dir, "ok"), []byte("ok"), 0o644)
		}
	})

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"image":"alpine:latest","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	srv.ServeHTTP(rec, req)

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
	srv := newTestServer(func(s *Server) {
		s.buildDockerfile = func(opts dockerfile.BuildOptions) (*dockerfile.BuildResult, error) {
			if opts.OutputDir == "" || opts.DockerfilePath == "" {
				t.Fatalf("opts = %+v", opts)
			}
			return &dockerfile.BuildResult{Config: oci.ImageConfig{WorkingDir: "/app"}}, nil
		}
	})

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	body := `{"dockerfile":"/tmp/Dockerfile","context":"/tmp","output_dir":` + jsonString(outputDir) + `}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build", strings.NewReader(body))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBuildReportsWorkerError(t *testing.T) {
	srv := newTestServer(func(s *Server) {
		s.pullImage = func(oci.PullOptions) (*oci.PulledImage, error) {
			return nil, errors.New("pull failed")
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
}

// Client round-trip is tested via TestListenUnixCreatesSocket in server_extra_test.go.

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
