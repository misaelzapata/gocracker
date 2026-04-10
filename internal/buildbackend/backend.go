package buildbackend

import (
	"context"

	"github.com/gocracker/gocracker/internal/oci"
)

type Request struct {
	Image        string
	Dockerfile   string
	Context      string
	BuildArgs    map[string]string
	BuildSecrets []string
	BuildSSH     []string
	Target       string
	Platform     string
	NoCache      bool
	OutputDir    string
	CacheDir     string
}

type Result struct {
	RootfsDir string
	Config    oci.ImageConfig
}

type Backend interface {
	BuildRootfs(ctx context.Context, req Request) (*Result, error)
}
