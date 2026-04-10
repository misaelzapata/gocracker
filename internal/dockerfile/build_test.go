package dockerfile

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gocracker/gocracker/internal/hostguard"
	"github.com/gocracker/gocracker/internal/oci"
)

// skipIfNoMountNS skips tests that need mount namespace creation (unshare).
// GitHub Actions runners and other restricted environments block this syscall.
func skipIfNoMountNS(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return // root can always unshare
	}
	// Try creating a mount namespace — if this fails, RUN tests will also fail.
	err := syscall.Unshare(syscall.CLONE_NEWNS)
	if err != nil {
		t.Skipf("mount namespace not available (unshare: %v); skipping RUN test", err)
	}
}

func TestBuild_COPYDirectoryHonorsDockerignore(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nWORKDIR /app\nCOPY . .\n")
	writeContextFile(t, ctxDir, ".dockerignore", "secret.txt\nignored/\n")
	writeContextFile(t, ctxDir, "keep.txt", "keep")
	writeContextFile(t, ctxDir, "secret.txt", "secret")
	writeContextFile(t, ctxDir, "sub/note.txt", "note")
	writeContextFile(t, ctxDir, "ignored/nope.txt", "nope")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "app", "keep.txt"), "keep")
	assertFileContent(t, filepath.Join(result.RootfsDir, "app", "sub", "note.txt"), "note")
	assertNotExists(t, filepath.Join(result.RootfsDir, "app", "secret.txt"))
	assertNotExists(t, filepath.Join(result.RootfsDir, "app", "ignored", "nope.txt"))
	assertNotExists(t, filepath.Join(result.RootfsDir, "app", filepath.Base(ctxDir)))
}

func TestBuild_COPYGlob(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY *.txt /data/\n")
	writeContextFile(t, ctxDir, "a.txt", "a")
	writeContextFile(t, ctxDir, "b.txt", "b")
	writeContextFile(t, ctxDir, "c.bin", "c")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "data", "a.txt"), "a")
	assertFileContent(t, filepath.Join(result.RootfsDir, "data", "b.txt"), "b")
	assertNotExists(t, filepath.Join(result.RootfsDir, "data", "c.bin"))
}

func TestBuild_COPYChmod(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY --chmod=0750 script.sh /app/script.sh\n")
	writeContextFile(t, ctxDir, "script.sh", "#!/bin/sh\necho ok\n")

	result := buildFromContext(t, ctxDir)

	info, err := os.Stat(filepath.Join(result.RootfsDir, "app", "script.sh"))
	if err != nil {
		t.Fatalf("Stat(script): %v", err)
	}
	if got := info.Mode().Perm(); got != 0750 {
		t.Fatalf("mode = %o, want 750", got)
	}
}

func TestBuild_COPYChown(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY --chown=1000:1000 owned.txt /app/owned.txt\n")
	writeContextFile(t, ctxDir, "owned.txt", "owned")

	result := buildFromContext(t, ctxDir)

	info, err := os.Lstat(filepath.Join(result.RootfsDir, "app", "owned.txt"))
	if err != nil {
		t.Fatalf("Lstat(owned): %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Fatalf("ownership = %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}
}

func TestBuild_COPYFromStageDirectoryChownSetsDirectoryOwnership(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch AS filesystem\nCOPY loki/ /loki/\nFROM scratch\nCOPY --from=filesystem --chown=1000:1000 /loki /loki\n")
	writeContextFile(t, ctxDir, "loki/rules/rule.yaml", "groups: []\n")

	result := buildFromContext(t, ctxDir)

	info, err := os.Lstat(filepath.Join(result.RootfsDir, "loki"))
	if err != nil {
		t.Fatalf("Lstat(/loki): %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Fatalf("/loki ownership = %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}
	assertFileContent(t, filepath.Join(result.RootfsDir, "loki", "rules", "rule.yaml"), "groups: []\n")
}

func TestBuild_ADDLocalArchiveExtracts(t *testing.T) {
	ctxDir := t.TempDir()
	archivePath := filepath.Join(ctxDir, "archive.tar.gz")
	createTarGz(t, archivePath, map[string]string{
		"nested/file.txt": "payload",
	})
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nADD archive.tar.gz /extract\n")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "extract", "nested", "file.txt"), "payload")
}

func TestBuild_ADDLocalArchiveUnpackTrue(t *testing.T) {
	ctxDir := t.TempDir()
	archivePath := filepath.Join(ctxDir, "archive.tar.gz")
	createTarGz(t, archivePath, map[string]string{
		"nested/file.txt": "payload",
	})
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nADD --unpack=true archive.tar.gz /extract\n")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "extract", "nested", "file.txt"), "payload")
}

func TestBuild_ADDRemoteURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "remote")
	}))
	defer server.Close()

	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", fmt.Sprintf("FROM scratch\nADD %s/artifact.txt /downloads/\n", server.URL))

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "downloads", "artifact.txt"), "remote")
}

