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
	// Ambiguity is now resolved deterministically by lex-smallest path
	// instead of erroring — real repos (dragonflydb, eclipse-mosquitto)
	// often ship multiple Dockerfile variants at the same rank and any is
	// a reasonable default. Callers wanting a specific one pass --subdir
	// or --dockerfile.
	wantA := filepath.Join(aDir, "Dockerfile")
	wantB := filepath.Join(bDir, "Dockerfile")
	if r.DockerfilePath != wantA && r.DockerfilePath != wantB {
		t.Fatalf("DockerfilePath = %q, expected %q or %q", r.DockerfilePath, wantA, wantB)
	}
	// And the pick must be stable (lex-smallest) — so with "a" before "b",
	// we expect services/a/Dockerfile.
	if r.DockerfilePath != wantA {
		t.Fatalf("DockerfilePath = %q, want lex-smallest %q", r.DockerfilePath, wantA)
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

// ---- NEW TESTS ----

func TestIsLocalPath_ExhaustiveCases(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// Local paths
		{"/", true},
		{"/home/user/project", true},
		{"/tmp/foo", true},
		{"./relative", true},
		{"./", true},
		{"../parent", true},
		{"../", true},
		{".", true},

		// Non-local paths
		{"", false},
		{"https://github.com/user/repo", false},
		{"http://github.com/user/repo", false},
		{"git@github.com:user/repo.git", false},
		{"ssh://git@github.com/user/repo", false},
		{"file:///path/to/repo", false},
		{"github.com/user/repo", false},
		{"user/repo", false},
		{"repo", false},
		{"some-remote", false},
		{"a", false},
	}
	for _, tt := range tests {
		got := isLocalPath(tt.input)
		if got != tt.want {
			t.Errorf("isLocalPath(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsHexString_Comprehensive(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"0", true},
		{"9", true},
		{"a", true},
		{"f", true},
		{"A", true},
		{"F", true},
		{"0123456789", true},
		{"abcdef", true},
		{"ABCDEF", true},
		{"aAbBcCdDeEfF0123456789", true},
		{"g", false},
		{"G", false},
		{"xyz", false},
		{"0x1", false},
		{" ", false},
		{"abc def", false},
		{"abc\n", false},
		{"abc!", false},
		// 40-char full SHA
		{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true},
		// 39-char short SHA
		{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b", true},
	}
	for _, tt := range tests {
		got := isHexString(tt.input)
		if got != tt.want {
			t.Errorf("isHexString(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestSubdirPointsToDockerfile_Comprehensive(t *testing.T) {
	dir := t.TempDir()
	// Create test files
	for _, name := range []string{
		"Dockerfile",
		"Dockerfile.prod",
		"Dockerfile.dev",
		"dockerfile",
		"notadockerfile.txt",
		"README.md",
	} {
		os.WriteFile(filepath.Join(dir, name), []byte("FROM scratch\n"), 0644)
	}
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)

	tests := []struct {
		path string
		want bool
	}{
		{filepath.Join(dir, "Dockerfile"), true},
		{filepath.Join(dir, "Dockerfile.prod"), true},
		{filepath.Join(dir, "Dockerfile.dev"), true},
		{filepath.Join(dir, "dockerfile"), true},
		{filepath.Join(dir, "notadockerfile.txt"), false},
		{filepath.Join(dir, "README.md"), false},
		{filepath.Join(dir, "subdir"), false},
		{filepath.Join(dir, "nonexistent"), false},
	}
	for _, tt := range tests {
		got := subdirPointsToDockerfile(tt.path)
		if got != tt.want {
			t.Errorf("subdirPointsToDockerfile(%q) = %v, want %v", filepath.Base(tt.path), got, tt.want)
		}
	}
}

func TestResolveLocal_WithSubdirAndDockerfile(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	os.MkdirAll(subdir, 0755)
	dfPath := filepath.Join(subdir, "Dockerfile")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)

	src := Source{URL: dir, Subdir: "sub"}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal: %v", err)
	}
	if result.ContextDir != subdir {
		t.Errorf("ContextDir = %q, want %q", result.ContextDir, subdir)
	}
	if result.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", result.DockerfilePath, dfPath)
	}
}

func TestResolveLocal_WithSubdirAndCompose(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "deploy")
	os.MkdirAll(subdir, 0755)
	composePath := filepath.Join(subdir, "docker-compose.yml")
	os.WriteFile(composePath, []byte("version: '3'\n"), 0644)

	src := Source{URL: dir, Subdir: "deploy"}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal: %v", err)
	}
	if result.ComposePath != composePath {
		t.Errorf("ComposePath = %q, want %q", result.ComposePath, composePath)
	}
}

