package buildserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gocracker/gocracker/internal/buildbackend"
	"github.com/gocracker/gocracker/internal/oci"
)

type spyBackend struct {
	t   *testing.T
	got buildbackend.Request
}

func (s *spyBackend) BuildRootfs(_ context.Context, req buildbackend.Request) (*buildbackend.Result, error) {
	s.got = req
	return &buildbackend.Result{
		RootfsDir: req.OutputDir,
		Config: oci.ImageConfig{
			Entrypoint: []string{"/bin/sh"},
			Cmd:        []string{"-c", "echo ok"},
		},
	}, nil
}

func TestHandleBuildForwardsBuildOptions(t *testing.T) {
	original := buildBackendFactory
	backend := &spyBackend{t: t}
	buildBackendFactory = func() buildbackend.Backend { return backend }
	t.Cleanup(func() { buildBackendFactory = original })

	srv := New()
	outDir := t.TempDir()
	reqBody := BuildRequest{
		Dockerfile:   filepath.Join(t.TempDir(), "Dockerfile"),
		Context:      filepath.Join(t.TempDir(), "context"),
		BuildArgs:    map[string]string{"HELLO": "world"},
		BuildSecrets: []string{"id=secret,src=/tmp/secret"},
		BuildSSH:     []string{"default=/tmp/agent.sock"},
		Target:       "release",
		Platform:     "linux/arm64",
		NoCache:      true,
		OutputDir:    outDir,
		CacheDir:     filepath.Join(t.TempDir(), "cache"),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/build", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if backend.got.Dockerfile != reqBody.Dockerfile {
		t.Fatalf("dockerfile = %q, want %q", backend.got.Dockerfile, reqBody.Dockerfile)
	}
	if backend.got.Context != reqBody.Context {
		t.Fatalf("context = %q, want %q", backend.got.Context, reqBody.Context)
	}
	if !reflect.DeepEqual(backend.got.BuildArgs, reqBody.BuildArgs) {
		t.Fatalf("build args = %#v, want %#v", backend.got.BuildArgs, reqBody.BuildArgs)
	}
	if !reflect.DeepEqual(backend.got.BuildSecrets, reqBody.BuildSecrets) {
		t.Fatalf("build secrets = %#v, want %#v", backend.got.BuildSecrets, reqBody.BuildSecrets)
	}
	if !reflect.DeepEqual(backend.got.BuildSSH, reqBody.BuildSSH) {
		t.Fatalf("build ssh = %#v, want %#v", backend.got.BuildSSH, reqBody.BuildSSH)
	}
	if backend.got.Target != reqBody.Target {
		t.Fatalf("target = %q, want %q", backend.got.Target, reqBody.Target)
	}
	if backend.got.Platform != reqBody.Platform {
		t.Fatalf("platform = %q, want %q", backend.got.Platform, reqBody.Platform)
	}
	if !backend.got.NoCache {
		t.Fatal("no_cache = false, want true")
	}
	if backend.got.OutputDir != reqBody.OutputDir {
		t.Fatalf("output dir = %q, want %q", backend.got.OutputDir, reqBody.OutputDir)
	}
	if backend.got.CacheDir != reqBody.CacheDir {
		t.Fatalf("cache dir = %q, want %q", backend.got.CacheDir, reqBody.CacheDir)
	}

	var resp BuildResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.RootfsDir != reqBody.OutputDir {
		t.Fatalf("response rootfs = %q, want %q", resp.RootfsDir, reqBody.OutputDir)
	}
}