func TestBuild_COPYExclude(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY --exclude=*.secret --exclude=private src/ /out/\n")
	writeContextFile(t, ctxDir, "src/public.txt", "ok")
	writeContextFile(t, ctxDir, "src/notes.secret", "nope")
	writeContextFile(t, ctxDir, "src/private/value.txt", "hidden")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "out", "public.txt"), "ok")
	assertNotExists(t, filepath.Join(result.RootfsDir, "out", "notes.secret"))
	assertNotExists(t, filepath.Join(result.RootfsDir, "out", "private", "value.txt"))
}

func TestBuild_COPYExcludeFromStage(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch AS build\nCOPY src/ /stage/\nFROM scratch\nCOPY --from=build --exclude=*.secret /stage/ /out/\n")
	writeContextFile(t, ctxDir, "src/public.txt", "ok")
	writeContextFile(t, ctxDir, "src/token.secret", "hidden")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "out", "public.txt"), "ok")
	assertNotExists(t, filepath.Join(result.RootfsDir, "out", "token.secret"))
}

func TestBuild_COPYParents(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY --parents dir/sub/file.txt /out/\n")
	writeContextFile(t, ctxDir, "dir/sub/file.txt", "payload")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "out", "dir", "sub", "file.txt"), "payload")
}

func TestBuild_COPYParentsWithDoubleStarGlob(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY --parents **/go.mod **/go.sum /workspace/\n")
	writeContextFile(t, ctxDir, "go.mod", "module root\n")
	writeContextFile(t, ctxDir, "services/api/go.mod", "module api\n")
	writeContextFile(t, ctxDir, "services/api/go.sum", "sum\n")
	writeContextFile(t, ctxDir, "pkg/lib/go.mod", "module lib\n")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "workspace", "go.mod"), "module root\n")
	assertFileContent(t, filepath.Join(result.RootfsDir, "workspace", "services", "api", "go.mod"), "module api\n")
	assertFileContent(t, filepath.Join(result.RootfsDir, "workspace", "services", "api", "go.sum"), "sum\n")
	assertFileContent(t, filepath.Join(result.RootfsDir, "workspace", "pkg", "lib", "go.mod"), "module lib\n")
}

func TestBuild_ADDLocalArchiveUnpackFalse(t *testing.T) {
	ctxDir := t.TempDir()
	archivePath := filepath.Join(ctxDir, "archive.tar.gz")
	createTarGz(t, archivePath, map[string]string{
		"nested/file.txt": "payload",
	})
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nADD --unpack=false archive.tar.gz /artifacts/archive.tar.gz\n")

	result := buildFromContext(t, ctxDir)

	assertNotExists(t, filepath.Join(result.RootfsDir, "artifacts", "nested", "file.txt"))
	if _, err := os.Stat(filepath.Join(result.RootfsDir, "artifacts", "archive.tar.gz")); err != nil {
		t.Fatalf("Stat(archive): %v", err)
	}
}

