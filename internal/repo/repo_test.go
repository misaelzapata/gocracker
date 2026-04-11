package repo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsLocalPath(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"/home/user/project", true},
		{"/tmp/foo", true},
		{"./relative", true},
		{"../parent", true},
		{".", true},
		{"https://github.com/user/repo", false},
		{"git@github.com:user/repo.git", false},
		{"github.com/user/repo", false},
		{"some-remote", false},
	}
	for _, tt := range tests {
		got := isLocalPath(tt.input)
		if got != tt.want {
			t.Errorf("isLocalPath(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestResolveLocal_ValidDir(t *testing.T) {
	dir := t.TempDir()
	src := Source{URL: dir}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal: %v", err)
	}
	if !result.IsLocal {
		t.Error("IsLocal should be true")
	}
	if result.Dir != dir {
		t.Errorf("Dir = %q, want %q", result.Dir, dir)
	}
	// Cleanup should be a no-op
	result.Cleanup()
}

func TestResolveLocal_NonexistentDir(t *testing.T) {
	src := Source{URL: "/nonexistent/path/that/does/not/exist"}
	_, err := resolveLocal(src)
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestResolveLocal_WithSubdir(t *testing.T) {
	dir := t.TempDir()
	subdir := "services/web"
	os.MkdirAll(filepath.Join(dir, subdir), 0755)

	src := Source{URL: dir, Subdir: subdir}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal: %v", err)
	}
	want := filepath.Join(dir, subdir)
	if result.ContextDir != want {
		t.Errorf("ContextDir = %q, want %q", result.ContextDir, want)
	}
}

func TestLocateFiles_Dockerfile(t *testing.T) {
	dir := t.TempDir()
	// Create a Dockerfile
	dfPath := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFiles_DockerfileLowercase(t *testing.T) {
	dir := t.TempDir()
	dfPath := filepath.Join(dir, "dockerfile")
	if err := os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFiles_DockerCompose(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("version: '3'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.ComposePath != composePath {
		t.Errorf("ComposePath = %q, want %q", r.ComposePath, composePath)
	}
}

func TestLocateFiles_ComposeYAML(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("version: '3'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.ComposePath != composePath {
		t.Errorf("ComposePath = %q, want %q", r.ComposePath, composePath)
	}
}

func TestLocateFiles_NoFiles(t *testing.T) {
	dir := t.TempDir()
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.DockerfilePath != "" {
		t.Errorf("DockerfilePath should be empty, got %q", r.DockerfilePath)
	}
	if r.ComposePath != "" {
		t.Errorf("ComposePath should be empty, got %q", r.ComposePath)
	}
}

func TestLocateFiles_Priority(t *testing.T) {
	dir := t.TempDir()
	// Create both Dockerfile and Dockerfile.prod
	// Dockerfile should be preferred
	dfPath := filepath.Join(dir, "Dockerfile")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "Dockerfile.prod"), []byte("FROM ubuntu\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q (should prefer Dockerfile over Dockerfile.prod)",
			r.DockerfilePath, dfPath)
	}
}

func TestLocateFiles_BothDockerfileAndCompose(t *testing.T) {
	dir := t.TempDir()
	dfPath := filepath.Join(dir, "Dockerfile")
	composePath := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)
	os.WriteFile(composePath, []byte("version: '3'\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
	if r.ComposePath != composePath {
		t.Errorf("ComposePath = %q, want %q", r.ComposePath, composePath)
	}
}

func TestResolve_LocalPath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644)

	src := Source{URL: dir}
	result, err := Resolve(src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer result.Cleanup()

	if !result.IsLocal {
		t.Error("expected IsLocal=true")
	}
	if result.DockerfilePath == "" {
		t.Error("expected DockerfilePath to be set")
	}
}

func TestResolve_DefaultDepth(t *testing.T) {
	// Verify that Resolve sets depth=1 when 0
	dir := t.TempDir()
	src := Source{URL: dir, Depth: 0}
	_, err := Resolve(src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The function internally checks src.Depth == 0 and sets to 1.
	// We just verify it doesn't break.
}

func TestLocateFiles_RecursiveDockerfileKeepsRootContext(t *testing.T) {
	dir := t.TempDir()
	serviceDir := filepath.Join(dir, "services", "api")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		t.Fatal(err)
	}
	dfPath := filepath.Join(serviceDir, "Dockerfile")
	if err := os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, false)
	if r.DockerfilePath != dfPath {
		t.Fatalf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
	if r.ContextDir != dir {
		t.Fatalf("ContextDir = %q, want %q", r.ContextDir, dir)
	}
}

func TestLocateFiles_RecursiveSkipsDiscouragedDirectories(t *testing.T) {
	dir := t.TempDir()
	examplesDir := filepath.Join(dir, "examples", "demo")
	serviceDir := filepath.Join(dir, "services", "api")
	if err := os.MkdirAll(examplesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(examplesDir, "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dfPath := filepath.Join(serviceDir, "Dockerfile")
	if err := os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, false)
	if r.DockerfilePath != dfPath {
		t.Fatalf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFiles_RecursiveAmbiguousDockerfiles(t *testing.T) {
	dir := t.TempDir()
	aDir := filepath.Join(dir, "services", "a")
	bDir := filepath.Join(dir, "services", "b")
	if err := os.MkdirAll(aDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bDir, "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, false)
	if r.DockerfilePath != "" {
		t.Fatalf("DockerfilePath = %q, want empty due to ambiguity", r.DockerfilePath)
	}
}

func TestIsLocalPathExtended(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"/", true},
		{"/a/b/c", true},
		{"./a", true},
		{"../a/b", true},
		{".", true},
		{"", false},
		{"http://example.com/repo", false},
		{"ssh://git@github.com/user/repo", false},
		{"git@github.com:user/repo.git", false},
		{"github.com/user/repo", false},
		{"user/repo", false},
		{"repo", false},
		{"file:///path/to/repo", false}, // file:// is not a local path in this logic
	}
	for _, tc := range tests {
		got := isLocalPath(tc.input)
		if got != tc.want {
			t.Errorf("isLocalPath(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestIsHexString(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"0", true},
		{"a", true},
		{"f", true},
		{"A", true},
		{"F", true},
		{"0123456789abcdef", true},
		{"0123456789ABCDEF", true},
		{"abc123", true},
		{"g", false},
		{"xyz", false},
		{"0x1a", false}, // 'x' is not hex
		{"abcdefg", false},
		{" ", false},
		{"abc def", false},
		// Full SHA-like
		{"a" + "b" + "c" + "d" + "e" + "f" + "0123456789abcdef0123456789abcdef01", true},
	}
	for _, tc := range tests {
		got := isHexString(tc.input)
		if got != tc.want {
			t.Errorf("isHexString(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestSubdirPointsToDockerfile(t *testing.T) {
	dir := t.TempDir()

	// Create various files
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "Dockerfile.prod"), []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "dockerfile"), []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "notadockerfile"), []byte("FROM scratch\n"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)

	tests := []struct {
		path string
		want bool
	}{
		{filepath.Join(dir, "Dockerfile"), true},
		{filepath.Join(dir, "Dockerfile.prod"), true},
		{filepath.Join(dir, "dockerfile"), true},
		{filepath.Join(dir, "notadockerfile"), false},
		{filepath.Join(dir, "subdir"), false},            // directory
		{filepath.Join(dir, "nonexistent"), false},        // doesn't exist
	}
	for _, tc := range tests {
		got := subdirPointsToDockerfile(tc.path)
		if got != tc.want {
			t.Errorf("subdirPointsToDockerfile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestResolveLocal_WithSubdirAsDockerfile(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "docker")
	os.MkdirAll(subdir, 0755)
	dfPath := filepath.Join(subdir, "Dockerfile.dev")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)

	src := Source{URL: dir, Subdir: "docker"}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal: %v", err)
	}
	if result.ContextDir != subdir {
		t.Errorf("ContextDir = %q, want %q", result.ContextDir, subdir)
	}
}

func TestCloneResultCleanupNoop(t *testing.T) {
	// Cleanup with nil cleanup function should not panic
	r := &CloneResult{}
	r.Cleanup() // should be a no-op
}

func TestCloneResultCleanupCalled(t *testing.T) {
	called := false
	r := &CloneResult{
		cleanup: func() { called = true },
	}
	r.Cleanup()
	if !called {
		t.Error("cleanup function was not called")
	}
}

func TestSourceDefaultDepth(t *testing.T) {
	src := Source{URL: "/tmp", Depth: 0}
	// Resolve sets depth=1 when 0. We just verify the struct field.
	if src.Depth != 0 {
		t.Errorf("initial Depth = %d, want 0", src.Depth)
	}
}

func TestLocateFilesExact_ComposePriority(t *testing.T) {
	dir := t.TempDir()
	// Create both docker-compose.yml and compose.yml
	// docker-compose.yml should be preferred
	dcPath := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(dcPath, []byte("version: '3'\n"), 0644)
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("version: '3'\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.ComposePath != dcPath {
		t.Errorf("ComposePath = %q, want %q (should prefer docker-compose.yml)", r.ComposePath, dcPath)
	}
}

func TestLocateFilesExact_DockerfileProduction(t *testing.T) {
	dir := t.TempDir()
	// Only Dockerfile.production present
	dfPath := filepath.Join(dir, "Dockerfile.production")
	os.WriteFile(dfPath, []byte("FROM ubuntu\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFilesExact_DockerfileProd(t *testing.T) {
	dir := t.TempDir()
	dfPath := filepath.Join(dir, "Dockerfile.prod")
	os.WriteFile(dfPath, []byte("FROM ubuntu\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, true)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFilesExact_ComposeYAMLVariants(t *testing.T) {
	tests := []struct {
		name     string
		filename string
	}{
		{"docker-compose.yml", "docker-compose.yml"},
		{"docker-compose.yaml", "docker-compose.yaml"},
		{"compose.yml", "compose.yml"},
		{"compose.yaml", "compose.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, tc.filename)
			os.WriteFile(p, []byte("version: '3'\n"), 0644)

			r := &CloneResult{Dir: dir, ContextDir: dir}
			locateFiles(r, true)
			if r.ComposePath != p {
				t.Errorf("ComposePath = %q, want %q", r.ComposePath, p)
			}
		})
	}
}
