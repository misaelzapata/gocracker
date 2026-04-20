//go:build !linux

package agent

import (
	"fmt"
	"net/http"
)

// handleSetNetwork on non-Linux is a stub — the agent only ever runs
// inside a Linux guest, but the package compiles on macOS / other
// platforms so host-side tests/imports work. Returning 501 instead
// of panicking lets callers detect the missing capability.
func handleSetNetwork(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, fmt.Errorf("setnetwork: only supported on linux guests"))
}