func TestBuild_ADDRemoteURLChecksum(t *testing.T) {
	payload := "remote"
	sum := sha256.Sum256([]byte(payload))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	}))
	defer server.Close()

	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", fmt.Sprintf("FROM scratch\nADD --checksum=sha256:%s %s/artifact.txt /downloads/\n", hex.EncodeToString(sum[:]), server.URL))

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "downloads", "artifact.txt"), payload)
}

func TestBuild_ADDRemoteURLChecksumMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "remote")
	}))
	defer server.Close()

	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", fmt.Sprintf("FROM scratch\nADD --checksum=sha256:%s %s/artifact.txt /downloads/\n", strings.Repeat("0", 64), server.URL))

	_, err := Build(BuildOptions{
		DockerfilePath: filepath.Join(ctxDir, "Dockerfile"),
		ContextDir:     ctxDir,
		OutputDir:      filepath.Join(t.TempDir(), "rootfs"),
	})
	if err == nil {
		t.Fatal("Build() succeeded, want checksum mismatch")
	}
	if got := err.Error(); !strings.Contains(got, "checksum mismatch") {
		t.Fatalf("error = %q, want checksum mismatch", got)
	}
}

func TestBuild_COPYLinkAccepted(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY --link hello.txt /out/hello.txt\n")
	writeContextFile(t, ctxDir, "hello.txt", "hello")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "out", "hello.txt"), "hello")
}

func TestBuild_ADDKeepGitDirAccepted(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nADD --keep-git-dir=true hello.txt /out/hello.txt\n")
	writeContextFile(t, ctxDir, "hello.txt", "hello")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "out", "hello.txt"), "hello")
}

func TestBuild_EnsureBuildEnvironmentWritesGitConfig(t *testing.T) {
	rootfs := t.TempDir()
	b := &builder{rootfs: rootfs}
	if err := b.ensureBuildEnvironment([]string{
		"HOME=/root",
		"TMPDIR=/tmp",
		"GIT_CONFIG_GLOBAL=/tmp/gocracker-gitconfig",
	}); err != nil {
		t.Fatalf("ensureBuildEnvironment(): %v", err)
	}
	assertFileContent(t, filepath.Join(rootfs, "tmp", "gocracker-gitconfig"), "[safe]\n\tdirectory = *\n")
}

func TestBuild_PreservesImageMetadata(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nLABEL app=gocracker\nEXPOSE 8080/tcp\nVOLUME /data\nSTOPSIGNAL SIGTERM\nHEALTHCHECK --interval=5s --timeout=2s --retries=4 CMD curl -f http://localhost:8080/health\n")

	result := buildFromContext(t, ctxDir)

	if result.Config.Labels["app"] != "gocracker" {
		t.Fatalf("label app = %q", result.Config.Labels["app"])
	}
	if len(result.Config.ExposedPorts) != 1 || result.Config.ExposedPorts[0] != "8080/tcp" {
		t.Fatalf("exposed ports = %#v", result.Config.ExposedPorts)
	}
	if len(result.Config.Volumes) != 1 || result.Config.Volumes[0] != "/data" {
		t.Fatalf("volumes = %#v", result.Config.Volumes)
	}
	if result.Config.StopSignal != "SIGTERM" {
		t.Fatalf("stop signal = %q", result.Config.StopSignal)
	}
	if result.Config.Healthcheck == nil || result.Config.Healthcheck.Retries != 4 {
		t.Fatalf("healthcheck = %#v", result.Config.Healthcheck)
	}
}

func TestBuild_MultiStageScratchFinalConfig(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch AS build\nCOPY artifact /app\nFROM scratch\nCOPY --from=build /app /app\nEXPOSE 8080\nENTRYPOINT [\"/app\"]\n")
	writeContextFile(t, ctxDir, "artifact", "payload")

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "app"), "payload")
	if len(result.Config.Entrypoint) != 1 || result.Config.Entrypoint[0] != "/app" {
		t.Fatalf("entrypoint = %#v, want [/app]", result.Config.Entrypoint)
	}
	if len(result.Config.ExposedPorts) != 1 || result.Config.ExposedPorts[0] != "8080" {
		t.Fatalf("exposed ports = %#v, want [8080]", result.Config.ExposedPorts)
	}
}

