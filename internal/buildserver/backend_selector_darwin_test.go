//go:build darwin

package buildserver

import (
	"testing"

	"github.com/gocracker/gocracker/internal/buildkit"
)

func TestSelectedBackendUsesDarwinBuildKitWrapper(t *testing.T) {
	backend := selectedBackend()
	if backend == nil {
		t.Fatal("selectedBackend() returned nil")
	}
	if _, ok := backend.(*buildkit.Backend); !ok {
		t.Fatalf("selectedBackend() = %T, want *buildkit.Backend", backend)
	}
}
