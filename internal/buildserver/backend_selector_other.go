//go:build !darwin

package buildserver

import "github.com/gocracker/gocracker/internal/buildbackend"

func selectedBackend() buildbackend.Backend {
	return buildbackend.NewDockerfileBackend()
}
