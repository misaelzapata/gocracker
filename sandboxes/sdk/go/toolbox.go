package gocracker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

// ToolboxClient talks to the in-guest toolbox agent over a per-sandbox
// UDS + Firecracker-style CONNECT handshake. Stateless — every call
// dials a fresh socket to match the agent's Connection: close
// response shape. Thread-safe (no shared per-client state).
type ToolboxClient struct {
	UDSPath     string
	Port        uint32        // default 10023
	DialTimeout time.Duration // default 5s
}

// TBChannel values mirror internal/toolbox/agent.Channel*.
const (
	TBChannelStdin  = 0
	TBChannelStdout = 1
	TBChannelStderr = 2
	TBChannelExit   = 3
	TBChannelSignal = 4
)

// NewToolboxClient builds a ToolboxClient with default port + timeout.
func NewToolboxClient(udsPath string) *ToolboxClient {
	return &ToolboxClient{UDSPath: udsPath, Port: 10023, DialTimeout: 5 * time.Second}
}

// ToolboxError is returned by every toolbox-agent call.
type ToolboxError struct{ Message string }

func (e *ToolboxError) Error() string { return e.Message }

// ExecResult captures stdout/stderr/exit from a blocking Exec.
type ExecResult struct {
	ExitCode int32
	Stdout   []byte
	Stderr   []byte
}

// ExecOptions extends Exec call with env / workdir / stdin.
type ExecOptions struct {
	Env     []string
	WorkDir string
	Stdin   []byte
	Timeout time.Duration
}

// Exec runs a command in the guest and collects all stdout/stderr/exit.
// Blocking; for streaming use ExecStream.
func (t *ToolboxClient) Exec(ctx context.Context, cmd []string, opts ...ExecOptions) (*ExecResult, error) {
	o := ExecOptions{Timeout: 30 * time.Second}
	if len(opts) > 0 {
		o = opts[0]
		if o.Timeout == 0 {
			o.Timeout = 30 * time.Second
		}
	}
	result := &ExecResult{ExitCode: -1}
	frames, err := t.ExecStream(ctx, cmd, o)
	if err != nil {
		return nil, err
	}
	for f := range frames {
		switch f.Channel {
		case TBChannelStdout:
			result.Stdout = append(result.Stdout, f.Payload...)
		case TBChannelStderr:
			result.Stderr = append(result.Stderr, f.Payload...)
		case TBChannelExit:
			if len(f.Payload) >= 4 {
				result.ExitCode = int32(binary.BigEndian.Uint32(f.Payload[:4]))
			}
		case TBChannelSignal:
			// Control frame — ignore for Exec aggregation.
		}
	}
	return result, nil
}

// Frame is one message from ExecStream.
type Frame struct {
	Channel byte
	Payload []byte
}

