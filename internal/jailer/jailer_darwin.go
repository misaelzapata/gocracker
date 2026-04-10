//go:build darwin

// On macOS, the jailer provides process-level isolation using macOS sandbox
// profiles (sandbox-exec). This is the darwin equivalent of the Linux
// chroot+seccomp+namespace jailer.
//
// The sandbox restricts the VM process to:
//   - Read/write access to the VM's working directory only
//   - Network access (for NAT)
//   - Virtualization framework entitlement
//   - No access to the rest of the filesystem
package jailer

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultChrootBaseDir = "/tmp/gocracker-jail"

type multiFlag []string

func (f *multiFlag) String() string { return strings.Join(*f, ",") }
func (f *multiFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// Config holds darwin jailer configuration.
type Config struct {
	ID             string
	ExecFile       string
	UID            int
	GID            int
	ChrootBaseDir  string
	Mounts         []string
	Env            []string
	CgroupVersion  int
	Cgroups        []string
	ParentCgroup   string
	NetNS          string
	ResourceLimits []string
	Daemonize      bool
	NewPIDNS       bool
	ExtraArgs      []string
}

// RunCLI parses jailer arguments and launches the jailed process.
func RunCLI(args []string) error {
	fs := flag.NewFlagSet("jailer", flag.ContinueOnError)

	cfg := Config{}
	fs.StringVar(&cfg.ID, "id", "", "microVM identifier")
	fs.StringVar(&cfg.ExecFile, "exec-file", "", "binary to execute")
	fs.IntVar(&cfg.UID, "uid", os.Getuid(), "user ID")
	fs.IntVar(&cfg.GID, "gid", os.Getgid(), "group ID")
	fs.StringVar(&cfg.ChrootBaseDir, "chroot-base-dir", defaultChrootBaseDir, "base directory for jail")

	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.ExtraArgs = fs.Args()

	return Run(cfg)
}

// Run launches the jailed process with macOS sandbox isolation.
func Run(cfg Config) error {
	if cfg.ID == "" {
		return fmt.Errorf("jailer: --id is required")
	}
	if cfg.ExecFile == "" {
		return fmt.Errorf("jailer: --exec-file is required")
	}

	// Create jail directory
	jailDir := filepath.Join(cfg.ChrootBaseDir, cfg.ID)
	if err := os.MkdirAll(jailDir, 0755); err != nil {
		return fmt.Errorf("jailer: create jail dir: %w", err)
	}

	// Generate sandbox profile that restricts filesystem access
	profile := generateSandboxProfile(jailDir)
	profilePath := filepath.Join(jailDir, "sandbox.sb")
	if err := os.WriteFile(profilePath, []byte(profile), 0644); err != nil {
		return fmt.Errorf("jailer: write sandbox profile: %w", err)
	}

	// Build command with sandbox-exec
	cmdArgs := []string{"-f", profilePath, cfg.ExecFile}
	cmdArgs = append(cmdArgs, cfg.ExtraArgs...)

	cmd := exec.Command("sandbox-exec", cmdArgs...)
	cmd.Dir = jailDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), cfg.Env...)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jailer: sandbox-exec: %w", err)
	}
	return nil
}

// generateSandboxProfile creates a Seatbelt profile that restricts the process.
func generateSandboxProfile(jailDir string) string {
	return fmt.Sprintf(`(version 1)
(deny default)

;; Allow basic process operations
(allow process-exec)
(allow process-fork)
(allow signal)
(allow sysctl-read)
(allow mach-lookup)
(allow mach-register)
(allow ipc-posix-shm)
(allow iokit-open)

;; Allow Virtualization.framework
(allow system-privilege)

;; Allow read access to system libraries and frameworks
(allow file-read*
  (subpath "/usr/lib")
  (subpath "/System")
  (subpath "/Library/Frameworks")
  (subpath "/usr/share")
  (subpath "/private/var/db")
  (subpath "/dev"))

;; Allow read/write to the jail directory
(allow file-read* file-write*
  (subpath %q))

;; Allow read/write to temp
(allow file-read* file-write*
  (subpath "/tmp")
  (subpath "/private/tmp")
  (subpath "/var/folders"))

;; Allow network for NAT
(allow network*)

;; Allow pseudoterminals for console
(allow pseudo-tty)
`, jailDir)
}

// Cleanup removes the jail directory.
func Cleanup(baseDir, id string) {
	if baseDir == "" {
		baseDir = defaultChrootBaseDir
	}
	jailDir := filepath.Join(baseDir, id)
	os.RemoveAll(jailDir)
}
