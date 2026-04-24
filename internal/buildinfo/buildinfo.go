// Package buildinfo exposes the build-time identity of the binary:
// a semver Version, the git commit it was built from, and the build
// date. Each field is injected via -ldflags at build time; bare `go
// build` leaves the defaults in place so development binaries still
// produce a readable `gocracker version` output.
//
// Wire it into a binary via:
//
//	go build -ldflags "\
//	  -X github.com/gocracker/gocracker/internal/buildinfo.Version=$(VERSION) \
//	  -X github.com/gocracker/gocracker/internal/buildinfo.Commit=$(COMMIT) \
//	  -X github.com/gocracker/gocracker/internal/buildinfo.Date=$(DATE)"
//
// The Makefile `build` target does this automatically from git state.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// These vars are set at build time via -ldflags. The defaults make
// `go build ./...` (no ldflags) still produce a sane --version output
// for dev binaries — you get "dev" + the module's VCS stamp instead
// of a hardcoded string that would lie about what was built.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a single-line version stamp suitable for `--version`
// output. Format: "gocracker <version> (<commit>, <date>) <go version> <os>/<arch>".
// If Commit is still the default and the binary was built with Go's
// built-in VCS stamping (module mode, no -buildvcs=false), we pull
// the real commit from runtime/debug so dev binaries aren't totally
// opaque.
func String() string {
	commit := Commit
	date := Date
	if commit == "unknown" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if len(s.Value) >= 7 {
						commit = s.Value[:7]
					}
				case "vcs.time":
					date = s.Value
				}
			}
		}
	}
	return fmt.Sprintf("gocracker %s (%s, %s) %s %s/%s",
		Version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