// ExecStream returns a channel of Frames that closes when the exec
// finishes. Callers drain it to implement line-by-line streaming or
// interactive I/O. Errors during the setup phase return immediately;
// errors mid-stream close the channel.
func (t *ToolboxClient) ExecStream(ctx context.Context, cmd []string, opts ExecOptions) (<-chan Frame, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	body := map[string]any{"cmd": cmd}
	if len(opts.Env) > 0 {
		body["env"] = opts.Env
	}
	if opts.WorkDir != "" {
		body["workdir"] = opts.WorkDir
	}
	bodyBytes, _ := json.Marshal(body)

	sock, err := t.dialConnect(ctx)
	if err != nil {
		return nil, err
	}
	_ = sock.SetDeadline(time.Now().Add(opts.Timeout))

	req := fmt.Sprintf(
		"POST /exec HTTP/1.0\r\nHost: x\r\nContent-Length: %d\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n",
		len(bodyBytes),
	)
	if _, err := sock.Write([]byte(req)); err != nil {
		sock.Close()
		return nil, &ToolboxError{Message: "exec: write headers: " + err.Error()}
	}
	if _, err := sock.Write(bodyBytes); err != nil {
		sock.Close()
		return nil, &ToolboxError{Message: "exec: write body: " + err.Error()}
	}
	if len(opts.Stdin) > 0 {
		if err := writeFrame(sock, TBChannelStdin, opts.Stdin); err != nil {
			sock.Close()
			return nil, err
		}
		if err := writeFrame(sock, TBChannelStdin, nil); err != nil {
			sock.Close()
			return nil, err
		}
	}

	br := bufio.NewReader(sock)
	if _, err := br.ReadString('\n'); err != nil {
		sock.Close()
		return nil, &ToolboxError{Message: "exec: status: " + err.Error()}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			sock.Close()
			return nil, &ToolboxError{Message: "exec: headers: " + err.Error()}
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	out := make(chan Frame, 4)
	go func() {
		defer close(out)
		defer sock.Close()
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(br, hdr[:]); err != nil {
				return
			}
			channel := hdr[0]
			n := binary.BigEndian.Uint32(hdr[1:5])
			var payload []byte
			if n > 0 {
				payload = make([]byte, n)
				if _, err := io.ReadFull(br, payload); err != nil {
					return
				}
			}
			select {
			case out <- Frame{Channel: channel, Payload: payload}:
			case <-ctx.Done():
				return
			}
			if channel == TBChannelExit {
				return
			}
		}
	}()
	return out, nil
}

// ---- Files ------------------------------------------------------

// FileEntry is one entry from ListFiles.
type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Kind  string `json:"kind"`  // "file" | "dir"
	Size  int64  `json:"size"`
}

// IsDir is a convenience wrapper over Kind == "dir".
func (e FileEntry) IsDir() bool { return e.Kind == "dir" }

// ListFiles returns the entries under the guest path.
func (t *ToolboxClient) ListFiles(ctx context.Context, path string) ([]FileEntry, error) {
	status, _, body, err := t.httpReq(ctx, "GET", "/files?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &ToolboxError{Message: fmt.Sprintf("list_files: status=%d", status)}
	}
	var resp struct {
		Entries []FileEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &ToolboxError{Message: "list_files: decode: " + err.Error()}
	}
	return resp.Entries, nil
}

// Download returns the raw bytes of a guest file.
func (t *ToolboxClient) Download(ctx context.Context, path string) ([]byte, error) {
	status, _, body, err := t.httpReq(ctx, "GET", "/files/download?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &ToolboxError{Message: fmt.Sprintf("download: status=%d", status)}
	}
	return body, nil
}

// Upload writes data to a guest file.
func (t *ToolboxClient) Upload(ctx context.Context, path string, data []byte) error {
	status, _, _, err := t.httpReq(ctx, "POST", "/files/upload?path="+url.QueryEscape(path), data, "application/octet-stream")
	if err != nil {
		return err
	}
	if status != 200 && status != 201 {
		return &ToolboxError{Message: fmt.Sprintf("upload: status=%d", status)}
	}
	return nil
}

// DeleteFile removes a guest file.
func (t *ToolboxClient) DeleteFile(ctx context.Context, path string) error {
	status, _, _, err := t.httpReq(ctx, "DELETE", "/files?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return &ToolboxError{Message: fmt.Sprintf("delete_file: status=%d", status)}
	}
	return nil
}

// Mkdir creates a directory. parents=true is mkdir -p.
func (t *ToolboxClient) Mkdir(ctx context.Context, path string, parents bool) error {
	body, _ := json.Marshal(map[string]any{"path": path, "all": parents})
	status, _, _, err := t.httpReq(ctx, "POST", "/files/mkdir", body, "application/json")
	if err != nil {
		return err
	}
	if status != 200 {
		return &ToolboxError{Message: fmt.Sprintf("mkdir: status=%d", status)}
	}
	return nil
}

