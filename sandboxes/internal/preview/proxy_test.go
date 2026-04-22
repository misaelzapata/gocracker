package preview

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeResolver satisfies Resolver via an in-memory map.
type fakeResolver struct {
	uds map[string]string
}

func (f *fakeResolver) UDSPathForSandbox(id string) (string, bool) {
	p, ok := f.uds[id]
	return p, ok
}

// fakeUDS spins up a Unix-socket listener that speaks the
// Firecracker-style "CONNECT <port>\n" handshake and then proxies
// stream bytes to a per-port handler. Cleaned up on test end.
type fakeUDS struct {
	t       *testing.T
	path    string
	ln      *net.UnixListener
	wg      sync.WaitGroup
	mu      sync.Mutex
	handler func(port uint32, c net.Conn)
}

func newFakeUDS(t *testing.T, handler func(port uint32, c net.Conn)) *fakeUDS {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "vm.sock")
	addr, _ := net.ResolveUnixAddr("unix", path)
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeUDS{t: t, path: path, ln: ln, handler: handler}
	f.wg.Add(1)
	go f.acceptLoop()
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeUDS) acceptLoop() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func(c net.Conn) {
			defer f.wg.Done()
			defer c.Close()
			br := bufio.NewReader(c)
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "CONNECT ") {
				_, _ = c.Write([]byte("FAILURE bad_handshake\n"))
				return
			}
			var port uint32
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT ")), "%d", &port)
			if _, err := c.Write([]byte("OK\n")); err != nil {
				return
			}
			f.mu.Lock()
			h := f.handler
			f.mu.Unlock()
			if h != nil {
				h(port, c)
			}
		}(c)
	}
}

// guestHandler returns a function that pretends to be the guest's
// HTTP server: parses the inbound request and responds with the
// given status + body. Captures the seen request for assertion.
func guestHandler(status int, body string, seen *http.Request) func(uint32, net.Conn) {
	return func(_ uint32, c net.Conn) {
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if seen != nil {
			*seen = *req
		}
		// Drain body so the proxy's writer doesn't block on a
		// pipelined chunk.
		if req.ContentLength > 0 {
			_, _ = io.CopyN(io.Discard, req.Body, req.ContentLength)
		}
		resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			status, http.StatusText(status), len(body), body)
		_, _ = c.Write([]byte(resp))
	}
}

func TestProxy_HappyPath_ResponseFromGuest(t *testing.T) {
	uds := newFakeUDS(t, guestHandler(200, "hello from guest", nil))
	res := &fakeResolver{uds: map[string]string{"sb-x": uds.path}}
	p := &Proxy{Resolver: res}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if err := p.ServeRequest(w, req, "sb-x", 8080); err != nil {
		t.Fatalf("ServeRequest: %v", err)
	}
	if w.Code != 200 {
		t.Errorf("status=%d, want 200", w.Code)
	}
	if w.Body.String() != "hello from guest" {
		t.Errorf("body=%q, want %q", w.Body.String(), "hello from guest")
	}
}

func TestProxy_ResolverMiss_404(t *testing.T) {
	res := &fakeResolver{uds: map[string]string{}}
	p := &Proxy{Resolver: res}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	err := p.ServeRequest(w, req, "missing", 80)
	if !errors.Is(err, ErrSandboxNotFound) {
		t.Errorf("err=%v, want ErrSandboxNotFound", err)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestProxy_DialFailure_502(t *testing.T) {
	res := &fakeResolver{uds: map[string]string{"sb-x": "/nonexistent/path.sock"}}
	p := &Proxy{Resolver: res}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if err := p.ServeRequest(w, req, "sb-x", 80); err == nil {
		t.Fatal("ServeRequest should fail on bad UDS path")
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", w.Code)
	}
}

func TestProxy_ForwardsRequestPathAndMethod(t *testing.T) {
	var seen http.Request
	uds := newFakeUDS(t, guestHandler(200, "ok", &seen))
	res := &fakeResolver{uds: map[string]string{"sb-x": uds.path}}
	p := &Proxy{Resolver: res}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/foo?q=bar", strings.NewReader("payload"))
	req.Header.Set("X-Custom", "yes")
	req.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	if err := p.ServeRequest(w, req, "sb-x", 8080); err != nil {
		t.Fatalf("ServeRequest: %v", err)
	}
	if seen.Method != http.MethodPost {
		t.Errorf("guest saw method=%q, want POST", seen.Method)
	}
	if seen.URL.Path != "/api/v1/foo" {
		t.Errorf("guest saw path=%q, want /api/v1/foo", seen.URL.Path)
	}
	if seen.URL.RawQuery != "q=bar" {
		t.Errorf("guest saw query=%q, want q=bar", seen.URL.RawQuery)
	}
	if got := seen.Header.Get("X-Custom"); got != "yes" {
		t.Errorf("X-Custom not forwarded: %q", got)
	}
	if got := seen.Header.Get("X-Forwarded-For"); !strings.Contains(got, "10.0.0.5") {
		t.Errorf("X-Forwarded-For=%q, want to contain 10.0.0.5", got)
	}
}

func TestProxy_StripsHopByHopHeaders(t *testing.T) {
	var seen http.Request
	uds := newFakeUDS(t, guestHandler(200, "ok", &seen))
	res := &fakeResolver{uds: map[string]string{"sb-x": uds.path}}
	p := &Proxy{Resolver: res}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Proxy-Authorization", "leak-this-please")
	req.Header.Set("Te", "trailers")
	w := httptest.NewRecorder()
	if err := p.ServeRequest(w, req, "sb-x", 8080); err != nil {
		t.Fatalf("ServeRequest: %v", err)
	}
	if seen.Header.Get("Proxy-Authorization") != "" {
		t.Error("Proxy-Authorization leaked to guest")
	}
	if seen.Header.Get("Te") != "" {
		t.Error("Te leaked to guest")
	}
}

func TestProxy_GuestErrorStatusFlows(t *testing.T) {
	uds := newFakeUDS(t, guestHandler(404, "not in guest", nil))
	res := &fakeResolver{uds: map[string]string{"sb-x": uds.path}}
	p := &Proxy{Resolver: res}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	if err := p.ServeRequest(w, req, "sb-x", 8080); err != nil {
		t.Fatalf("ServeRequest: %v", err)
	}
	if w.Code != 404 {
		t.Errorf("status=%d, want 404", w.Code)
	}
	if w.Body.String() != "not in guest" {
		t.Errorf("body=%q, want %q", w.Body.String(), "not in guest")
	}
}

func TestIsHopByHop(t *testing.T) {
	cases := map[string]bool{
		"Connection":         true,
		"connection":         true,
		"Transfer-Encoding":  true,
		"X-Custom":           false,
		"Content-Type":       false,
	}
	for h, want := range cases {
		t.Run(h, func(t *testing.T) {
			if got := isHopByHop(h); got != want {
				t.Errorf("isHopByHop(%q) = %v, want %v", h, got, want)
			}
		})
	}
}