func TestBuild_COPYFromRemoteImage(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY --from=ghcr.io/astral-sh/uv:0.9.26 /uv /uvx /bin/\n")

	pulls := 0
	restore := overridePullOCIImage(t, func(opts oci.PullOptions) (pulledImage, error) {
		pulls++
		if opts.Ref != "ghcr.io/astral-sh/uv:0.9.26" {
			return nil, fmt.Errorf("unexpected ref %q", opts.Ref)
		}
		return fakePulledImage{
			config: oci.ImageConfig{},
			files: map[string]string{
				"uv":  "uv-binary",
				"uvx": "uvx-binary",
			},
		}, nil
	})
	defer restore()

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "bin", "uv"), "uv-binary")
	assertFileContent(t, filepath.Join(result.RootfsDir, "bin", "uvx"), "uvx-binary")
	if pulls != 1 {
		t.Fatalf("pull count = %d, want 1", pulls)
	}
}

func TestBuild_ADDFromRemoteImageUsesCachedExtraction(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nADD --from=ghcr.io/example/assets:1.2.3 /asset.txt /first.txt\nCOPY --from=ghcr.io/example/assets:1.2.3 /asset.txt /second.txt\n")

	pulls := 0
	restore := overridePullOCIImage(t, func(opts oci.PullOptions) (pulledImage, error) {
		pulls++
		if opts.Ref != "ghcr.io/example/assets:1.2.3" {
			return nil, fmt.Errorf("unexpected ref %q", opts.Ref)
		}
		return fakePulledImage{
			config: oci.ImageConfig{},
			files: map[string]string{
				"asset.txt": "payload",
			},
		}, nil
	})
	defer restore()

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "first.txt"), "payload")
	assertFileContent(t, filepath.Join(result.RootfsDir, "second.txt"), "payload")
	if pulls != 1 {
		t.Fatalf("pull count = %d, want 1", pulls)
	}
}

func TestBuild_RUNWorksRootlessWithoutExternalTools(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("exercise rootless RUN as an unprivileged user")
	}
	skipIfNoMountNS(t)

	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY tool /bin/tool\nENV FOO=bar\nRUN [\"/bin/tool\", \"/out.txt\"]\n")
	buildStaticTestTool(t, ctxDir, "tool", `package main

import (
	"fmt"
	"os"
)

func main() {
	out := "/out.txt"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	_, procErr := os.Stat("/proc")
	_, devErr := os.Stat("/dev/null")
	_, tmpErr := os.Stat("/tmp")
	data := fmt.Sprintf("uid=%d\nfoo=%s\nproc=%t\ndevnull=%t\ntmp=%t\n", os.Getuid(), os.Getenv("FOO"), procErr == nil, devErr == nil, tmpErr == nil)
	if err := os.WriteFile(out, []byte(data), 0644); err != nil {
		panic(err)
	}
}
`)

	result := buildFromContext(t, ctxDir)

	assertFileContent(t, filepath.Join(result.RootfsDir, "out.txt"), "uid=0\nfoo=bar\nproc=true\ndevnull=true\ntmp=true\n")
}

func TestBuild_ARGQuotedDefaultAndPlatformExpansion(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "ARG ARCH=\"amd64\"\nFROM --platform=linux/$ARCH scratch\n")

	result := buildFromContext(t, ctxDir)
	if result.RootfsDir == "" {
		t.Fatal("expected RootfsDir to be set")
	}
}

func TestBuild_ARGWithoutDefaultIsDefinedEmptyForFROMExpansion(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", strings.Join([]string{
		"ARG GPU",
		"FROM scratch AS qdrant-cpu",
		"FROM scratch AS qdrant-gpu-nvidia",
		"FROM qdrant-${GPU:+gpu-}${GPU:-cpu}",
		"",
	}, "\n"))

	result := buildFromContext(t, ctxDir)
	if result.RootfsDir == "" {
		t.Fatal("expected RootfsDir to be set")
	}
}

