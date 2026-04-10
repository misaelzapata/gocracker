package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindDockerfile_PrefersShallowerPath(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	rootDockerfile := filepath.Join(root, "dockerfile")
	if err := os.WriteFile(rootDockerfile, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, contextDir, err := FindDockerfile(root)
	if err != nil {
		t.Fatalf("FindDockerfile: %v", err)
	}
	if path != rootDockerfile {
		t.Fatalf("path = %q, want %q", path, rootDockerfile)
	}
	if contextDir != root {
		t.Fatalf("contextDir = %q, want %q", contextDir, root)
	}
}

func TestFindCompose_ResolvesNearestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	composeDir := filepath.Join(root, "deploy")
	if err := os.MkdirAll(composeDir, 0755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(composeDir, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveComposePath(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("ResolveComposePath: %v", err)
	}
	if resolved != composePath {
		t.Fatalf("resolved = %q, want %q", resolved, composePath)
	}
}

func TestFindDockerfile_Ambiguous(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "services", "a"),
		filepath.Join(root, "services", "b"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := FindDockerfile(root)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFindDockerfile_PrefersRuntimeSignalsOverShallowerCandidate(t *testing.T) {
	root := t.TempDir()
	devcontainerDir := filepath.Join(root, ".devcontainer")
	serviceDir := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(devcontainerDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		t.Fatal(err)
	}
	devDockerfile := filepath.Join(devcontainerDir, "Dockerfile")
	if err := os.WriteFile(devDockerfile, []byte("FROM scratch\nRUN echo dev\n"), 0644); err != nil {
		t.Fatal(err)
	}
	serviceDockerfile := filepath.Join(serviceDir, "Dockerfile")
	if err := os.WriteFile(serviceDockerfile, []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, _, err := FindDockerfile(root)
	if err != nil {
		t.Fatalf("FindDockerfile: %v", err)
	}
	if path != serviceDockerfile {
		t.Fatalf("path = %q, want %q", path, serviceDockerfile)
	}
}

func TestFindCompose_PrefersCanonicalOverVariantAndDevcontainer(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, ".devcontainer"),
		filepath.Join(root, "deploy"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	devcontainerCompose := filepath.Join(root, ".devcontainer", "compose.dev.yaml")
	if err := os.WriteFile(devcontainerCompose, []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	canonicalCompose := filepath.Join(root, "deploy", "docker-compose.yml")
	if err := os.WriteFile(canonicalCompose, []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, err := FindCompose(root)
	if err != nil {
		t.Fatalf("FindCompose: %v", err)
	}
	if path != canonicalCompose {
		t.Fatalf("path = %q, want %q", path, canonicalCompose)
	}
}

func TestFindCompose_FindsVariantName(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "deploy", "compose.dev.yaml")
	if err := os.MkdirAll(filepath.Dir(composePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, err := FindCompose(root)
	if err != nil {
		t.Fatalf("FindCompose: %v", err)
	}
	if path != composePath {
		t.Fatalf("path = %q, want %q", path, composePath)
	}
}
