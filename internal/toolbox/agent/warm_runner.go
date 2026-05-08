package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "embed"
)

// node-repl-server.js is shipped inside the toolbox agent binary so we
// don't have to coordinate disk-build paths with sandboxd; the agent
// writes it out to a known guest path on first need and spawns `node`
// against it. See runners/node-repl-server.js for the wire protocol.
//
//go:embed runners/node-repl-server.js
var nodeReplServerJS []byte

const (
	// nodeWarmRunnerPath is where the JS file lives inside the guest;
	// the agent writes it during StartNodeWarmRunner. /opt/gocracker
	// already exists for the toolbox binary itself, so this just
	// drops a sibling.
	nodeWarmRunnerPath = "/opt/gocracker/runners/node-repl-server.js"

	// nodeWarmSockPath is the UDS the JS server listens on. The agent
	// dials this when it sees cmd[0] == "node-warm" in /exec.
	nodeWarmSockPath = "/run/gocracker/warm-node.sock"

	// nodeWarmReadyPath is touched by the JS server after `listen()`
	// callback fires. The host polls this via the /runtime/node/ready
	// endpoint to know "snapshot the VM now, the runtime is parked
	// idle and the connect path works."
	nodeWarmReadyPath = "/run/gocracker/warm-node.ready"
)

// nodeWarmState tracks whether the warm runner is up. The /runtime/node/ready
// HTTP handler reads this; the StartNodeWarmRunner goroutine flips it.
var (
	nodeWarmReady atomic.Bool
	nodeWarmOnce  sync.Once
)

// StartNodeWarmRunner spawns the embedded node-repl-server.js as a
// long-running child of the agent. Idempotent: only the first call
// actually does work; the rest no-op (avoiding the startup cost on
// snapshot-restore where the agent's atexit handlers don't run before
// pause). Errors are logged but never returned — the warm path
// degrades to "no warm runner" and exec falls back to fork+exec via
// the regular `node` cmd path.
//
// Call this from the agent's Serve startup AFTER the listener is
// bound but BEFORE accepting connections, so the readiness probe at
// /runtime/node/ready never serves true while the runner is half-up.
func StartNodeWarmRunner() {
	nodeWarmOnce.Do(func() {
		if err := startNodeWarmRunner(); err != nil {
			// Best-effort: log and continue. The exec path checks
			// nodeWarmReady before dialling, so this just means
			// node-warm cmds fall back / error politely.
			fmt.Fprintf(os.Stderr, "[warm-runner] node startup skipped: %v\n", err)
		}
	})
}

func startNodeWarmRunner() error {
	if _, err := exec.LookPath("node"); err != nil {
		return fmt.Errorf("node not in PATH: %w", err)
	}
	// Drop the JS to disk if it isn't already there. We rewrite on
	// every cold start so a toolbox version bump invalidates stale
	// scripts cleanly; the snapshot path captures whatever was on
	// disk at snapshot time, which is what we want.
	if err := os.MkdirAll(filepath.Dir(nodeWarmRunnerPath), 0o755); err != nil {
		return fmt.Errorf("mkdir runners dir: %w", err)
	}
	if err := os.WriteFile(nodeWarmRunnerPath, nodeReplServerJS, 0o755); err != nil {
		return fmt.Errorf("write js: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(nodeWarmSockPath), 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Stale ready/sock files from a previous boot must be cleared so
	// the readiness probe can't observe an old marker before the new
	// listener is up.
	_ = os.Remove(nodeWarmReadyPath)
	_ = os.Remove(nodeWarmSockPath)

	cmd := exec.Command("node", nodeWarmRunnerPath)
	cmd.Env = append(os.Environ(),
		"GOCRACKER_NODE_WARM_SOCK="+nodeWarmSockPath,
		"GOCRACKER_NODE_WARM_READY="+nodeWarmReadyPath,
	)
	cmd.Stdout = os.Stderr // forward READY line + diagnostics to agent log
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	// Promote a goroutine that flips nodeWarmReady when the JS
	// server's ready file appears, AND a watchdog that flips it
	// back off if the child exits. The poll period is short (10 ms)
	// because we expect the runner to be up in &lt; 200 ms even on
	// alpine cold cache.
	go waitNodeWarmReady(cmd)
	return nil
}

func waitNodeWarmReady(cmd *exec.Cmd) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(nodeWarmReadyPath); err == nil {
			nodeWarmReady.Store(true)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Reap the child. If it exits we drop ready. Don't restart
	// automatically — a crash means the user code corrupted the
	// shared sandbox state and the only safe move is for callers
	// to fall back to fork+exec via regular `node` exec.
	_ = cmd.Wait()
	nodeWarmReady.Store(false)
}

// NodeWarmReady reports whether the warm runner is currently up and
// answering. Used by the /runtime/node/ready HTTP handler and by the
// exec dispatcher to fail-closed quickly when the runner is down.
func NodeWarmReady() bool { return nodeWarmReady.Load() }

// dialNodeWarm opens a fresh UDS connection to the warm runner. We
// don't pool connections: the runner is single-threaded V8, and
// pooling would just reorder requests. One-conn-per-exec is correct
// and the connect cost is sub-millisecond on a UNIX domain socket.
func dialNodeWarm(ctx context.Context) (net.Conn, error) {
	if !nodeWarmReady.Load() {
		return nil, errors.New("node-warm runner not ready (set GOCRACKER_NODE_WARM=1 on the template, or fall back to plain `node` exec)")
	}
	d := &net.Dialer{}
	return d.DialContext(ctx, "unix", nodeWarmSockPath)
}

// WarmEvalRequest matches the JSON shape node-repl-server.js expects.
type WarmEvalRequest struct {
	ID        int    `json:"id"`
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// WarmEvalResponse matches what node-repl-server.js writes back.
type WarmEvalResponse struct {
	ID       int    `json:"id"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Result   string `json:"result,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// EncodeWarmRequest writes a request as a single JSON line. Splitting
// out the encoder + decoder so the test for the wire format can run
// without a real subprocess.
func EncodeWarmRequest(w net.Conn, req WarmEvalRequest) error {
	data, err := json.Marshal(&req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// DecodeWarmResponse reads exactly one newline-delimited JSON object
// from the connection and unmarshals it.
func DecodeWarmResponse(r net.Conn) (WarmEvalResponse, error) {
	dec := json.NewDecoder(r)
	var resp WarmEvalResponse
	if err := dec.Decode(&resp); err != nil {
		return WarmEvalResponse{}, err
	}
	return resp, nil
}
