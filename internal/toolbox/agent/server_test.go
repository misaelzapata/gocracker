package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetSecrets is required before any subtest that asserts /secrets state
// because guestSecrets is a process-global map. Without it tests would
// observe leftover entries from other subtests run in the same binary.
func resetSecrets(t *testing.T) {
	t.Helper()
	guestSecretsMu.Lock()
	guestSecrets = map[string]SandboxSecret{}
	guestSecretsMu.Unlock()
}

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir tmp: %v", err)
	}
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)
	return srv, dir
}

func mustGet(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func mustDo(t *testing.T, method, url string, body []byte) *http.Response {
	t.Helper()
	var br io.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, br)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := mustGet(t, srv, "/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !h.OK || h.Version != Version {
		t.Fatalf("payload: got %+v", h)
	}
}

func TestInfoVersion(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := mustGet(t, srv, "/info/version")
	defer resp.Body.Close()
	var v map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v["version"] != Version {
		t.Fatalf("version: got %q, want %q", v["version"], Version)
	}
}

func TestFiles_UploadListDownloadDelete(t *testing.T) {
	srv, dir := newTestServer(t)
	target := filepath.Join(dir, "hello.txt")
	payload := []byte("hello toolbox\n")

	up := mustDo(t, http.MethodPost, srv.URL+"/files/upload?path="+target, payload)
	up.Body.Close()
	if up.StatusCode != http.StatusOK {
		t.Fatalf("upload status: %d", up.StatusCode)
	}
	if data, err := os.ReadFile(target); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("on-disk: %v / %q", err, data)
	}

	ls := mustGet(t, srv, "/files?path="+dir)
	defer ls.Body.Close()
	var listResp FileListResponse
	if err := json.NewDecoder(ls.Body).Decode(&listResp); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	found := false
	for _, e := range listResp.Entries {
		if e.Name == "hello.txt" && e.Kind == "file" && e.Size == int64(len(payload)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("entry not found: %+v", listResp.Entries)
	}

	dl := mustGet(t, srv, "/files/download?path="+target)
	defer dl.Body.Close()
	gotBody, _ := io.ReadAll(dl.Body)
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("download payload mismatch: %q", gotBody)
	}

	del := mustDo(t, http.MethodDelete, srv.URL+"/files?path="+target, nil)
	del.Body.Close()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file not removed: err=%v", err)
	}
}

func TestFiles_MkdirRenameChmod(t *testing.T) {
	srv, dir := newTestServer(t)
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")

	body, _ := json.Marshal(map[string]any{"path": a, "all": true})
	resp := mustDo(t, http.MethodPost, srv.URL+"/files/mkdir", body)
	resp.Body.Close()
	if info, err := os.Stat(a); err != nil || !info.IsDir() {
		t.Fatalf("mkdir: %v / %v", err, info)
	}

	body, _ = json.Marshal(map[string]string{"old_path": a, "new_path": b})
	resp = mustDo(t, http.MethodPost, srv.URL+"/files/rename", body)
	resp.Body.Close()
	if _, err := os.Stat(b); err != nil {
		t.Fatalf("rename: %v", err)
	}

	body, _ = json.Marshal(map[string]any{"path": b, "mode": 0o700})
	resp = mustDo(t, http.MethodPost, srv.URL+"/files/chmod", body)
	resp.Body.Close()
	info, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat post-chmod: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("chmod: got %v, want 0700", info.Mode().Perm())
	}
}

func TestSecrets_SetListDelete(t *testing.T) {
	resetSecrets(t)
	srv, _ := newTestServer(t)

	body, _ := json.Marshal(SandboxSecret{Name: "API_KEY", Value: "shh", AllowedHosts: []string{"api.example.com"}})
	resp := mustDo(t, http.MethodPost, srv.URL+"/secrets", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set status: %d", resp.StatusCode)
	}

	ls := mustGet(t, srv, "/secrets")
	defer ls.Body.Close()
	var lsResp map[string][]string
	if err := json.NewDecoder(ls.Body).Decode(&lsResp); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(lsResp["secrets"]) != 1 || lsResp["secrets"][0] != "API_KEY" {
		t.Fatalf("list: got %+v", lsResp)
	}

	del := mustDo(t, http.MethodDelete, srv.URL+"/secrets/API_KEY", nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status: %d", del.StatusCode)
	}

	ls2 := mustGet(t, srv, "/secrets")
	defer ls2.Body.Close()
	var lsResp2 map[string][]string
	_ = json.NewDecoder(ls2.Body).Decode(&lsResp2)
	if len(lsResp2["secrets"]) != 0 {
		t.Fatalf("not empty post-delete: %+v", lsResp2)
	}
}

func TestSecrets_NameRequired(t *testing.T) {
	resetSecrets(t)
	srv, _ := newTestServer(t)

	body, _ := json.Marshal(SandboxSecret{Value: "v"})
	resp := mustDo(t, http.MethodPost, srv.URL+"/secrets", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestProxy_PortValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, p := range []string{"abc", "0", "70000", ""} {
		resp := mustGet(t, srv, "/proxy/http/"+p+"/")
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("port=%q: expected 400, got %d", p, resp.StatusCode)
		}
	}
}

func TestProxy_ForwardsToBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "yes")
		_, _ = w.Write([]byte("from-backend:" + r.URL.Path))
	}))
	defer backend.Close()
	port := strings.TrimPrefix(backend.URL, "http://127.0.0.1:")

	srv, _ := newTestServer(t)
	resp := mustGet(t, srv, "/proxy/http/"+port+"/sub/path?q=1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Backend"); got != "yes" {
		t.Fatalf("X-Backend not propagated: %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "from-backend:/sub/path" {
		t.Fatalf("body: %q", body)
	}
}
