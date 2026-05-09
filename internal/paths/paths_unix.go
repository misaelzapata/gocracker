//go:build !windows

package paths

// Linux/Darwin defaults. These are byte-identical to the literals the
// codebase shipped before the paths package existed; changing them would
// break operators who scripted against the old locations.

func apiSocket() string     { return "/tmp/gocracker.sock" }
func vmmSocket() string     { return "/tmp/gocracker-vmm.sock" }
func buildSocket() string   { return "/tmp/gocracker-build.sock" }
func serveStateDir() string { return "/tmp/gocracker-serve-state" }
func snapshotsDir() string  { return "/tmp/gocracker-snapshots" }
func jailerBaseDir() string { return "/srv/jailer" }
func toolboxLog() string    { return "/tmp/gocracker-toolbox.log" }
func gitConfigGlobal() string {
	return "/tmp/gocracker-gitconfig"
}
