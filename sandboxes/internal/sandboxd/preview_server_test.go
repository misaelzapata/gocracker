package sandboxd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUDS mirrors the preview/proxy_test.go helper but kept local
// here since sandboxd_test can't reach preview's internal helpers.
// Speaks CONNECT handshake + hands each accepted conn off to a
// user-supplied handler.
type fakeUDS struct {
	t       *testing.T
	path    string
	ln      *net.UnixListener
	wg      sync.WaitGroup
	handler func(port uint32, c net.Conn)
}

func startFakeUDS(t *testing.T, handler func(port uint32, c net.Conn)) *fakeUDS {
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
	go f.loop()
	t.Cleanup(func() {
		ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeUDS) loop() {
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
			if err != nil || !strings.HasPrefix(line, "CONNECT ") {
				return
			}
			var port uint32
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT ")), "%d", &port)
			c.Write([]byte("OK\n"))
			if f.handler != nil {
				f.handler(port, c)
			}
		}(c)
	}
}

func guestHTTPHandler(status int, body string) func(uint32, net.Conn) {
	return func(_ uint32, c net.Conn) {
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.ContentLength > 0 {
			_, _ = io.CopyN(io.Discard, req.Body, req.ContentLength)
		}
		fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			status, http.StatusText(status), len(body), body)
	}
}

// setupPreview wires a real Manager with a fake UDS and a seeded
// sandbox. Returns the server, manager, and sandbox id.
func setupPreview(t *testing.T, guest func(uint32, net.Conn)) (*httptest.Server, *Manager, string) {
	t.Helper()
	uds := startFakeUDS(t, guest)
	store, _ := NewStore("")
	sbID := "sb-preview-x"
	_ = store.Add(&Sandbox{ID: sbID, State: StateReady, UDSPath: uds.path})
	mgr := &Manager{
		Store:             store,
		PreviewSigningKey: []byte("test-key-must-be-at-least-32-bytes-long"),
		PreviewTTL:        10 * time.Minute,
		PreviewHost:       "sbx.localhost",
	}
	srv := httptest.NewServer((&Server{Lifecycle: mgr, Store: store}).Handler())
	t.Cleanup(srv.Close)
	return srv, mgr, sbID
}

func TestPreview_MintPost_Returns201WithToken(t *testing.T) {
	srv, _, sbID := setupPreview(t, nil)
	resp, err := http.Post(srv.URL+"/sandboxes/"+sbID+"/preview/3000", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d, want 201", resp.StatusCode)
	}
	var got MintPreviewResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Token == "" {
		t.Error("empty token")
	}
	if !strings.HasPrefix(got.URL, "/previews/") {
		t.Errorf("URL=%q, want /previews/ prefix", got.URL)
	}
	if got.Subdomain != sbID+".sbx.localhost" {
		t.Errorf("Subdomain=%q", got.Subdomain)
	}
	if got.ExpiresAt.Before(time.Now()) {
		t.Errorf("ExpiresAt in the past: %v", got.ExpiresAt)
	}
}

