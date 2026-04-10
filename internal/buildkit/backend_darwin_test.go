//go:build darwin

package buildkit

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gocracker/gocracker/internal/buildbackend"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/session/sshforward/sshprovider"
)

type forwardingBackend struct {
	got buildbackend.Request
}

func (f *forwardingBackend) BuildRootfs(_ context.Context, req buildbackend.Request) (*buildbackend.Result, error) {
	f.got = req
	return &buildbackend.Result{
		RootfsDir: req.OutputDir,
		Config: oci.ImageConfig{
			Entrypoint: []string{"/bin/true"},
		},
	}, nil
}

func TestBackendFallsBackForImageBuilds(t *testing.T) {
	fallback := &forwardingBackend{}
	backend := NewBackend(fallback)

	req := buildbackend.Request{
		Image:     "alpine:3.20",
		OutputDir: t.TempDir(),
		CacheDir:  t.TempDir(),
	}

	got, err := backend.BuildRootfs(context.Background(), req)
	if err != nil {
		t.Fatalf("BuildRootfs() error = %v", err)
	}
	if got.RootfsDir != req.OutputDir {
		t.Fatalf("rootfs dir = %q, want %q", got.RootfsDir, req.OutputDir)
	}
	if !reflect.DeepEqual(got.Config.Entrypoint, []string{"/bin/true"}) {
		t.Fatalf("config = %#v, want entrypoint %#v", got.Config, []string{"/bin/true"})
	}
	if fallback.got.Image != req.Image {
		t.Fatalf("fallback image = %q, want %q", fallback.got.Image, req.Image)
	}
}

func TestBuildFrontendAttrs(t *testing.T) {
	attrs := buildFrontendAttrs(buildbackend.Request{
		BuildArgs: map[string]string{
			"B": "2",
			"A": "1",
		},
		Target:   "release",
		Platform: "linux/arm64",
		NoCache:  true,
	}, "Dockerfile.custom")

	want := map[string]string{
		"filename":    "Dockerfile.custom",
		"build-arg:A": "1",
		"build-arg:B": "2",
		"target":      "release",
		"platform":    "linux/arm64",
		"no-cache":    "",
	}
	if !reflect.DeepEqual(attrs, want) {
		t.Fatalf("attrs = %#v, want %#v", attrs, want)
	}
}

func TestParseSecretSpecs(t *testing.T) {
	got, err := parseSecretSpecs([]string{
		"id=mysecret,src=/tmp/secret",
		"type=env,id=fromenv,env=MY_ENV_SECRET",
	})
	if err != nil {
		t.Fatalf("parseSecretSpecs() error = %v", err)
	}
	want := []secretsprovider.Source{
		{ID: "mysecret", FilePath: "/tmp/secret"},
		{ID: "fromenv", Env: "MY_ENV_SECRET"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSecretSpecs() = %#v, want %#v", got, want)
	}
}

func TestParseSSHSpecs(t *testing.T) {
	got, err := parseSSHSpecs([]string{
		"default=/tmp/agent.sock",
		"other",
	})
	if err != nil {
		t.Fatalf("parseSSHSpecs() error = %v", err)
	}
	want := []sshprovider.AgentConfig{
		{ID: "default", Paths: []string{"/tmp/agent.sock"}},
		{ID: "other"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSSHSpecs() = %#v, want %#v", got, want)
	}
}

func TestResolveBuilderKernelPathFromEnv(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "kernel")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel fixture: %v", err)
	}
	prev := os.Getenv(builderKernelEnvKey)
	t.Cleanup(func() {
		_ = os.Setenv(builderKernelEnvKey, prev)
	})
	if err := os.Setenv(builderKernelEnvKey, kernel); err != nil {
		t.Fatalf("set env: %v", err)
	}
	got, err := resolveBuilderKernelPath()
	if err != nil {
		t.Fatalf("resolveBuilderKernelPath() error = %v", err)
	}
	if got != kernel {
		t.Fatalf("resolveBuilderKernelPath() = %q, want %q", got, kernel)
	}
}
