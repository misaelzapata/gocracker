//go:build darwin

package container

import (
	"testing"

	"github.com/gocracker/gocracker/internal/buildkit"
)

func TestSelectedLocalBuildBackendUsesDarwinBuildKitWrapper(t *testing.T) {
	backend := selectedLocalBuildBackend()
	if backend == nil {
		t.Fatal("selectedLocalBuildBackend() returned nil")
	}
	if _, ok := backend.(*buildkit.Backend); !ok {
		t.Fatalf("selectedLocalBuildBackend() = %T, want *buildkit.Backend", backend)
	}
}
