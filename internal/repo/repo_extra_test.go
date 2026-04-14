package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocateFilesExact_AllCandidates(t *testing.T) {
	// Test each Dockerfile candidate in priority order
	for _, name := range []string{"Dockerfile", "dockerfile", "Dockerfile.prod", "Dockerfile.production"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			os.WriteFile(path, []byte("FROM scratch\n"), 0644)
			r := &CloneResult{Dir: dir, ContextDir: dir}
			locateFilesExact(r)
			if r.DockerfilePath != path {
				t.Fatalf("DockerfilePath = %q, want %q", r.DockerfilePath, path)
			}
		})
	}
}

func TestLocateFilesExact_ComposeCandidates(t *testing.T) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			os.WriteFile(path, []byte("services: {}\n"), 0644)
			r := &CloneResult{Dir: dir, ContextDir: dir}
			locateFilesExact(r)
			if r.ComposePath != path {
				t.Fatalf("ComposePath = %q, want %q", r.ComposePath, path)
			}
		})
	}
}

// Regression: subdir="." should be treated as "walk the repo root", not as
// an explicit sub-target that pins locateFiles to an exact-root lookup.
// Previously, `gocracker repo --subdir .` missed Dockerfiles nested under
// subdirs (grafana/tempo's tools/Dockerfile) and broke the external-repos
// regression gate. Same for "./".
func TestIsExplicitSubdir(t *testing.T) {
	cases := map[string]bool{
		"":         false,
		".":        false,
		"./":       false,
		"docker":   true,
		"cmd/app":  true,
		"./docker": true, // explicit relative path IS a subdir
	}
	for input, want := range cases {
		if got := isExplicitSubdir(input); got != want {
			t.Errorf("isExplicitSubdir(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestLocateFilesRecursive(t *testing.T) {
	dir := t.TempDir()
	dfPath := filepath.Join(dir, "Dockerfile")
	os.WriteFile(dfPath, []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644)
	composePath := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(composePath, []byte("services: {}\n"), 0644)

	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFiles(r, false)
	if r.DockerfilePath != dfPath {
		t.Fatalf("DockerfilePath = %q, want %q", r.DockerfilePath, dfPath)
	}
	if r.ComposePath != composePath {
		t.Fatalf("ComposePath = %q, want %q", r.ComposePath, composePath)
	}
}

func TestSummary(t *testing.T) {
	r := &CloneResult{
		ContextDir:     "/tmp/test",
		DockerfilePath: "/tmp/test/Dockerfile",
		ComposePath:    "/tmp/test/docker-compose.yml",
	}
	// Should not panic
	r.Summary()
}

func TestSummaryNoDockerfile(t *testing.T) {
	r := &CloneResult{
		ContextDir: "/tmp/test",
	}
	// Should print warning but not panic
	r.Summary()
}

func TestResolveLocalWithExactSubdir(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "app")
	os.MkdirAll(subdir, 0755)
	dfPath := filepath.Join(subdir, "Dockerfile")
	os.WriteFile(dfPath, []byte("FROM scratch\n"), 0644)

	src := Source{URL: dir, Subdir: "app"}
	result, err := resolveLocal(src)
	if err != nil {
		t.Fatalf("resolveLocal: %v", err)
	}
	if result.ContextDir != subdir {
		t.Fatalf("ContextDir = %q, want %q", result.ContextDir, subdir)
	}
	if result.DockerfilePath != dfPath {
		t.Fatalf("DockerfilePath = %q, want %q", result.DockerfilePath, dfPath)
	}
}

func TestResolveLocalRelativePath(t *testing.T) {
	dir := t.TempDir()
	// Create a relative path
	src := Source{URL: dir}
	result, err := Resolve(src)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer result.Cleanup()
	if !result.IsLocal {
		t.Fatal("expected IsLocal=true")
	}
}

func TestLocateFilesExactPriority(t *testing.T) {
	dir := t.TempDir()
	// Create both Dockerfile and dockerfile; Dockerfile should win
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0644)
	os.WriteFile(filepath.Join(dir, "dockerfile"), []byte("FROM scratch\n"), 0644)
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if filepath.Base(r.DockerfilePath) != "Dockerfile" {
		t.Fatalf("DockerfilePath = %q, want Dockerfile (not dockerfile)", r.DockerfilePath)
	}
}

func TestLocateFilesExactComposePriority(t *testing.T) {
	dir := t.TempDir()
	// Create both docker-compose.yml and compose.yml
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0644)
	r := &CloneResult{Dir: dir, ContextDir: dir}
	locateFilesExact(r)
	if filepath.Base(r.ComposePath) != "docker-compose.yml" {
		t.Fatalf("ComposePath = %q, want docker-compose.yml", r.ComposePath)
	}
}