func TestBuild_ARGWithoutDefaultIsNotExportedToRUNWhenUnset(t *testing.T) {
	skipIfNoMountNS(t)
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nARG JEMALLOC_SYS_WITH_LG_PAGE\nCOPY tool /bin/tool\nRUN [\"/bin/tool\", \"/out.txt\"]\n")
	buildStaticTestTool(t, ctxDir, "tool", `package main

import (
	"fmt"
	"os"
)

func main() {
	out := "/out.txt"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	value, ok := os.LookupEnv("JEMALLOC_SYS_WITH_LG_PAGE")
	data := fmt.Sprintf("set=%t\nvalue=%q\n", ok, value)
	if err := os.WriteFile(out, []byte(data), 0644); err != nil {
		panic(err)
	}
}
`)

	result := buildFromContext(t, ctxDir)
	assertFileContent(t, filepath.Join(result.RootfsDir, "out.txt"), "set=false\nvalue=\"\"\n")
}

func TestBuild_RUNSeesARGValues(t *testing.T) {
	skipIfNoMountNS(t)
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nARG S6_ARCH=x86_64\nCOPY tool /bin/tool\nRUN [\"/bin/tool\", \"/out.txt\"]\n")
	buildStaticTestTool(t, ctxDir, "tool", `package main

import (
	"os"
)

func main() {
	out := "/out.txt"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.WriteFile(out, []byte(os.Getenv("S6_ARCH")), 0644); err != nil {
		panic(err)
	}
}
`)

	result := buildFromContext(t, ctxDir)
	assertFileContent(t, filepath.Join(result.RootfsDir, "out.txt"), "x86_64")
}

func TestBuild_RUNHasDevPts(t *testing.T) {
	skipIfNoMountNS(t)
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY tool /bin/tool\nRUN [\"/bin/tool\", \"/out.txt\"]\n")
	buildStaticTestTool(t, ctxDir, "tool", `package main

import (
	"fmt"
	"os"
)

func main() {
	out := "/out.txt"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	_, ptsErr := os.Stat("/dev/pts")
	fd, ptmxErr := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if ptmxErr == nil {
		fd.Close()
	}
	data := fmt.Sprintf("pts=%t\nptmx=%t\n", ptsErr == nil, ptmxErr == nil)
	if err := os.WriteFile(out, []byte(data), 0644); err != nil {
		panic(err)
	}
}
`)

	result := buildFromContext(t, ctxDir)
	assertFileContent(t, filepath.Join(result.RootfsDir, "out.txt"), "pts=true\nptmx=true\n")
}

func TestBuild_FromUnresolvedVariableFailsExplicitly(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM ${IMAGE}\n")

	_, err := Build(BuildOptions{
		DockerfilePath: filepath.Join(ctxDir, "Dockerfile"),
		ContextDir:     ctxDir,
		OutputDir:      filepath.Join(t.TempDir(), "rootfs"),
	})
	if err == nil {
		t.Fatal("Build() succeeded, want error")
	}
	if got := err.Error(); !strings.Contains(got, "unresolved variables") {
		t.Fatalf("error = %q, want unresolved variables", got)
	}
}

func TestBuild_DockerfilePathAndContextDirCanDiffer(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "docker/Dockerfile", "FROM scratch\nCOPY app.txt /app.txt\n")
	writeContextFile(t, ctxDir, "app.txt", "payload")

	result, err := Build(BuildOptions{
		DockerfilePath: filepath.Join("docker", "Dockerfile"),
		ContextDir:     ctxDir,
		OutputDir:      filepath.Join(t.TempDir(), "rootfs"),
	})
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	assertFileContent(t, filepath.Join(result.RootfsDir, "app.txt"), "payload")
}

