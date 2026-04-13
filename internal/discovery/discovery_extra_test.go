package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileRuntimeRank(t *testing.T) {
	dir := t.TempDir()

	writeDF := func(path string, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// File with ENTRYPOINT -> rank 0
	f1 := filepath.Join(dir, "df1")
	writeDF(f1, "FROM scratch\nENTRYPOINT [\"/app\"]\n")
	if got := dockerfileRuntimeRank(f1); got != 0 {
		t.Fatalf("ENTRYPOINT rank = %d, want 0", got)
	}

	// File with CMD -> rank 0
	f2 := filepath.Join(dir, "df2")
	writeDF(f2, "FROM scratch\nCMD [\"/app\"]\n")
	if got := dockerfileRuntimeRank(f2); got != 0 {
		t.Fatalf("CMD rank = %d, want 0", got)
	}

	// File with EXPOSE -> rank 1
	f3 := filepath.Join(dir, "df3")
	writeDF(f3, "FROM scratch\nEXPOSE 8080\n")
	if got := dockerfileRuntimeRank(f3); got != 1 {
		t.Fatalf("EXPOSE rank = %d, want 1", got)
	}

	// File with only FROM -> rank 2
	f4 := filepath.Join(dir, "df4")
	writeDF(f4, "FROM scratch\nRUN echo hello\n")
	if got := dockerfileRuntimeRank(f4); got != 2 {
		t.Fatalf("no runtime rank = %d, want 2", got)
	}

	// Missing file -> rank 2
	if got := dockerfileRuntimeRank("/nonexistent"); got != 2 {
		t.Fatalf("missing file rank = %d, want 2", got)
	}
}

func TestDockerfileRuntimeRankENTRYPOINTAtStart(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "df")
	if err := os.WriteFile(f, []byte("ENTRYPOINT [\"/app\"]\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := dockerfileRuntimeRank(f); got != 0 {
		t.Fatalf("ENTRYPOINT at start rank = %d, want 0", got)
	}
}

func TestDockerfileRuntimeRankCMDAtStart(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "df")
	if err := os.WriteFile(f, []byte("CMD [\"/app\"]\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := dockerfileRuntimeRank(f); got != 0 {
		t.Fatalf("CMD at start rank = %d, want 0", got)
	}
}

func TestDockerfileRuntimeRankEXPOSEAtStart(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "df")
	if err := os.WriteFile(f, []byte("EXPOSE 80\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := dockerfileRuntimeRank(f); got != 1 {
		t.Fatalf("EXPOSE at start rank = %d, want 1", got)
	}
}

func TestResolveComposePath_NonExistentNoAncestor(t *testing.T) {
	_, err := ResolveComposePath("/nonexistent/deeply/nested/compose.yml")
	if err == nil {
		t.Fatal("expected error for nonexistent path with no ancestor")
	}
}

func TestResolveComposePath_StatError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	dir := t.TempDir()
	noAccess := filepath.Join(dir, "noaccess")
	if err := os.MkdirAll(noAccess, 0000); err != nil {
		t.Fatalf("mkdir %s: %v", noAccess, err)
	}
	defer os.Chmod(noAccess, 0755)
	_, err := ResolveComposePath(filepath.Join(noAccess, "compose.yml"))
	if err == nil {
		t.Fatal("expected error for unreadable path")
	}
}

func TestFindOneNotDirectory(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := findOne(f, dockerfileNameRank, "test")
	if err == nil {
		t.Fatal("expected error for non-directory root")
	}
}

func TestFindOneNonExistent(t *testing.T) {
	_, err := findOne("/nonexistent/path", dockerfileNameRank, "test")
	if err == nil {
		t.Fatal("expected error for nonexistent root")
	}
}

func TestNearestExistingAncestorRoot(t *testing.T) {
	got := nearestExistingAncestor("/")
	if got != "/" {
		t.Fatalf("nearestExistingAncestor(/) = %q, want /", got)
	}
}

func TestNearestExistingAncestorEmpty(t *testing.T) {
	got := nearestExistingAncestor("")
	if got != "" {
		t.Fatalf("nearestExistingAncestor('') = %q, want empty", got)
	}
}

func TestCompareReverse(t *testing.T) {
	// Test the reverse direction of compare
	a := result{discouraged: 1, depth: 0}
	b := result{discouraged: 0, depth: 2}
	if got := compare(a, b); got != 1 {
		t.Fatalf("compare(a>b discouraged) = %d, want 1", got)
	}
}

func TestCompareRuntimeRankReverse(t *testing.T) {
	a := result{runtimeRank: 2, depth: 0}
	b := result{runtimeRank: 0, depth: 2}
	if got := compare(a, b); got != 1 {
		t.Fatalf("compare(a>b runtimeRank) = %d, want 1", got)
	}
}

func TestCompareDepthReverse(t *testing.T) {
	a := result{depth: 3}
	b := result{depth: 1}
	if got := compare(a, b); got != 1 {
		t.Fatalf("compare(a>b depth) = %d, want 1", got)
	}
}

func TestCompareNameRankReverse(t *testing.T) {
	a := result{nameRank: 3}
	b := result{nameRank: 1}
	if got := compare(a, b); got != 1 {
		t.Fatalf("compare(a>b nameRank) = %d, want 1", got)
	}
}

func TestFindDockerfilePrefersEntrypointOverExpose(t *testing.T) {
	root := t.TempDir()
	svcA := filepath.Join(root, "a")
	svcB := filepath.Join(root, "b")
	if err := os.MkdirAll(svcA, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", svcA, err)
	}
	if err := os.MkdirAll(svcB, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", svcB, err)
	}
	// a has EXPOSE only
	if err := os.WriteFile(filepath.Join(svcA, "Dockerfile"), []byte("FROM scratch\nEXPOSE 80\n"), 0644); err != nil {
		t.Fatalf("write a/Dockerfile: %v", err)
	}
	// b has ENTRYPOINT
	bDF := filepath.Join(svcB, "Dockerfile")
	if err := os.WriteFile(bDF, []byte("FROM scratch\nENTRYPOINT [\"/app\"]\n"), 0644); err != nil {
		t.Fatalf("write b/Dockerfile: %v", err)
	}
	path, _, err := FindDockerfile(root)
	if err != nil {
		t.Fatalf("FindDockerfile: %v", err)
	}
	if path != bDF {
		t.Fatalf("path = %q, want %q", path, bDF)
	}
}

func TestHasYAMLSuffix(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"file.yml", true},
		{"file.yaml", true},
		{"file.json", false},
		{"file.txt", false},
		{"compose.yml", true},
	}
	for _, tt := range tests {
		if got := hasYAMLSuffix(tt.name); got != tt.want {
			t.Errorf("hasYAMLSuffix(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestResolveComposePath_AncestorFallbackNoCompose(t *testing.T) {
	dir := t.TempDir()
	// No compose file anywhere
	_, err := ResolveComposePath(filepath.Join(dir, "nonexistent.yml"))
	if err == nil {
		t.Fatal("expected error when no compose found")
	}
	if !strings.Contains(err.Error(), "no Compose file found") {
		t.Fatalf("err = %v", err)
	}
}
