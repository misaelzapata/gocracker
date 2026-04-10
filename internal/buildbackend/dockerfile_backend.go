package buildbackend

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/gocracker/gocracker/internal/dockerfile"
	"github.com/gocracker/gocracker/internal/oci"
)

type DockerfileBackend struct{}

func NewDockerfileBackend() Backend {
	return DockerfileBackend{}
}

func (DockerfileBackend) BuildRootfs(_ context.Context, req Request) (*Result, error) {
	if (req.Image == "") == (req.Dockerfile == "") {
		return nil, fmt.Errorf("exactly one of image or dockerfile is required")
	}
	if req.OutputDir == "" {
		return nil, fmt.Errorf("output_dir is required")
	}

	switch {
	case req.Image != "":
		pulled, err := oci.Pull(oci.PullOptions{
			Ref:      req.Image,
			CacheDir: filepath.Join(req.CacheDir, "layers"),
		})
		if err != nil {
			return nil, err
		}
		if err := pulled.ExtractToDir(req.OutputDir); err != nil {
			return nil, err
		}
		return &Result{
			RootfsDir: req.OutputDir,
			Config:    pulled.Config,
		}, nil
	default:
		result, err := dockerfile.Build(dockerfile.BuildOptions{
			DockerfilePath: req.Dockerfile,
			ContextDir:     req.Context,
			BuildArgs:      req.BuildArgs,
			BuildSecrets:   append([]string(nil), req.BuildSecrets...),
			BuildSSH:       append([]string(nil), req.BuildSSH...),
			Target:         req.Target,
			Platform:       req.Platform,
			NoCache:        req.NoCache,
			OutputDir:      req.OutputDir,
			CacheDir:       req.CacheDir,
		})
		if err != nil {
			return nil, err
		}
		return &Result{
			RootfsDir: req.OutputDir,
			Config:    result.Config,
		}, nil
	}
}