func TestPreview_MintPost_UnknownSandbox_404(t *testing.T) {
	srv, _, _ := setupPreview(t, nil)
	resp, _ := http.Post(srv.URL+"/sandboxes/sb-ghost/preview/3000", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestPreview_MintPost_BadPort_400(t *testing.T) {
	srv, _, sbID := setupPreview(t, nil)
	resp, _ := http.Post(srv.URL+"/sandboxes/"+sbID+"/preview/notanumber", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

// TestPreview_PathRouteProxies: mint + GET /previews/<tok>/hello →
// the guest sees the request on /hello and returns 200.
func TestPreview_PathRouteProxies(t *testing.T) {
	srv, _, sbID := setupPreview(t, guestHTTPHandler(200, "hello from guest"))

	// Mint.
	resp, _ := http.Post(srv.URL+"/sandboxes/"+sbID+"/preview/3000", "application/json", nil)
	var mr MintPreviewResponse
	_ = json.NewDecoder(resp.Body).Decode(&mr)
	resp.Body.Close()

	// Fetch via the token URL.
	pr, err := http.Get(srv.URL + "/previews/" + mr.Token + "/hello")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != 200 {
		t.Errorf("status=%d, want 200", pr.StatusCode)
	}
	body, _ := io.ReadAll(pr.Body)
	if string(body) != "hello from guest" {
		t.Errorf("body=%q", body)
	}
}

func TestPreview_PathRoute_InvalidToken_401(t *testing.T) {
	srv, _, _ := setupPreview(t, guestHTTPHandler(200, "wont-see-this"))
	resp, err := http.Get(srv.URL + "/previews/bogus-token/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
}

// TestPreview_SubdomainRoute_TokenInQuery: the first request to the
// subdomain carries ?token=<tok>, proxy verifies + sets cookie +
// redirects to same URL without the token.
func TestPreview_SubdomainRoute_TokenInQuery(t *testing.T) {
	srv, _, sbID := setupPreview(t, guestHTTPHandler(200, "ok"))

	// Mint.
	mintResp, _ := http.Post(srv.URL+"/sandboxes/"+sbID+"/preview/3000", "application/json", nil)
	var mr MintPreviewResponse
	_ = json.NewDecoder(mintResp.Body).Decode(&mr)
	mintResp.Body.Close()

	// Hit the subdomain with ?token=<tok>.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/?token="+mr.Token, nil)
	req.Host = mr.Subdomain
	// Disable auto-redirect so we can inspect the 303 + cookie.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET subdomain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", resp.StatusCode)
	}
	setCookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, previewCookieName+"=") {
		t.Errorf("Set-Cookie missing %s: %q", previewCookieName, setCookie)
	}
}

// TestPreview_SubdomainRoute_CookieAuth: once the cookie is set,
// subsequent requests to the subdomain (no ?token) proxy directly.
func TestPreview_SubdomainRoute_CookieAuth(t *testing.T) {
	srv, _, sbID := setupPreview(t, guestHTTPHandler(200, "hello via cookie"))

	mintResp, _ := http.Post(srv.URL+"/sandboxes/"+sbID+"/preview/3000", "application/json", nil)
	var mr MintPreviewResponse
	_ = json.NewDecoder(mintResp.Body).Decode(&mr)
	mintResp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/path/to/app", nil)
	req.Host = mr.Subdomain
	req.AddCookie(&http.Cookie{Name: previewCookieName, Value: mr.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status=%d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello via cookie" {
		t.Errorf("body=%q", body)
	}
}

func TestPreview_SubdomainRoute_NoCredentials_401(t *testing.T) {
	srv, _, sbID := setupPreview(t, guestHTTPHandler(200, "wont-see"))
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = sbID + ".sbx.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

// TestPreview_SubdomainRoute_WrongSandboxID_403: cookie has valid
// token but for a different sandbox/port than the subdomain claims.
// That's an explicit 403, not 401 — credential is real.
func TestPreview_SubdomainRoute_WrongSandboxID_403(t *testing.T) {
	srv, mgr, _ := setupPreview(t, guestHTTPHandler(200, "wont-see"))
	// Add a second sandbox + mint token for IT.
	otherID := "sb-other"
	_ = mgr.Store.Add(&Sandbox{ID: otherID, State: StateReady, UDSPath: "/nonexistent/sock"})
	otherMint, _ := mgr.MintPreview(otherID, 9999)

	// Hit "sb-preview-x.sbx.localhost" with otherMint.Token.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = "sb-preview-x.sbx.localhost"
	req.AddCookie(&http.Cookie{Name: previewCookieName, Value: otherMint.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status=%d, want 403", resp.StatusCode)
	}
}

func TestPreview_NonPreviewHostFallsThrough(t *testing.T) {
	// healthz is a non-preview route; preview middleware should let
	// it through unchanged.
	srv, _, _ := setupPreview(t, nil)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz status=%d, want 200 (middleware swallowed it?)", resp.StatusCode)
	}
}

func TestParsePreviewHost(t *testing.T) {
	cases := []struct {
		host   string
		root   string
		wantID string
		ok     bool
	}{
		{"sb-abc.sbx.localhost", "sbx.localhost", "sb-abc", true},
		{"sb-abc.sbx.localhost:9091", "sbx.localhost", "sb-abc", true},
		{"sbx.localhost", "sbx.localhost", "", false},                  // no subdomain label
		{"foo.bar.sbx.localhost", "sbx.localhost", "", false},          // nested subdomain
		{"sb-abc.other.com", "sbx.localhost", "", false},
		{".sbx.localhost", "sbx.localhost", "", false},                 // empty label
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			id, ok := parsePreviewHost(tc.host, tc.root)
			if ok != tc.ok || id != tc.wantID {
				t.Errorf("parsePreviewHost(%q) = (%q, %v), want (%q, %v)",
					tc.host, id, ok, tc.wantID, tc.ok)
			}
		})
	}
}