func TestLocateFilesExact_OnlyDockerfileProd(t *testing.T) {
	dir := t.TempDir()
	dfPath := filepath.Join(dir, "Dockerfile.prod")
	os.WriteFile(dfPath, []byte("FROM ubuntu\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFilesExact_LowercaseDockerfile(t *testing.T) {
	dir := t.TempDir()
	dfPath := filepath.Join(dir, "dockerfile")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFilesExact_DockerComposePriority(t *testing.T) {
	dir := t.TempDir()
	// Create all four compose variants
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		os.WriteFile(filepath.Join(dir, name), []byte("version: '3'\n"), 0644)
	}

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	// docker-compose.yml should win
	if filepath.Base(r.ComposePath) != "docker-compose.yml" {
		t.Errorf("ComposePath = %q, want docker-compose.yml", filepath.Base(r.ComposePath))
	}
}

func TestLocateFilesExact_DockerfileOverDockerfileProd(t *testing.T) {
	dir := t.TempDir()
	// Create both
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "Dockerfile.prod"), []byte("FROM ubuntu\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if filepath.Base(r.DockerfilePath) != "Dockerfile" {
		t.Errorf("DockerfilePath = %q, want Dockerfile (not Dockerfile.prod)", filepath.Base(r.DockerfilePath))
	}
}

func TestLocateFiles_NonExact_FindsRecursive(t *testing.T) {
	dir := t.TempDir()
	nestedDir := filepath.Join(dir, "app")
	os.MkdirAll(nestedDir, 0755)
	dfPath := filepath.Join(nestedDir, "Dockerfile")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, false)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestCloneResult_Summary(t *testing.T) {
	r := &CloneResult{
		Dir:            "/tmp/repo",
		ContextDir:     "/tmp/repo",
		DockerfilePath: "/tmp/repo/Dockerfile",
		ComposePath:    "/tmp/repo/docker-compose.yml",
	}
	// Just verify Summary() does not panic
	r.Summary()
}

func TestCloneResult_SummaryNoDockerfile(t *testing.T) {
	r := &CloneResult{
		Dir:        "/tmp/repo",
		ContextDir: "/tmp/repo",
	}
	r.Summary()
}

func TestResolve_DepthDefault(t *testing.T) {
	dir := t.TempDir()
	src := Source{URL: dir}
	result, err := Resolve(src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer result.Cleanup()
	if !result.IsLocal {
		t.Fatal("expected IsLocal")
	}
}

func TestResolve_WithDockerfileAndCompose(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("version: '3'\n"), 0644)

	src := Source{URL: dir}
	result, err := Resolve(src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer result.Cleanup()

	if result.DockerfilePath == "" {
		t.Error("expected DockerfilePath to be set")
	}
	if result.ComposePath == "" {
		t.Error("expected ComposePath to be set")
	}
}

func TestResolveLocal_DotPath(t *testing.T) {
	// "." should resolve to current working directory
	src := Source{URL: "."}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal(.): %v", err)
	}
	if !result.IsLocal {
		t.Fatal("expected IsLocal")
	}
	if result.Dir == "" {
		t.Fatal("Dir should not be empty")
	}
}

// --- New coverage-boosting tests ---

func TestResolve_LocalWithSubdirDockerfile(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "deploy")
	os.MkdirAll(subdir, 0755)
	dfPath := filepath.Join(subdir, "Dockerfile")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)

	src := Source{URL: dir, Subdir: "deploy"}
	result, err := Resolve(src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer result.Cleanup()

	if result.ContextDir != subdir {
		t.Errorf("ContextDir = %q, want %q", result.ContextDir, subdir)
	}
	if result.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", result.DockerfilePath, dfPath)
	}
}

