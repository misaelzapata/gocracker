package sharedfs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Backend struct {
	cmd       *exec.Cmd
	socketDir string
	socket    string
	stderr    bytes.Buffer
}

// Attach returns a Backend that wraps an already-listening virtiofsd unix
// socket without starting a new process. Used when virtiofsd is spawned on
// the host and the consumer (a jailed VMM) only needs the socket path.
func Attach(socketPath string) *Backend {
	return &Backend{socket: socketPath}
}

// StartAt is like Start but writes the unix socket at the caller-provided
// path instead of inside a fresh tempdir. Used by the worker to place the
// socket inside the run dir that is bind-mounted into the jail.
func StartAt(sharedDir, tag, socketPath string) (*Backend, error) {
	binary, err := findVirtioFSD()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)
	sandbox := preferredSandboxMode()
	cmd := exec.Command(binary,
		"--socket-path", socketPath,
		"--shared-dir", sharedDir,
		"--tag", tag,
		"--sandbox", sandbox,
		"--cache", "never",
		"--log-level", "error",
	)
	backend := &Backend{
		cmd:    cmd,
		socket: socketPath,
		// socketDir intentionally empty: the run dir is owned by the caller
	}
	cmd.Stdout = &backend.stderr
	cmd.Stderr = &backend.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := waitForSocket(cmd, socketPath, 5*time.Second); err != nil {
		if output := strings.TrimSpace(backend.ErrorOutput()); output != "" {
			err = fmt.Errorf("%w: %s", err, output)
		}
		_ = backend.Close()
		return nil, err
	}
	return backend, nil
}

func Start(sharedDir, tag string) (*Backend, error) {
	binary, err := findVirtioFSD()
	if err != nil {
		return nil, err
	}
	socketDir, err := os.MkdirTemp("", "gocracker-virtiofsd-*")
	if err != nil {
		return nil, err
	}
	socketPath := filepath.Join(socketDir, "sock")
	sandbox := preferredSandboxMode()
	cmd := exec.Command(binary,
		"--socket-path", socketPath,
		"--shared-dir", sharedDir,
		"--tag", tag,
		"--sandbox", sandbox,
		"--cache", "never",
		"--log-level", "error",
	)
	backend := &Backend{
		cmd:       cmd,
		socketDir: socketDir,
		socket:    socketPath,
	}
	cmd.Stdout = &backend.stderr
	cmd.Stderr = &backend.stderr
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	if err := waitForSocket(cmd, socketPath, 5*time.Second); err != nil {
		if output := strings.TrimSpace(backend.ErrorOutput()); output != "" {
			err = fmt.Errorf("%w: %s", err, output)
		}
		_ = backend.Close()
		return nil, err
	}
	return backend, nil
}

func (b *Backend) SocketPath() string {
	if b == nil {
		return ""
	}
	return b.socket
}

func (b *Backend) Close() error {
	if b == nil {
		return nil
	}
	var err error
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() {
			done <- b.cmd.Wait()
		}()
		select {
		case waitErr := <-done:
			var exitErr *exec.ExitError
			if waitErr != nil && !errors.As(waitErr, &exitErr) {
				err = waitErr
			}
		case <-time.After(2 * time.Second):
			_ = b.cmd.Process.Kill()
			waitErr := <-done
			var exitErr *exec.ExitError
			if waitErr != nil && !errors.As(waitErr, &exitErr) {
				err = waitErr
			}
		}
	}
	if b.socketDir != "" {
		_ = os.RemoveAll(b.socketDir)
	}
	if err != nil {
		if output := strings.TrimSpace(b.stderr.String()); output != "" {
			return fmt.Errorf("%w: %s", err, output)
		}
	}
	return err
}

func (b *Backend) ErrorOutput() string {
	if b == nil {
		return ""
	}
	return b.stderr.String()
}

func findVirtioFSD() (string, error) {
	for _, candidate := range []string{
		"virtiofsd",
		"/usr/libexec/virtiofsd",
		"/usr/lib/qemu/virtiofsd",
	} {
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("virtiofsd not found")
}

func preferredSandboxMode() string {
	if os.Geteuid() == 0 {
		return "chroot"
	}
	if namespaceSandboxAvailable() {
		return "namespace"
	}
	return "none"
}

func namespaceSandboxAvailable() bool {
	if _, err := exec.LookPath("newuidmap"); err != nil {
		return false
	}
	if _, err := exec.LookPath("newgidmap"); err != nil {
		return false
	}
	data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(data)) != "0"
}

func waitForSocket(cmd *exec.Cmd, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		if cmd.Process != nil && cmd.Process.Signal(syscall.Signal(0)) != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cmd.Process != nil && cmd.Process.Signal(syscall.Signal(0)) != nil {
		return fmt.Errorf("virtiofsd exited before socket became ready")
	}
	return fmt.Errorf("virtiofsd socket %s was not created in time", socketPath)
}