func TestBuild_FromPlatformBuildPlatformAndTargetArch(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM --platform=$BUILDPLATFORM scratch AS build\nFROM --platform=linux/$TARGETARCH scratch\n")

	result, err := Build(BuildOptions{
		DockerfilePath: filepath.Join(ctxDir, "Dockerfile"),
		ContextDir:     ctxDir,
		OutputDir:      filepath.Join(t.TempDir(), "rootfs"),
	})
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if result.RootfsDir == "" {
		t.Fatal("expected RootfsDir to be set")
	}
}

func TestBuild_SkipsUnusedForeignPlatformStage(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", strings.Join([]string{
		"FROM --platform=linux/arm64 scratch AS arm64",
		"FROM --platform=linux/amd64 scratch AS amd64",
		"COPY artifact /artifact",
		"FROM amd64",
		"COPY --from=amd64 /artifact /artifact",
	}, "\n"))
	writeContextFile(t, ctxDir, "artifact", "payload")

	result := buildFromContext(t, ctxDir)
	assertFileContent(t, filepath.Join(result.RootfsDir, "artifact"), "payload")
}

func TestBuild_SkipsUnusedStagePreservesNumericFromIndexes(t *testing.T) {
	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", strings.Join([]string{
		"FROM scratch AS zero",
		"COPY zero.txt /zero.txt",
		"FROM scratch AS skipme",
		"COPY skip.txt /skip.txt",
		"FROM scratch AS two",
		"COPY --from=0 /zero.txt /copied-zero.txt",
		"FROM scratch",
		"COPY --from=2 /copied-zero.txt /final.txt",
	}, "\n"))
	writeContextFile(t, ctxDir, "zero.txt", "zero")
	writeContextFile(t, ctxDir, "skip.txt", "skip")

	result := buildFromContext(t, ctxDir)
	assertFileContent(t, filepath.Join(result.RootfsDir, "final.txt"), "zero")
}

func TestBuild_RUNAsRootCannotDamageHostDev(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{}); err != nil {
		t.Skipf("host /dev is already unhealthy: %v", err)
	}

	ctxDir := t.TempDir()
	writeContextFile(t, ctxDir, "Dockerfile", "FROM scratch\nCOPY tool /bin/tool\nRUN [\"/bin/tool\", \"/out.txt\"]\n")
	buildStaticTestTool(t, ctxDir, "tool", `package main

import "os"

func main() {
	for _, path := range []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/ptmx"} {
		_ = os.Remove(path)
	}
	if err := os.WriteFile("/out.txt", []byte("isolated\n"), 0644); err != nil {
		panic(err)
	}
}
`)

	result := buildFromContext(t, ctxDir)
	assertFileContent(t, filepath.Join(result.RootfsDir, "out.txt"), "isolated\n")
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{}); err != nil {
		t.Fatalf("host /dev was damaged by privileged RUN: %v", err)
	}
}

func TestPrivilegedRunMountsStayInPrivateNamespace(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{}); err != nil {
		t.Skipf("host /dev is already unhealthy: %v", err)
	}

	rootfs := t.TempDir()
	b := &builder{
		opts:         BuildOptions{ContextDir: t.TempDir()},
		rootfs:       rootfs,
		env:          map[string]string{},
		args:         defaultBuildArgs(),
		shell:        defaultShell(),
		pullCache:    map[string]pulledImage{},
		remoteRootfs: map[string]string{},
		stageByName:  map[string]*buildStage{},
		runCacheRoot: filepath.Join(t.TempDir(), "cache"),
	}
	b.ensureDirs()
	buildStaticTestTool(t, rootfs, "bin/tool", `package main

import "os"

func main() {
	if err := os.WriteFile("/out.txt", []byte("ok\n"), 0644); err != nil {
		panic(err)
	}
}
`)

	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TMPDIR=/tmp",
	}
	if err := b.ensureBuildEnvironment(env); err != nil {
		t.Fatalf("ensureBuildEnvironment(): %v", err)
	}
	if err := b.runPrivileged([]string{"/bin/tool"}, env, nil); err != nil {
		t.Fatalf("runPrivileged(): %v", err)
	}
	assertFileContent(t, filepath.Join(rootfs, "out.txt"), "ok\n")

	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("ReadFile(/proc/self/mountinfo): %v", err)
	}
	if strings.Contains(string(mountInfo), rootfs) {
		t.Fatalf("privileged RUN leaked mount entries into the host namespace for %s", rootfs)
	}
}