func TestSubdirPointsToDockerfileExtended(t *testing.T) {
	dir := t.TempDir()
	// Dockerfile.test should match
	os.WriteFile(filepath.Join(dir, "Dockerfile.test"), []byte("FROM scratch\n"), 0644)
	if !subdirPointsToDockerfile(filepath.Join(dir, "Dockerfile.test")) {
		t.Fatal("Dockerfile.test should be detected as dockerfile")
	}
}

func TestIsHexStringUpperAndLower(t *testing.T) {
	if !isHexString("aAbBcCdDeEfF0123456789") {
		t.Fatal("mixed case hex should be valid")
	}
}

func TestResolveWithNonexistentPath(t *testing.T) {
	_, err := Resolve(Source{URL: "/nonexistent/path/xyz"})
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestCloneRemote_LocalGitRepo(t *testing.T) {
	// Create a local git repo to clone
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	os.WriteFile(filepath.Join(repoDir, "Dockerfile"), []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "init")

	result, err := cloneRemote(Source{URL: repoDir, Depth: 1})
	if err != nil {
		t.Fatalf("cloneRemote: %v", err)
	}
	defer result.Cleanup()

	if result.IsLocal {
		t.Fatal("should not be local")
	}
	if result.DockerfilePath == "" {
		t.Fatal("expected DockerfilePath to be found")
	}
}

func TestCloneRemote_WithRef(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	os.WriteFile(filepath.Join(repoDir, "Dockerfile"), []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "init")
	runGit(t, repoDir, "tag", "v1.0.0")

	result, err := cloneRemote(Source{URL: repoDir, Ref: "v1.0.0", Depth: 1})
	if err != nil {
		t.Fatalf("cloneRemote: %v", err)
	}
	defer result.Cleanup()
	if result.DockerfilePath == "" {
		t.Fatal("expected DockerfilePath")
	}
}

func TestCloneRemote_WithSubdir(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	subDir := filepath.Join(repoDir, "app")
	os.MkdirAll(subDir, 0755)
	os.WriteFile(filepath.Join(subDir, "Dockerfile"), []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "init")

	result, err := cloneRemote(Source{URL: repoDir, Subdir: "app", Depth: 1})
	if err != nil {
		t.Fatalf("cloneRemote: %v", err)
	}
	defer result.Cleanup()
	if result.DockerfilePath == "" {
		t.Fatal("expected DockerfilePath in subdir")
	}
}

func TestCloneRemote_WithSubdirPointingToDockerfile(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	os.WriteFile(filepath.Join(repoDir, "Dockerfile.dev"), []byte("FROM scratch\n"), 0644)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "init")

	result, err := cloneRemote(Source{URL: repoDir, Subdir: "Dockerfile.dev", Depth: 1})
	if err != nil {
		t.Fatalf("cloneRemote: %v", err)
	}
	defer result.Cleanup()
	if result.DockerfilePath == "" {
		t.Fatal("expected DockerfilePath when subdir points to Dockerfile")
	}
}

func TestCloneRemote_ShortSHA(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	os.WriteFile(filepath.Join(repoDir, "Dockerfile"), []byte("FROM scratch\nCMD [\"/app\"]\n"), 0644)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "init")

	// Get the short SHA
	out := runGitOutput(t, repoDir, "rev-parse", "--short=7", "HEAD")
	shortSHA := out

	result, err := cloneRemote(Source{URL: repoDir, Ref: shortSHA})
	if err != nil {
		t.Fatalf("cloneRemote(short SHA): %v", err)
	}
	defer result.Cleanup()
}

func TestCloneRemote_InvalidRef(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	os.WriteFile(filepath.Join(repoDir, "file"), []byte("x"), 0644)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "init")

	_, err := cloneRemote(Source{URL: repoDir, Ref: "nonexistent-branch-xyz", Depth: 1})
	if err == nil {
		t.Fatal("expected error for invalid ref")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := filepath.Join("/usr", "bin", "git")
	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := filepath.Join("/usr", "bin", "git")
	c := exec.Command(cmd, args...)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
