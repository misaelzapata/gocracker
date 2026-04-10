package container

import (
	"context"
	"reflect"
	"testing"

	"github.com/gocracker/gocracker/internal/buildbackend"
	"github.com/gocracker/gocracker/internal/oci"
)

type buildRootfsSpy struct {
	t   *testing.T
	got buildbackend.Request
}

func (s *buildRootfsSpy) BuildRootfs(_ context.Context, req buildbackend.Request) (*buildbackend.Result, error) {
	s.got = req
	return &buildbackend.Result{
		RootfsDir: req.OutputDir,
		Config: oci.ImageConfig{
			Entrypoint: []string{"/bin/sh"},
		},
	}, nil
}

func TestBuildRootfsForwardsBuildKitOptions(t *testing.T) {
	original := buildRootfsBackendFactory
	spy := &buildRootfsSpy{t: t}
	buildRootfsBackendFactory = func() buildbackend.Backend { return spy }
	t.Cleanup(func() { buildRootfsBackendFactory = original })

	rootfsDir := t.TempDir()
	cacheDir := t.TempDir()
	opts := RunOptions{
		Dockerfile:   "/tmp/Dockerfile",
		Context:      "/tmp/context",
		BuildArgs:    map[string]string{"HELLO": "world"},
		BuildSecrets: []string{"id=secret,src=/tmp/secret"},
		BuildSSH:     []string{"default=/tmp/ssh-agent.sock"},
		Target:       "release",
		Platform:     "linux/arm64",
		NoCache:      true,
		CacheDir:     cacheDir,
	}

	cfg, err := buildRootfs(rootfsDir, opts)
	if err != nil {
		t.Fatalf("buildRootfs() error = %v", err)
	}
	if !reflect.DeepEqual(cfg.Entrypoint, []string{"/bin/sh"}) {
		t.Fatalf("config = %#v, want entrypoint %#v", cfg, []string{"/bin/sh"})
	}
	if spy.got.Dockerfile != opts.Dockerfile {
		t.Fatalf("dockerfile = %q, want %q", spy.got.Dockerfile, opts.Dockerfile)
	}
	if spy.got.Context != opts.Context {
		t.Fatalf("context = %q, want %q", spy.got.Context, opts.Context)
	}
	if !reflect.DeepEqual(spy.got.BuildArgs, opts.BuildArgs) {
		t.Fatalf("build args = %#v, want %#v", spy.got.BuildArgs, opts.BuildArgs)
	}
	if !reflect.DeepEqual(spy.got.BuildSecrets, opts.BuildSecrets) {
		t.Fatalf("build secrets = %#v, want %#v", spy.got.BuildSecrets, opts.BuildSecrets)
	}
	if !reflect.DeepEqual(spy.got.BuildSSH, opts.BuildSSH) {
		t.Fatalf("build ssh = %#v, want %#v", spy.got.BuildSSH, opts.BuildSSH)
	}
	if spy.got.Target != opts.Target {
		t.Fatalf("target = %q, want %q", spy.got.Target, opts.Target)
	}
	if spy.got.Platform != opts.Platform {
		t.Fatalf("platform = %q, want %q", spy.got.Platform, opts.Platform)
	}
	if !spy.got.NoCache {
		t.Fatal("no_cache = false, want true")
	}
	if spy.got.OutputDir != rootfsDir {
		t.Fatalf("output dir = %q, want %q", spy.got.OutputDir, rootfsDir)
	}
	if spy.got.CacheDir != cacheDir {
		t.Fatalf("cache dir = %q, want %q", spy.got.CacheDir, cacheDir)
	}
}
