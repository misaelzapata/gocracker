//go:build darwin

package buildserver

import (
	"github.com/gocracker/gocracker/internal/buildbackend"
	"github.com/gocracker/gocracker/internal/buildkit"
)

func selectedBackend() buildbackend.Backend {
	return buildkit.NewBackend(buildbackend.NewDockerfileBackend())
}