// Rename moves a guest file or directory.
func (t *ToolboxClient) Rename(ctx context.Context, src, dst string) error {
	body, _ := json.Marshal(map[string]any{"old_path": src, "new_path": dst})
	status, _, _, err := t.httpReq(ctx, "POST", "/files/rename", body, "application/json")
	if err != nil {
		return err
	}
	if status != 200 {
		return &ToolboxError{Message: fmt.Sprintf("rename: status=%d", status)}
	}
	return nil
}

// Chmod changes a guest file's mode bits.
func (t *ToolboxClient) Chmod(ctx context.Context, path string, mode uint32) error {
	body, _ := json.Marshal(map[string]any{"path": path, "mode": mode})
	status, _, _, err := t.httpReq(ctx, "POST", "/files/chmod", body, "application/json")
	if err != nil {
		return err
	}
	if status != 200 {
		return &ToolboxError{Message: fmt.Sprintf("chmod: status=%d", status)}
	}
	return nil
}

// ---- Git --------------------------------------------------------

// GitClone clones repository into directory, optionally checking out ref.
func (t *ToolboxClient) GitClone(ctx context.Context, repository, directory, ref string) (map[string]any, error) {
	b := map[string]any{"repository": repository, "directory": directory}
	if ref != "" {
		b["ref"] = ref
	}
	body, _ := json.Marshal(b)
	status, _, respBody, err := t.httpReq(ctx, "POST", "/git/clone", body, "application/json")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &ToolboxError{Message: fmt.Sprintf("git_clone: status=%d body=%s", status, string(respBody))}
	}
	var out map[string]any
	_ = json.Unmarshal(respBody, &out)
	return out, nil
}

// GitStatus runs `git -C directory status --porcelain`.
func (t *ToolboxClient) GitStatus(ctx context.Context, directory string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"directory": directory})
	status, _, respBody, err := t.httpReq(ctx, "POST", "/git/status", body, "application/json")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &ToolboxError{Message: fmt.Sprintf("git_status: status=%d", status)}
	}
	var out map[string]any
	_ = json.Unmarshal(respBody, &out)
	return out, nil
}

// ---- Secrets ----------------------------------------------------

// SetSecret stores a name=value pair in the per-sandbox in-memory store.
func (t *ToolboxClient) SetSecret(ctx context.Context, name, value string) error {
	body, _ := json.Marshal(map[string]any{"name": name, "value": value})
	status, _, _, err := t.httpReq(ctx, "POST", "/secrets", body, "application/json")
	if err != nil {
		return err
	}
	if status != 200 && status != 201 {
		return &ToolboxError{Message: fmt.Sprintf("set_secret: status=%d", status)}
	}
	return nil
}

