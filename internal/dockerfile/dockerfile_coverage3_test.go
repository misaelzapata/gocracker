package dockerfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocracker/gocracker/internal/oci"
)

// --- hasDockerfileInstructions ---

func TestHasDockerfileInstructions_CommentsOnly(t *testing.T) {
	if hasDockerfileInstructions("# comment\n# another\n") {
		t.Fatal("only comments should return false")
	}
}

func TestHasDockerfileInstructions_BlankLines(t *testing.T) {
	if hasDockerfileInstructions("\n\n   \n  \n") {
		t.Fatal("blank lines should return false")
	}
}

// --- parseHealthcheck ---

func TestParseHealthcheck_CMDWithAllOptions(t *testing.T) {
	hc, err := parseHealthcheck([]string{"--interval=30s", "--timeout=10s", "--start-period=5s", "--start-interval=2s", "--retries=3", "CMD", "curl", "-f", "http://localhost/"})
	if err != nil {
		t.Fatal(err)
	}
	if hc.Interval.Seconds() != 30 {
		t.Fatalf("interval = %v", hc.Interval)
	}
	if hc.Timeout.Seconds() != 10 {
		t.Fatalf("timeout = %v", hc.Timeout)
	}
	if hc.StartPeriod.Seconds() != 5 {
		t.Fatalf("start-period = %v", hc.StartPeriod)
	}
	if hc.StartInterval.Seconds() != 2 {
		t.Fatalf("start-interval = %v", hc.StartInterval)
	}
	if hc.Retries != 3 {
		t.Fatalf("retries = %d", hc.Retries)
	}
	if len(hc.Test) < 2 || hc.Test[0] != "CMD" {
		t.Fatalf("test = %v", hc.Test)
	}
}

func TestParseHealthcheck_BadRetries(t *testing.T) {
	_, err := parseHealthcheck([]string{"--retries=xyz", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for bad retries")
	}
}

// --- envMap ---

func TestEnvMap_Parses(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/root", "FOO=bar=baz"}
	m, order := envMap(env)
	if m["PATH"] != "/usr/bin" || m["HOME"] != "/root" || m["FOO"] != "bar=baz" {
		t.Fatalf("map = %v", m)
	}
	if len(order) != 3 {
		t.Fatalf("order = %v", order)
	}
}

func TestEnvMap_SkipsInvalid(t *testing.T) {
	env := []string{"VALID=ok", "NOEQUALSSIGN", "ALSO_VALID=yes"}
	m, order := envMap(env)
	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %v", m)
	}
	if len(order) != 2 {
		t.Fatalf("order = %v", order)
	}
}

func TestEnvMap_DuplicateOverwrites(t *testing.T) {
	env := []string{"KEY=first", "KEY=second"}
	m, order := envMap(env)
	if m["KEY"] != "second" {
		t.Fatalf("expected second value, got %q", m["KEY"])
	}
	if len(order) != 1 {
		t.Fatalf("order = %v (should not duplicate)", order)
	}
}

// --- cloneStringSlice ---

func TestCloneStringSlice_DeepCopy(t *testing.T) {
	src := []string{"a", "b", "c"}
	dst := cloneStringSlice(src)
	dst[0] = "x"
	if src[0] != "a" {
		t.Fatal("clone modified original")
	}
}

// --- cloneStringMap ---

func TestCloneStringMap_DeepCopy(t *testing.T) {
	src := map[string]string{"a": "1", "b": "2"}
	dst := cloneStringMap(src)
	dst["a"] = "99"
	if src["a"] != "1" {
		t.Fatal("clone modified original")
	}
}

// --- appendUnique ---

func TestAppendUnique_New(t *testing.T) {
	got := appendUnique([]string{"a", "b"}, "c")
	if len(got) != 3 || got[2] != "c" {
		t.Fatalf("got %v", got)
	}
}

func TestAppendUnique_Existing(t *testing.T) {
	got := appendUnique([]string{"a", "b"}, "a")
	if len(got) != 2 {
		t.Fatalf("should not duplicate: %v", got)
	}
}

// --- rootfsPath ---

func TestRootfsPath_Traversal(t *testing.T) {
	got := rootfsPath("/out", "/a/../b")
	if got != "/out/b" {
		t.Fatalf("got %q, want /out/b", got)
	}
}

// --- cloneImageConfig ---

