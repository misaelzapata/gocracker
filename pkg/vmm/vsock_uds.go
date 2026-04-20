package vmm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// udsListener exposes a VM's vsock device as a Firecracker-style Unix
// Domain Socket. A client connects, sends "CONNECT <port>\n", and the
// listener bridges that UDS stream to the guest port via DialVsock.
//
// Wire protocol (matches Firecracker):
//
//	client → server:  "CONNECT <port>\n"
//	server → client:  "OK\n"                      (bridge starts)
//	                  "FAILURE <reason>\n"         (and server closes)
//
// Lifecycle: newUDSListener creates the socket and returns; run() accepts
// in a loop until Close() is called. Close() removes the socket file,
// cancels all active bridges, and waits for goroutines to exit.
type udsListener struct {
	path   string
	dialer VsockDialer
	ln     *net.UnixListener

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	bridgesMu sync.Mutex
	bridges   map[*bridgeConn]struct{}
}

type bridgeConn struct {
	client net.Conn
	guest  net.Conn
	cancel context.CancelFunc
}

func newUDSListener(path string, dialer VsockDialer) (*udsListener, error) {
	if path == "" {
		return nil, fmt.Errorf("uds listener: empty path")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("uds listener: path must be absolute, got %q", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("uds listener: mkdir parent: %w", err)
	}
	// Best-effort removal of a stale socket file from a previous crash.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("uds listener: listen: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("uds listener: chmod: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	u := &udsListener{
		path:    path,
		dialer:  dialer,
		ln:      ln.(*net.UnixListener),
		ctx:     ctx,
		cancel:  cancel,
		bridges: make(map[*bridgeConn]struct{}),
	}
	// Reserve wg slot for run() here — BEFORE the caller issues
	// `go u.run()` — so Close()'s wg.Wait never races with a
	// not-yet-started accept goroutine. See run() for the invariant.
	u.wg.Add(1)
	return u, nil
}

// run blocks accepting connections until the listener is closed. Each
// accepted connection spawns a goroutine running handleConn. Safe to call
// in its own goroutine.
//
// newUDSListener bumped wg by 1 for this loop; we Done at exit. Doing
// the Add in the constructor (before `go run`) means Close's wg.Wait
// cannot race with an Add from inside the loop: the counter is ≥1 from
// construction until run returns, so handler Adds only ever grow a
// non-zero counter — they never bump it off zero with a concurrent Wait.
func (u *udsListener) run() {
	defer u.wg.Done()
	for {
		c, err := u.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient Accept error shouldn't happen on Unix sockets unless
			// the listener is being torn down. Exit — Close will be called
			// by the owning VM lifecycle.
			return
		}
		u.wg.Add(1)
		go func(c net.Conn) {
			defer u.wg.Done()
			u.handleConn(c)
		}(c)
	}
}

func (u *udsListener) handleConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	port, perr := parseConnect(line)
	if perr != nil {
		_, _ = c.Write([]byte("FAILURE " + sanitizeReason(perr.Error()) + "\n"))
		return
	}
	guestConn, derr := u.dialer.DialVsock(port)
	if derr != nil {
		_, _ = c.Write([]byte("FAILURE " + sanitizeReason(derr.Error()) + "\n"))
		return
	}
	defer guestConn.Close()
	if _, err := c.Write([]byte("OK\n")); err != nil {
		return
	}
	// Forward any bytes the client pipelined after CONNECT\n.
	if buffered := br.Buffered(); buffered > 0 {
		b, _ := br.Peek(buffered)
		if _, err := guestConn.Write(b); err != nil {
			return
		}
	}

	bctx, bcancel := context.WithCancel(u.ctx)
	bc := &bridgeConn{client: c, guest: guestConn, cancel: bcancel}
	u.bridgesMu.Lock()
	u.bridges[bc] = struct{}{}
	u.bridgesMu.Unlock()
	defer func() {
		u.bridgesMu.Lock()
		delete(u.bridges, bc)
		u.bridgesMu.Unlock()
		bcancel()
	}()

	// Force close both sides if the context is canceled (Close() or
	// closeAllBridges()). This unblocks any stuck io.Copy.
	go func() {
		<-bctx.Done()
		c.Close()
		guestConn.Close()
	}()

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(guestConn, c); errc <- err }()
	go func() { _, err := io.Copy(c, guestConn); errc <- err }()
	<-errc
	c.Close()
	guestConn.Close()
	<-errc
}

// closeAllBridges cancels every active bridge without touching the
// listener itself. Used by VM Pause() so clients disconnect cleanly while
// the listener remains available for post-Resume reconnects.
func (u *udsListener) closeAllBridges() {
	u.bridgesMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(u.bridges))
	for bc := range u.bridges {
		cancels = append(cancels, bc.cancel)
	}
	u.bridgesMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Close tears down the listener, cancels all active bridges, waits for
// every bridge goroutine to exit, and removes the socket file.
func (u *udsListener) Close() error {
	u.cancel()
	err := u.ln.Close()
	u.closeAllBridges()
	u.wg.Wait()
	_ = os.Remove(u.path)
	return err
}

// Path returns the absolute filesystem path of the listening socket.
func (u *udsListener) Path() string { return u.path }

// attachVsockUDSListener wires a Firecracker-style UDS listener onto the VM
// when Vsock.UDSPath is configured. Called from EVERY arch-specific
// setupDevices implementation immediately after the vsock device is
// created — the arch backends are the only place virtio-vsock is wired
// in. Centralising it here keeps amd64 and arm64 in lockstep; a new arch
// that forgets to call this is caught by TestAttachVsockUDSListener.
func attachVsockUDSListener(vm *VM) error {
	if vm == nil || vm.cfg.Vsock == nil || vm.cfg.Vsock.UDSPath == "" {
		return nil
	}
	listener, err := newUDSListener(vm.cfg.Vsock.UDSPath, vm)
	if err != nil {
		return fmt.Errorf("vsock uds listener: %w", err)
	}
	vm.udsListener = listener
	go listener.run()
	return nil
}

func parseConnect(line string) (uint32, error) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "CONNECT ") {
		return 0, fmt.Errorf("malformed_request")
	}
	portStr := strings.TrimPrefix(line, "CONNECT ")
	if portStr == "" {
		return 0, fmt.Errorf("missing_port")
	}
	n, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid_port")
	}
	if n == 0 {
		return 0, fmt.Errorf("invalid_port")
	}
	return uint32(n), nil
}

// sanitizeReason keeps the FAILURE response on one line — no newlines or
// other control chars that would confuse a client parser.
func sanitizeReason(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
