package compose

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

func TestSnapshotManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := StackSnapshotManifest{
		Version:     1,
		Timestamp:   time.Now().UTC().Round(time.Second),
		StackName:   "stack-123",
		ComposePath: "/tmp/docker-compose.yml",
		KernelPath:  "/tmp/vmlinux",
		Services: []StackSnapshotService{
			{Name: "db", VMID: "vm-db"},
			{Name: "web", VMID: "vm-web"},
		},
	}
	if err := WriteSnapshotManifest(dir, want); err != nil {
		t.Fatalf("WriteSnapshotManifest(): %v", err)
	}

	got, err := ReadSnapshotManifest(dir)
	if err != nil {
		t.Fatalf("ReadSnapshotManifest(): %v", err)
	}
	if got.StackName != want.StackName || got.ComposePath != want.ComposePath || got.KernelPath != want.KernelPath {
		t.Fatalf("manifest = %#v, want %#v", got, want)
	}
	if len(got.Services) != 2 || got.Services[0].Name != "db" || got.Services[1].Name != "web" {
		t.Fatalf("services = %#v", got.Services)
	}
}

func TestSnapshotRemoteWritesManifestAndServiceSnapshots(t *testing.T) {
	destDir := t.TempDir()
	composePath := filepath.Join(destDir, "docker-compose.yml")
	stackName := StackNameForComposePath(composePath)

	mux := http.NewServeMux()
	mux.HandleFunc("/vms", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("stack"); got != stackName {
			t.Fatalf("stack filter = %q, want %q", got, stackName)
		}
		_ = json.NewEncoder(w).Encode([]internalapi.VMInfo{
			{
				ID:     "vm-web",
				State:  "running",
				Kernel: "/kernels/vmlinux",
				Metadata: map[string]string{
					"service_name": "web",
					"compose_file": composePath,
					"guest_ip":     "172.20.0.3",
				},
			},
			{
				ID:     "vm-db",
				State:  "running",
				Kernel: "/kernels/vmlinux",
				Metadata: map[string]string{
					"service_name": "db",
					"compose_file": composePath,
					"guest_ip":     "172.20.0.2",
				},
			},
		})
	})
	mux.HandleFunc("/vms/vm-web/snapshot", func(w http.ResponseWriter, r *http.Request) {
		assertSnapshotRequest(t, w, r)
	})
	mux.HandleFunc("/vms/vm-db/snapshot", func(w http.ResponseWriter, r *http.Request) {
		assertSnapshotRequest(t, w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	manifest, err := SnapshotRemote(ts.URL, composePath, destDir)
	if err != nil {
		t.Fatalf("SnapshotRemote(): %v", err)
	}
	if manifest.StackName != stackName {
		t.Fatalf("manifest.StackName = %q, want %q", manifest.StackName, stackName)
	}
	if manifest.KernelPath != "/kernels/vmlinux" {
		t.Fatalf("manifest.KernelPath = %q", manifest.KernelPath)
	}
	if len(manifest.Services) != 2 || manifest.Services[0].Name != "db" || manifest.Services[1].Name != "web" {
		t.Fatalf("manifest.Services = %#v", manifest.Services)
	}
	if _, err := os.Stat(filepath.Join(destDir, StackSnapshotManifestName)); err != nil {
		t.Fatalf("manifest file missing: %v", err)
	}
}

func assertSnapshotRequest(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var req internalapi.SnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode snapshot request: %v", err)
	}
	if req.DestDir == "" {
		t.Fatal("snapshot dest_dir is empty")
	}
	if err := os.MkdirAll(req.DestDir, 0755); err != nil {
		t.Fatalf("create snapshot dir %s: %v", req.DestDir, err)
	}
	if !strings.Contains(req.DestDir, string(filepath.Separator)) {
		t.Fatalf("snapshot dest dir = %q, want a real path", req.DestDir)
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