func TestCloneImageConfig_DeepCopy(t *testing.T) {
	src := oci.ImageConfig{
		Entrypoint: []string{"/app"},
		Cmd:        []string{"--serve"},
		Env:        []string{"FOO=bar"},
		WorkingDir: "/app",
		User:       "nobody",
		Labels:     map[string]string{"version": "1"},
		Volumes:    []string{"/data"},
	}
	dst := cloneImageConfig(src)
	dst.Entrypoint[0] = "/other"
	dst.Labels["version"] = "2"
	if src.Entrypoint[0] != "/app" {
		t.Fatal("entrypoint clone affected original")
	}
	if src.Labels["version"] != "1" {
		t.Fatal("labels clone affected original")
	}
}

// --- cloneHealthcheck ---

func TestCloneHealthcheck_DeepCopy(t *testing.T) {
	src := &oci.Healthcheck{
		Test:     []string{"CMD", "curl", "http://localhost/"},
		Retries:  3,
		Interval: 30e9,
	}
	dst := cloneHealthcheck(src)
	dst.Test[0] = "CHANGED"
	if src.Test[0] != "CMD" {
		t.Fatal("clone affected original")
	}
}

// --- expandBuildArgValue ---

func TestExpandBuildArgValue_Simple(t *testing.T) {
	args := map[string]string{"BASE": "ubuntu"}
	got, err := expandBuildArgValue(args, "${BASE}:latest")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ubuntu:latest" {
		t.Fatalf("got %q", got)
	}
}

func TestExpandBuildArgValue_NoVars(t *testing.T) {
	args := map[string]string{}
	got, err := expandBuildArgValue(args, "literal")
	if err != nil {
		t.Fatal(err)
	}
	if got != "literal" {
		t.Fatalf("got %q", got)
	}
}

// --- defaultShell ---

func TestDefaultShell(t *testing.T) {
	sh := defaultShell()
	if len(sh) != 2 || sh[0] != "/bin/sh" || sh[1] != "-c" {
		t.Fatalf("defaultShell() = %v", sh)
	}
}

// --- Build with real minimal Dockerfiles ---

func TestBuild_ScratchWithENVAndWORKDIR(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch
ENV APP_NAME=myapp
WORKDIR /app
LABEL version="1.0"
EXPOSE 8080
VOLUME /data
CMD ["/app/run"]
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Config.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q", result.Config.WorkingDir)
	}
	found := false
	for _, env := range result.Config.Env {
		if env == "APP_NAME=myapp" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ENV APP_NAME=myapp not found in %v", result.Config.Env)
	}
	if len(result.Config.Cmd) != 1 || result.Config.Cmd[0] != "/app/run" {
		t.Errorf("Cmd = %v", result.Config.Cmd)
	}
}

func TestBuild_MultiStageWithCopyFrom(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch AS builder
ENV STAGE=build

FROM scratch
COPY --from=builder / /imported
CMD ["/start"]
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(result.Config.Cmd) != 1 || result.Config.Cmd[0] != "/start" {
		t.Errorf("Cmd = %v", result.Config.Cmd)
	}
}

func TestBuild_WithBuildArgsOverride(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch
ARG VERSION=1.0
ENV APP_VERSION=${VERSION}
CMD ["run"]
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
		BuildArgs:      map[string]string{"VERSION": "2.0"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, env := range result.Config.Env {
		if strings.Contains(env, "APP_VERSION=2.0") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected APP_VERSION=2.0 in %v", result.Config.Env)
	}
}

func TestBuild_WithEntrypointAndCmd(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["echo hello"]
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(result.Config.Entrypoint) != 2 || result.Config.Entrypoint[0] != "/bin/sh" {
		t.Errorf("Entrypoint = %v", result.Config.Entrypoint)
	}
}

func TestBuild_WithUser(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch
USER nobody
CMD ["/app"]
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Config.User != "nobody" {
		t.Errorf("User = %q", result.Config.User)
	}
}

func TestBuild_ENVOldStyleParsing(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch
ENV MYVAR hello world
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, env := range result.Config.Env {
		if env == "MYVAR=hello world" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected MYVAR=hello world in %v", result.Config.Env)
	}
}

func TestBuild_HealthcheckNone(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch
HEALTHCHECK NONE
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Config.Healthcheck == nil || result.Config.Healthcheck.Test[0] != "NONE" {
		t.Errorf("Healthcheck = %v", result.Config.Healthcheck)
	}
}

func TestBuild_ShellInstruction(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	content := `FROM scratch
SHELL ["/bin/bash", "-c"]
CMD ["echo", "hello"]
`
	if err := os.WriteFile(df, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := Build(BuildOptions{
		DockerfilePath: df,
		ContextDir:     dir,
		OutputDir:      outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(result.Config.Shell) != 2 || result.Config.Shell[0] != "/bin/bash" {
		t.Errorf("Shell = %v", result.Config.Shell)
	}
}