func TestPrivilegedRunProvidesWritableDevNullAfterDroppingPrivileges(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	rootfs := t.TempDir()
	b := &builder{
		opts:         BuildOptions{ContextDir: t.TempDir()},
		rootfs:       rootfs,
		env:          map[string]string{},
		args:         defaultBuildArgs(),
		shell:        defaultShell(),
		pullCache:    map[string]pulledImage{},
		remoteRootfs: map[string]string{},
		stageByName:  map[string]*buildStage{},
		runCacheRoot: filepath.Join(t.TempDir(), "cache"),
	}
	b.ensureDirs()
	buildStaticTestTool(t, rootfs, "bin/tool", `package main

import (
	"os"
	"syscall"
)

func main() {
	if err := syscall.Setgid(65534); err != nil {
		panic(err)
	}
	if err := syscall.Setuid(65534); err != nil {
		panic(err)
	}
	f, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		panic(err)
	}
	if _, err := f.Write([]byte("ok")); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
	if err := os.WriteFile("/tmp/dev-null-ok", []byte("ok\n"), 0644); err != nil {
		panic(err)
	}
}
`)

	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TMPDIR=/tmp",
	}
	if err := b.ensureBuildEnvironment(env); err != nil {
		t.Fatalf("ensureBuildEnvironment(): %v", err)
	}
	if err := b.runPrivileged([]string{"/bin/tool"}, env, nil); err != nil {
		t.Fatalf("runPrivileged(): %v", err)
	}
	assertFileContent(t, filepath.Join(rootfs, "tmp", "dev-null-ok"), "ok\n")
}

func buildFromContext(t *testing.T, ctxDir string) *BuildResult {
	t.Helper()

	outputDir := filepath.Join(t.TempDir(), "rootfs")
	result, err := Build(BuildOptions{
		DockerfilePath: filepath.Join(ctxDir, "Dockerfile"),
		ContextDir:     ctxDir,
		OutputDir:      outputDir,
	})
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	return result
}

type fakePulledImage struct {
	config oci.ImageConfig
	files  map[string]string
}

func (f fakePulledImage) ImageConfig() oci.ImageConfig {
	return f.config
}

func (f fakePulledImage) ExtractToDir(dir string) error {
	for rel, contents := range f.files {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(contents), 0644); err != nil {
			return err
		}
	}
	return nil
}

func overridePullOCIImage(t *testing.T, fn func(opts oci.PullOptions) (pulledImage, error)) func() {
	t.Helper()
	prev := pullOCIImage
	pullOCIImage = fn
	return func() {
		pullOCIImage = prev
	}
}

func writeContextFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
}

func buildStaticTestTool(t *testing.T, root, rel, source string) {
	t.Helper()
	tmp := t.TempDir()
	mainPath := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(mainPath, []byte(source), 0644); err != nil {
		t.Fatalf("WriteFile(helper): %v", err)
	}
	outPath := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		t.Fatalf("MkdirAll(helper out): %v", err)
	}

	cmd := exec.Command("go", "build", "-o", outPath, mainPath)
	cmd.Env = append(os.Environ(),
		"GO111MODULE=off",
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=amd64",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build helper tool: %v\n%s", err, output)
	}
}

func createTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create(%s): %v", path, err)
	}
	defer file.Close()

	gzw := gzip.NewWriter(file)
	defer gzw.Close()
	tw := tar.NewWriter(gzw)
	defer tw.Close()

	for name, contents := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(contents)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(contents)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if got := string(data); got != want {
		t.Fatalf("content of %s = %q, want %q", path, got, want)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist", path)
	}
}
