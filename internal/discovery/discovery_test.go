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

func TestFindDockerfile_NoDockerfile(t *testing.T) {
	root := t.TempDir()
	_, _, err := FindDockerfile(root)
	if err == nil {
		t.Fatal("expected error when no Dockerfile found")
	}
	if !strings.Contains(err.Error(), "no Dockerfile found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFindCompose_NoComposeFile(t *testing.T) {
	root := t.TempDir()
	_, err := FindCompose(root)
	if err == nil {
		t.Fatal("expected error when no compose file found")
	}
	if !strings.Contains(err.Error(), "no Compose file found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFindDockerfile_SkipsIgnoredDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"node_modules", "vendor", ".git"} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "Dockerfile"), []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Place the real Dockerfile at root
	rootDF := filepath.Join(root, "Dockerfile")
	if err := os.WriteFile(rootDF, []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, _, err := FindDockerfile(root)
	if err != nil {
		t.Fatalf("FindDockerfile: %v", err)
	}
	if path != rootDF {
		t.Fatalf("path = %q, want %q", path, rootDF)
	}
}

func TestFindDockerfile_PrefersCanonicalNameOverVariant(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "Dockerfile")
	variant := filepath.Join(root, "Dockerfile.prod")
	if err := os.WriteFile(canonical, []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(variant, []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, _, err := FindDockerfile(root)
	if err != nil {
		t.Fatalf("FindDockerfile: %v", err)
	}
	if path != canonical {
		t.Fatalf("path = %q, want canonical %q", path, canonical)
	}
}

func TestFindCompose_AllVariantNames(t *testing.T) {
	names := []string{
		"docker-compose.yml",
		"docker-compose.yaml",
		"compose.yml",
		"compose.yaml",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			composePath := filepath.Join(root, name)
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
		})
	}
}

func TestFindCompose_AmbiguousSameDepth(t *testing.T) {
	root := t.TempDir()
	// Two compose files at the same depth in sibling dirs
	for _, dir := range []string{
		filepath.Join(root, "svc-a"),
		filepath.Join(root, "svc-b"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := FindCompose(root)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerfileNameRank(t *testing.T) {
	tests := []struct {
		name    string
		want    int
		wantOK  bool
	}{
		{"Dockerfile", 0, true},
		{"dockerfile", 1, true},
		{"Dockerfile.prod", 2, true},
		{"Dockerfile.production", 3, true},
		{"Dockerfile.custom", len(dockerfileNames) + 1, true},
		{"dockerfile.dev", len(dockerfileNames) + 1, true},
		{"Makefile", 0, false},
		{"README.md", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rank, ok := dockerfileNameRank(tt.name)
			if ok != tt.wantOK {
				t.Fatalf("dockerfileNameRank(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
			}
			if ok && rank != tt.want {
				t.Fatalf("dockerfileNameRank(%q) = %d, want %d", tt.name, rank, tt.want)
			}
		})
	}
}

func TestComposeNameRank(t *testing.T) {
	tests := []struct {
		name   string
		want   int
		wantOK bool
	}{
		{"docker-compose.yml", 0, true},
		{"docker-compose.yaml", 1, true},
		{"compose.yml", 2, true},
		{"compose.yaml", 3, true},
		{"compose.dev.yml", 10, true},
		{"docker-compose.prod.yaml", 10, true},
		{"Makefile", 0, false},
		{"docker-compose.json", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rank, ok := composeNameRank(tt.name)
			if ok != tt.wantOK {
				t.Fatalf("composeNameRank(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
			}
			if ok && rank != tt.want {
				t.Fatalf("composeNameRank(%q) = %d, want %d", tt.name, rank, tt.want)
			}
		})
	}
}

func TestPathDepth(t *testing.T) {
	tests := []struct {
		relDir string
		want   int
	}{
		{".", 0},
		{"", 0},
		{"services", 1},
		{"services/api", 2},
		{"a/b/c/d", 4},
	}
	for _, tt := range tests {
		t.Run(tt.relDir, func(t *testing.T) {
			got := pathDepth(tt.relDir)
			if got != tt.want {
				t.Fatalf("pathDepth(%q) = %d, want %d", tt.relDir, got, tt.want)
			}
		})
	}
}

func TestDiscouragedCount(t *testing.T) {
	tests := []struct {
		relDir string
		want   int
	}{
		{".", 0},
		{"", 0},
		{"examples", 1},
		{"docs/api", 1},
		{".github/workflows", 1},
		{"tests/examples", 2},
		{"src/main", 0},
	}
	for _, tt := range tests {
		t.Run(tt.relDir, func(t *testing.T) {
			got := discouragedCount(tt.relDir)
			if got != tt.want {
				t.Fatalf("discouragedCount(%q) = %d, want %d", tt.relDir, got, tt.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b result
		want int
	}{
		{
			name: "same result",
			a:    result{depth: 1, nameRank: 0},
			b:    result{depth: 1, nameRank: 0},
			want: 0,
		},
		{
			name: "lower discouraged wins",
			a:    result{discouraged: 0, depth: 2},
			b:    result{discouraged: 1, depth: 0},
			want: -1,
		},
		{
			name: "lower runtime rank wins",
			a:    result{runtimeRank: 0, depth: 2},
			b:    result{runtimeRank: 2, depth: 0},
			want: -1,
		},
		{
			name: "shallower depth wins",
			a:    result{depth: 0, nameRank: 1},
			b:    result{depth: 2, nameRank: 0},
			want: -1,
		},
		{
			name: "lower name rank wins at same depth",
			a:    result{depth: 1, nameRank: 0},
			b:    result{depth: 1, nameRank: 1},
			want: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compare(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("compare() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveComposePath_ExistingFile(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveComposePath(composePath)
	if err != nil {
		t.Fatalf("ResolveComposePath: %v", err)
	}
	if got != composePath {
		t.Fatalf("path = %q, want %q", got, composePath)
	}
}

func TestResolveComposePath_Directory(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveComposePath(root)
	if err != nil {
		t.Fatalf("ResolveComposePath: %v", err)
	}
	if got != composePath {
		t.Fatalf("path = %q, want %q", got, composePath)
	}
}

func TestNearestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	got := nearestExistingAncestor(filepath.Join(sub, "nonexistent", "path"))
	if got != sub {
		t.Fatalf("nearestExistingAncestor() = %q, want %q", got, sub)
	}
}

func TestFindDockerfile_DiscouragedDirDeprioritized(t *testing.T) {
	root := t.TempDir()
	// Dockerfile in discouraged "tests" dir
	testsDir := filepath.Join(root, "tests")
	if err := os.MkdirAll(testsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "Dockerfile"), []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Dockerfile in normal dir at deeper path
	svcDir := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(svcDir, 0755); err != nil {
		t.Fatal(err)
	}
	svcDF := filepath.Join(svcDir, "Dockerfile")
	if err := os.WriteFile(svcDF, []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, _, err := FindDockerfile(root)
	if err != nil {
		t.Fatalf("FindDockerfile: %v", err)
	}
	if path != svcDF {
		t.Fatalf("path = %q, want %q (non-discouraged dir should win)", path, svcDF)
	}
}
