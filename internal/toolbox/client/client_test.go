package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
)

// startAgentWithUDSBridge stands up a real agent on TCP, plus a UDS
// listener that bridges any "CONNECT <port>\n" client to the agent
// — exactly the same shape as pkg/vmm/vsock_uds.go but in-process,
// so the client can be exercised without booting a VM.
func startAgentWithUDSBridge(t *testing.T) *Client {
	t.Helper()

	srv := httptest.NewServer(agent.Handler())
	t.Cleanup(srv.Close)

	udsPath := newTempSocketPath(t)
	ln, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", udsPath, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go bridgeOne(t, c, srv.Listener.Addr().String())
		}
	}()

	return &Client{UDSPath: udsPath, Port: 10023, DialTimeout: 5 * time.Second}
}

// bridgeOne reads "CONNECT <port>\n" from the client, writes "OK\n",
// and then io.Copy's bytes between the client UDS and the agent's
// TCP listener. Mimics pkg/vmm/vsock_uds.go without depending on it.
func bridgeOne(t *testing.T, client net.Conn, agentAddr string) {
	t.Helper()
	defer client.Close()
	buf := make([]byte, 64)
	n, err := readUntilNewline(client, buf)
	if err != nil {
		return
	}
	line := string(buf[:n])
	if !strings.HasPrefix(line, "CONNECT ") {
		_, _ = client.Write([]byte("FAILURE bad_request\n"))
		return
	}
	guest, err := net.Dial("tcp", agentAddr)
	if err != nil {
		_, _ = client.Write([]byte("FAILURE dial_failed\n"))
		return
	}
	defer guest.Close()
	if _, err := client.Write([]byte("OK\n")); err != nil {
		return
	}
	go io.Copy(guest, client)
	io.Copy(client, guest)
}

func readUntilNewline(c net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		one := buf[n : n+1]
		k, err := c.Read(one)
		if err != nil {
			return n, err
		}
		n += k
		if one[0] == '\n' {
			return n, nil
		}
	}
	return n, fmt.Errorf("connect line too long")
}

func newTempSocketPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir + "/agent.sock"
}

func TestClient_Health(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.OK {
		t.Fatalf("Health.OK = false")
	}
	if h.Version == "" {
		t.Fatalf("Health.Version empty")
	}
}

func TestClient_Exec_StdoutAndExit(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	var stdout, stderr bytes.Buffer
	res, err := c.Exec(
		context.Background(),
		agent.ExecRequest{Cmd: []string{"sh", "-c", "echo HELLO; echo ERR 1>&2; exit 5"}},
		nil, &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 5 {
		t.Fatalf("ExitCode: got %d, want 5", res.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "HELLO" {
		t.Fatalf("stdout: got %q, want %q", got, "HELLO")
	}
	if got := strings.TrimSpace(stderr.String()); got != "ERR" {
		t.Fatalf("stderr: got %q, want %q", got, "ERR")
	}
}

func TestClient_Exec_StdinEcho(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	stdin := strings.NewReader("hello-via-stdin\n")
	var stdout bytes.Buffer
	res, err := c.Exec(
		context.Background(),
		agent.ExecRequest{Cmd: []string{"cat"}},
		stdin, &stdout, nil,
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode: got %d, want 0", res.ExitCode)
	}
	if got := stdout.String(); got != "hello-via-stdin\n" {
		t.Fatalf("stdout: got %q", got)
	}
}

func TestClient_Stream_Signal(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	sess, err := c.Stream(context.Background(), agent.ExecRequest{Cmd: []string{"sleep", "30"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer sess.Close()
	// Give sleep a moment to actually start before yelling at it.
	time.Sleep(100 * time.Millisecond)
	if err := sess.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ch, payload, err := sess.NextFrame()
		if err != nil {
			t.Fatalf("NextFrame: %v", err)
		}
		if ch != agent.ChannelExit {
			continue
		}
		code, err := agent.ParseExitPayload(payload)
		if err != nil {
			t.Fatalf("ParseExitPayload: %v", err)
		}
		if code != -1 {
			t.Fatalf("exit after SIGTERM: got %d, want -1 (signal-killed)", code)
		}
		return
	}
	t.Fatal("never received EXIT frame after SIGTERM")
}

func TestClient_Concurrent_Sessions(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			var stdout bytes.Buffer
			res, err := c.Exec(
				context.Background(),
				agent.ExecRequest{Cmd: []string{"sh", "-c", fmt.Sprintf("echo concurrent-%d", i)}},
				nil, &stdout, nil,
			)
			if err != nil {
				errs[i] = err
				return
			}
			want := fmt.Sprintf("concurrent-%d\n", i)
			if got := stdout.String(); got != want {
				errs[i] = fmt.Errorf("session %d: got %q, want %q", i, got, want)
				return
			}
			if res.ExitCode != 0 {
				errs[i] = fmt.Errorf("session %d: exit %d", i, res.ExitCode)
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Errorf("%v", e)
		}
	}
}

func TestClient_LargeStdoutChunks(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	const N = 200 << 10
	var stdout bytes.Buffer
	res, err := c.Exec(
		context.Background(),
		agent.ExecRequest{Cmd: []string{"sh", "-c", fmt.Sprintf("head -c %d /dev/zero | tr '\\0' A", N)}},
		nil, &stdout, nil,
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode: got %d", res.ExitCode)
	}
	if stdout.Len() != N {
		t.Fatalf("stdout size: got %d, want %d", stdout.Len(), N)
	}
}

func TestClient_Stream_NextFrameSurfaces(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	sess, err := c.Stream(context.Background(), agent.ExecRequest{
		Cmd: []string{"sh", "-c", "echo line1; echo line2 1>&2; exit 0"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	var exitCode int32 = -42
	for {
		ch, payload, err := sess.NextFrame()
		if err != nil {
			t.Fatalf("NextFrame: %v", err)
		}
		switch ch {
		case agent.ChannelStdout:
			stdout.Write(payload)
		case agent.ChannelStderr:
			stderr.Write(payload)
		case agent.ChannelExit:
			exitCode, _ = agent.ParseExitPayload(payload)
		}
		if ch == agent.ChannelExit {
			break
		}
	}
	if strings.TrimSpace(stdout.String()) != "line1" {
		t.Fatalf("stdout: %q", stdout.String())
	}
	if strings.TrimSpace(stderr.String()) != "line2" {
		t.Fatalf("stderr: %q", stderr.String())
	}
	if exitCode != 0 {
		t.Fatalf("exit: got %d", exitCode)
	}
}

func TestClient_DialFailure_BadPath(t *testing.T) {
	c := &Client{UDSPath: "/nonexistent/socket.path"}
	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error dialing nonexistent UDS")
	}
	if !strings.Contains(err.Error(), "dial UDS") {
		t.Fatalf("error message should mention dial UDS: %v", err)
	}
}

func TestClient_DialFailure_EmptyPath(t *testing.T) {
	c := &Client{}
	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error with empty UDSPath")
	}
	if !strings.Contains(err.Error(), "UDSPath is required") {
		t.Fatalf("error: %v", err)
	}
}
