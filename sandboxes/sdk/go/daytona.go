package gocracker

// Daytona-style namespaces on *Sandbox so Go callers can write:
//
//	sb.Process().Exec(ctx, "python -c 'print(2+2)'")
//	sb.FS().WriteFile(ctx, "/tmp/x", []byte("hi"))
//	url, _ := sb.PreviewURL(ctx, 8080)
//
// These wrap the existing Toolbox() methods and MintPreview RPC so the
// surface matches the Python + JS SDKs (and, by extension, Daytona
// users' muscle memory). The flat ToolboxClient API keeps working —
// this is additive, not a breaking rename.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Additional typed errors for Daytona parity. Composed with the
// existing Error type via errors.Is — concrete returned values stay
// *Error so Status / Body remain accessible.
var (
	ErrPoolExhausted      = errors.New("sandboxd: pool exhausted")
	ErrRuntimeUnreachable = errors.New("sandboxd: runtime unreachable")
	ErrSandboxTimeout     = errors.New("sandboxd: operation timed out")
)

// ProcessExitError is returned by Sandbox.Process().Exec when the
// command exits non-zero. Carries the exit code plus captured
// stdout/stderr so callers can log or recover without re-running.
type ProcessExitError struct {
	ExitCode int32
	Stdout   []byte
	Stderr   []byte
}

func (e *ProcessExitError) Error() string {
	return fmt.Sprintf("process exited with code %d", e.ExitCode)
}

// ProcessNamespace wraps ToolboxClient with v2 / Daytona-shaped method
// names. Accessed via Sandbox.Process().
type ProcessNamespace struct {
	tb *ToolboxClient
}

// Exec runs cmd synchronously. A shell-string is wrapped with
// /bin/sh -c for ergonomics; caller-supplied arg slices are passed
// through unchanged. Non-zero exit codes surface as *ProcessExitError.
func (p *ProcessNamespace) Exec(ctx context.Context, cmd interface{}) (*ExecResult, error) {
	args, err := normalizeExecCmd(cmd)
	if err != nil {
		return nil, err
	}
	res, err := p.tb.Exec(ctx, args)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return res, &ProcessExitError{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}
	}
	return res, nil
}

// ExecStream streams stdout/stderr frames as the guest produces them.
// Mirrors v2's session.exec_stream. The returned channel closes when
// the process exits (or the context is canceled); an exit frame with
// ExitCode is the final value.
func (p *ProcessNamespace) ExecStream(ctx context.Context, cmd interface{}) (<-chan Frame, error) {
	args, err := normalizeExecCmd(cmd)
	if err != nil {
		return nil, err
	}
	return p.tb.ExecStream(ctx, args, ExecOptions{})
}

// Start launches cmd and returns the stream without waiting for exit —
// useful for long-running daemons where the caller drives the frame
// iterator in its own goroutine. Currently the same as ExecStream;
// the distinction exists so v2's Session.Start → Session.Wait pattern
// maps cleanly when agent-side process control lands.
func (p *ProcessNamespace) Start(ctx context.Context, cmd interface{}) (<-chan Frame, error) {
	return p.ExecStream(ctx, cmd)
}

func normalizeExecCmd(cmd interface{}) ([]string, error) {
	switch v := cmd.(type) {
	case string:
		return []string{"/bin/sh", "-c", v}, nil
	case []string:
		return v, nil
	default:
		return nil, fmt.Errorf("exec: cmd must be string or []string, got %T", cmd)
	}
}

// FSNamespace wraps ToolboxClient's file ops with Daytona-shaped
// method names. Accessed via Sandbox.FS().
type FSNamespace struct {
	tb *ToolboxClient
}

func (f *FSNamespace) WriteFile(ctx context.Context, path string, data []byte) error {
	return f.tb.Upload(ctx, path, data)
}

func (f *FSNamespace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return f.tb.Download(ctx, path)
}

func (f *FSNamespace) ListDir(ctx context.Context, path string) ([]FileEntry, error) {
	return f.tb.ListFiles(ctx, path)
}

func (f *FSNamespace) Remove(ctx context.Context, path string) error {
	return f.tb.DeleteFile(ctx, path)
}

func (f *FSNamespace) Mkdir(ctx context.Context, path string) error {
	return f.tb.Mkdir(ctx, path, true)
}

func (f *FSNamespace) Chmod(ctx context.Context, path string, mode uint32) error {
	return f.tb.Chmod(ctx, path, mode)
}

func (f *FSNamespace) Rename(ctx context.Context, src, dst string) error {
	return f.tb.Rename(ctx, src, dst)
}

// Process returns the Daytona-style process namespace for this
// sandbox. Returns nil if the sandbox has no UDS path (unready state).
func (s *Sandbox) Process() *ProcessNamespace {
	tb := s.Toolbox()
	if tb == nil {
		return nil
	}
	return &ProcessNamespace{tb: tb}
}

// FS returns the Daytona-style file-system namespace for this sandbox.
// Returns nil if the sandbox has no UDS path (unready state).
func (s *Sandbox) FS() *FSNamespace {
	tb := s.Toolbox()
	if tb == nil {
		return nil
	}
	return &FSNamespace{tb: tb}
}

// PreviewURL returns a signed preview URL for a guest-side port.
// Matches Daytona's `sandbox.preview_link(port)` shape. The URL is
// absolute (includes scheme + host + the `/previews/<token>/` path)
// and usable directly with any HTTP client.
//
// Returns an error if the sandbox has no owning Client (e.g. it was
// constructed directly without going through Client.CreateSandbox /
// Client.LeaseSandbox).
func (s *Sandbox) PreviewURL(ctx context.Context, port uint16) (string, error) {
	if s.client == nil {
		return "", fmt.Errorf("sandbox has no client; call client.MintPreview(ctx, id, port)")
	}
	preview, err := s.client.MintPreview(ctx, s.ID, port)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s.client.BaseURL, "/") + preview.URL, nil
}
