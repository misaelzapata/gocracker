package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Most of handleSetNetwork is netlink — we can't unit-test the
// real LinkSetDown/AddrReplace path without root + a real
// interface, and on macOS the platform-specific stub returns 501.
// What we CAN cover:
//   - request validation (missing IP → 400)
//   - JSON decode failures → 400
//   - body shape sanity
// The actual netlink path is exercised by the live E2E in
// tests/integration (gated by E2E=1 + root) and by the manual
// smoke against a running VM.

func postSetNetwork(t *testing.T, srv *httptest.Server, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/internal/setnetwork", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestSetNetwork_RejectsEmptyIP(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	body, _ := json.Marshal(SetNetworkRequest{Interface: "eth0"})
	resp := postSetNetwork(t, srv, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	var perr map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&perr)
	if !strings.Contains(perr["error"], "ip is required") {
		t.Fatalf("error message: %q does not mention 'ip is required'", perr["error"])
	}
}

func TestSetNetwork_RejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp := postSetNetwork(t, srv, []byte("{not json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestSetNetwork_RejectsBadCIDR(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	body, _ := json.Marshal(SetNetworkRequest{IP: "not-an-ip"})
	resp := postSetNetwork(t, srv, body)
	defer resp.Body.Close()
	// On Linux this fails ParseAddr → 400. On non-Linux the stub
	// returns 501 before validation runs. Both are acceptable
	// for this test — we only care that the handler doesn't
	// 500 or hang on garbage input.
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 400 or 501", resp.StatusCode)
	}
}
