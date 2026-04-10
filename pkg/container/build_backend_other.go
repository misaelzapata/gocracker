//go:build !darwin

package container

import "github.com/gocracker/gocracker/internal/buildbackend"

func selectedLocalBuildBackend() buildbackend.Backend {
	return buildbackend.NewDockerfileBackend()
}
