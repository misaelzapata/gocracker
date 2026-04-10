//go:build darwin

package container

import (
	"github.com/gocracker/gocracker/internal/buildbackend"
	"github.com/gocracker/gocracker/internal/buildkit"
)

func selectedLocalBuildBackend() buildbackend.Backend {
	return buildkit.NewBackend(buildbackend.NewDockerfileBackend())
}