// ListSecrets returns the names of stored secrets (values never leave
// the sandbox).
func (t *ToolboxClient) ListSecrets(ctx context.Context) ([]string, error) {
	status, _, body, err := t.httpReq(ctx, "GET", "/secrets", nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &ToolboxError{Message: fmt.Sprintf("list_secrets: status=%d", status)}
	}
	var resp struct {
		Secrets []string `json:"secrets"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &ToolboxError{Message: "list_secrets: decode: " + err.Error()}
	}
	return resp.Secrets, nil
}

// DeleteSecret removes a stored secret.
func (t *ToolboxClient) DeleteSecret(ctx context.Context, name string) error {
	status, _, _, err := t.httpReq(ctx, "DELETE", "/secrets/"+name, nil, "")
	if err != nil {
		return err
	}
	if status != 200 && status != 204 {
		return &ToolboxError{Message: fmt.Sprintf("delete_secret: status=%d", status)}
	}
	return nil
}

// Health calls the agent's /healthz.
func (t *ToolboxClient) Health(ctx context.Context) (map[string]any, error) {
	status, _, body, err := t.httpReq(ctx, "GET", "/healthz", nil, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &ToolboxError{Message: fmt.Sprintf("health: status=%d", status)}
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out, nil
}

// ---- HTTP over UDS ---------------------------------------------

func (t *ToolboxClient) dialConnect(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: t.DialTimeout}
	sock, err := dialer.DialContext(ctx, "unix", t.UDSPath)
	if err != nil {
		return nil, &ToolboxError{Message: "dial " + t.UDSPath + ": " + err.Error()}
	}
	port := t.Port
	if port == 0 {
		port = 10023
	}
	if _, err := sock.Write([]byte(fmt.Sprintf("CONNECT %d\n", port))); err != nil {
		sock.Close()
		return nil, &ToolboxError{Message: "CONNECT write: " + err.Error()}
	}
	br := bufio.NewReader(sock)
	line, err := br.ReadString('\n')
	if err != nil {
		sock.Close()
		return nil, &ToolboxError{Message: "CONNECT read: " + err.Error()}
	}
	if !strings.HasPrefix(line, "OK") {
		sock.Close()
		return nil, &ToolboxError{Message: "CONNECT rejected: " + strings.TrimSpace(line)}
	}
	// Wrap the socket to expose the already-buffered bytes from br
	// on subsequent reads (usually empty; defense-in-depth).
	return &prefixedConn{Conn: sock, br: br}, nil
}

// prefixedConn reads from the bufio.Reader (which may hold bytes
// that arrived after the OK\n handshake) before falling through
// to the raw conn.
type prefixedConn struct {
	net.Conn
	br *bufio.Reader
}

func (p *prefixedConn) Read(b []byte) (int, error) { return p.br.Read(b) }

func (t *ToolboxClient) httpReq(ctx context.Context, method, path string, body []byte, contentType string) (int, map[string]string, []byte, error) {
	sock, err := t.dialConnect(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	defer sock.Close()
	_ = sock.SetDeadline(time.Now().Add(10 * time.Second))

	hdrs := fmt.Sprintf("%s %s HTTP/1.0\r\nHost: x\r\nConnection: close\r\n", method, path)
	if body != nil {
		hdrs += fmt.Sprintf("Content-Length: %d\r\nContent-Type: %s\r\n", len(body), contentType)
	}
	hdrs += "\r\n"
	if _, err := sock.Write([]byte(hdrs)); err != nil {
		return 0, nil, nil, &ToolboxError{Message: "write headers: " + err.Error()}
	}
	if body != nil {
		if _, err := sock.Write(body); err != nil {
			return 0, nil, nil, &ToolboxError{Message: "write body: " + err.Error()}
		}
	}

	br := bufio.NewReader(sock)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, nil, &ToolboxError{Message: "read status: " + err.Error()}
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, nil, nil, &ToolboxError{Message: "malformed status: " + strings.TrimSpace(statusLine)}
	}
	var status int
	fmt.Sscanf(parts[1], "%d", &status)

	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, nil, &ToolboxError{Message: "read headers: " + err.Error()}
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			headers[strings.ToLower(strings.TrimSpace(line[:idx]))] = strings.TrimSpace(line[idx+1:])
		}
	}

	// Bounded body read: skip for 204/205/304, use Content-Length
	// when present, otherwise read to EOF.
	var bodyOut []byte
	if status != 204 && status != 205 && status != 304 {
		if cl, ok := headers["content-length"]; ok && cl != "" {
			var n int
			fmt.Sscanf(cl, "%d", &n)
			if n > 0 {
				bodyOut = make([]byte, n)
				_, err := io.ReadFull(br, bodyOut)
				if err != nil {
					return 0, nil, nil, &ToolboxError{Message: "read body: " + err.Error()}
				}
			}
		} else {
			bodyOut, _ = io.ReadAll(br)
		}
	}
	return status, headers, bodyOut, nil
}

func writeFrame(w io.Writer, channel byte, payload []byte) error {
	hdr := [5]byte{channel, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return &ToolboxError{Message: "write frame header: " + err.Error()}
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return &ToolboxError{Message: "write frame payload: " + err.Error()}
		}
	}
	return nil
}