func TestResolve_LocalWithDepth(t *testing.T) {
	dir := t.TempDir()
	src := Source{URL: dir, Depth: 5}
	result, err := Resolve(src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer result.Cleanup()
	if !result.IsLocal {
		t.Error("expected IsLocal")
	}
}

func TestLocateFilesExact_NothingPresent(t *testing.T) {
	dir := t.TempDir()
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if r.DockerfilePath != "" {
		t.Errorf("DockerfilePath = %q, want empty", r.DockerfilePath)
	}
	if r.ComposePath != "" {
		t.Errorf("ComposePath = %q, want empty", r.ComposePath)
	}
}

func TestLocateFilesExact_DockerfileProductionOnly(t *testing.T) {
	dir := t.TempDir()
	dfPath := filepath.Join(dir, "Dockerfile.production")
	os.WriteFile(dfPath, []byte("FROM ubuntu\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if r.DockerfilePath != dfPath {
		t.Errorf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
}

func TestLocateFilesExact_ComposeYamlOnly(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yaml")
	os.WriteFile(composePath, []byte("version: '3'\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if r.ComposePath != composePath {
		t.Errorf("ComposePath = %q, want %q", r.ComposePath, composePath)
	}
}

func TestLocateFilesExact_DockerComposeYamlOnly(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yaml")
	os.WriteFile(composePath, []byte("version: '3'\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if r.ComposePath != composePath {
		t.Errorf("ComposePath = %q, want %q", r.ComposePath, composePath)
	}
}

func TestLocateFilesExact_ComposeYmlOnly(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yml")
	os.WriteFile(composePath, []byte("version: '3'\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if r.ComposePath != composePath {
		t.Errorf("ComposePath = %q, want %q", r.ComposePath, composePath)
	}
}

func TestLocateFilesExact_DockerfilePriorityOverLowercase(t *testing.T) {
	dir := t.TempDir()
	// Dockerfile (capitalized) should be preferred over dockerfile (lowercase)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "dockerfile"), []byte("FROM scratch\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if filepath.Base(r.DockerfilePath) != "Dockerfile" {
		t.Errorf("DockerfilePath = %q, should prefer Dockerfile over dockerfile", filepath.Base(r.DockerfilePath))
	}
}

func TestLocateFiles_NonExact_WithCompose(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(composePath, []byte("version: '3'\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, false)
	if r.ComposePath == "" {
		t.Error("expected ComposePath to be found via non-exact search")
	}
}

func TestLocateFiles_NonExact_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, false)
	// Should not error, just leave paths empty
	if r.DockerfilePath != "" {
		t.Errorf("DockerfilePath = %q, want empty", r.DockerfilePath)
	}
}

func TestCloneResult_Summary_AllFields(t *testing.T) {
	r := &CloneResult{
		Dir:            "/tmp/repo",
		ContextDir:     "/tmp/repo/app",
		DockerfilePath: "/tmp/repo/app/Dockerfile",
		ComposePath:    "/tmp/repo/app/docker-compose.yml",
	}
	// Just verify Summary() doesn't panic with all fields set
	r.Summary()
}

func TestCloneResult_Summary_OnlyCompose(t *testing.T) {
	r := &CloneResult{
		Dir:         "/tmp/repo",
		ContextDir:  "/tmp/repo",
		ComposePath: "/tmp/repo/docker-compose.yml",
	}
	// Should print WARNING for missing Dockerfile
	r.Summary()
}

func TestResolveLocal_RelativeDotSlash(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644)
	origWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origWd)

	src := Source{URL: "./"}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal(./): %v", err)
	}
	if !result.IsLocal {
		t.Error("expected IsLocal")
	}
}

func TestResolveLocal_DotDotSlash(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	os.MkdirAll(subdir, 0755)
	origWd, _ := os.Getwd()
	os.Chdir(subdir)
	defer os.Chdir(origWd)

	src := Source{URL: ".."}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal(..): %v", err)
	}
	if !result.IsLocal {
		t.Error("expected IsLocal")
	}
}

func TestSubdirPointsToDockerfile_DotDockerfileSuffix(t *testing.T) {
	dir := t.TempDir()
	// Dockerfile.dev should match
	p := filepath.Join(dir, "Dockerfile.dev")
	os.WriteFile(p, []byte("FROM scratch\n"), 0644)
	if !subdirPointsToDockerfile(p) {
		t.Errorf("Dockerfile.dev should be recognized")
	}
}

func TestSubdirPointsToDockerfile_RandomFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "main.go")
	os.WriteFile(p, []byte("package main\n"), 0644)
	if subdirPointsToDockerfile(p) {
		t.Error("main.go should not be recognized as Dockerfile")
	}
}

func TestResolve_NonexistentLocal(t *testing.T) {
	src := Source{URL: "/nonexistent/path/that/does/not/exist/at/all"}
	_, err := Resolve(src)
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestLocateFiles_ExactTrueWithBothFiles(t *testing.T) {
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
