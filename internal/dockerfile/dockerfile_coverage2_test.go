package dockerfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_MultiStage(t *testing.T) {
	input := `FROM golang:1.21 AS builder
RUN go build -o /app .
FROM alpine:3.18
COPY --from=builder /app /app
CMD ["/app"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Expect: FROM builder, RUN, FROM alpine, COPY, CMD
	if len(instrs) != 5 {
		t.Fatalf("got %d instructions, want 5", len(instrs))
	}
	if instrs[0].Cmd != "FROM" || instrs[0].Args[0] != "golang:1.21" {
		t.Errorf("first FROM = %v", instrs[0])
	}
	if len(instrs[0].Args) < 3 || instrs[0].Args[1] != "AS" || instrs[0].Args[2] != "builder" {
		t.Errorf("expected AS builder, got %v", instrs[0].Args)
	}
	if instrs[2].Cmd != "FROM" || instrs[2].Args[0] != "alpine:3.18" {
		t.Errorf("second FROM = %v", instrs[2])
	}
	if instrs[3].Cmd != "COPY" {
		t.Errorf("expected COPY, got %v", instrs[3])
	}
}

func TestParse_ARGExpansion(t *testing.T) {
	input := `ARG BASE=ubuntu:22.04
FROM ${BASE}
RUN echo hello
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) < 2 {
		t.Fatalf("got %d instructions", len(instrs))
	}
	if instrs[0].Cmd != "ARG" {
		t.Errorf("first = %s, want ARG", instrs[0].Cmd)
	}
}

func TestParse_COPYFromStage(t *testing.T) {
	input := `FROM golang:1.21 AS build
RUN echo "build"
FROM alpine:3.18
COPY --from=build /out /out
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var copyInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "COPY" {
			copyInstr = &instrs[i]
			break
		}
	}
	if copyInstr == nil {
		t.Fatal("no COPY instruction found")
	}
}

func TestParse_RunMounts(t *testing.T) {
	input := `FROM ubuntu:22.04
RUN --mount=type=cache,target=/var/cache/apt apt-get update
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions", len(instrs))
	}
	if instrs[1].Cmd != "RUN" {
		t.Fatalf("instr[1] = %s", instrs[1].Cmd)
	}
	if len(instrs[1].RunMounts) == 0 {
		t.Fatal("expected run mounts")
	}
	if instrs[1].RunMounts[0].Type != "cache" {
		t.Errorf("mount type = %q", instrs[1].RunMounts[0].Type)
	}
	if instrs[1].RunMounts[0].Target != "/var/cache/apt" {
		t.Errorf("mount target = %q", instrs[1].RunMounts[0].Target)
	}
}

func TestParse_ExposePorts(t *testing.T) {
	input := `FROM scratch
EXPOSE 8080 9090/tcp
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions", len(instrs))
	}
	if instrs[1].Cmd != "EXPOSE" {
		t.Fatalf("cmd = %s", instrs[1].Cmd)
	}
}

func TestParse_UserInstruction(t *testing.T) {
	input := `FROM scratch
USER nobody
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions", len(instrs))
	}
	if instrs[1].Cmd != "USER" {
		t.Fatalf("cmd = %s", instrs[1].Cmd)
	}
}

func TestParse_Healthcheck(t *testing.T) {
	input := `FROM scratch
HEALTHCHECK --interval=30s CMD curl -f http://localhost/ || exit 1
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions", len(instrs))
	}
	if instrs[1].Cmd != "HEALTHCHECK" {
		t.Fatalf("cmd = %s", instrs[1].Cmd)
	}
}

func TestParse_EntrypointShellForm(t *testing.T) {
	input := `FROM scratch
ENTRYPOINT echo hello
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions", len(instrs))
	}
	if instrs[1].Cmd != "ENTRYPOINT" {
		t.Fatalf("cmd = %s", instrs[1].Cmd)
	}
	if !instrs[1].ShellForm {
		t.Error("expected ShellForm=true for shell-style ENTRYPOINT")
	}
}

func TestParse_EntrypointExecForm(t *testing.T) {
	input := `FROM scratch
ENTRYPOINT ["/bin/sh", "-c", "echo hello"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[1].ShellForm {
		t.Error("expected ShellForm=false for exec-form ENTRYPOINT")
	}
}

func TestParseInstruction_SingleCmd(t *testing.T) {
	instr, err := parseInstruction("ENV FOO=bar")
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "ENV" {
		t.Fatalf("cmd = %s", instr.Cmd)
	}
}

func TestParseInstruction_Empty(t *testing.T) {
	_, err := parseInstruction("")
	if err == nil {
		t.Fatal("expected error for empty instruction")
	}
}

func TestSplitArgs_ExecForm(t *testing.T) {
	args := splitArgs("CMD", `["echo","hello"]`)
	if len(args) != 2 || args[0] != "echo" || args[1] != "hello" {
		t.Fatalf("splitArgs CMD exec = %v", args)
	}
}

func TestSplitArgs_ShellForm(t *testing.T) {
	args, shell := splitArgsWithForm("RUN", "apt-get update && apt-get install -y curl")
	if !shell {
		t.Error("expected shell form")
	}
	if len(args) != 1 {
		t.Fatalf("expected 1 shell arg, got %v", args)
	}
}

func TestSplitArgs_Empty(t *testing.T) {
	args, _ := splitArgsWithForm("RUN", "")
	if args != nil {
		t.Fatalf("expected nil for empty, got %v", args)
	}
}

func TestNormalizeAddFromForBuildKit_ConvertsAddFrom(t *testing.T) {
	input := "ADD --from=builder /src /dst\nCOPY /a /b\n"
	result := normalizeAddFromForBuildKit(input)
	if !strings.Contains(result, "COPY --from=builder /src /dst") {
		t.Fatalf("expected ADD --from to become COPY --from, got %q", result)
	}
	if !strings.Contains(result, "COPY /a /b") {
		t.Fatal("non-ADD lines should be preserved")
	}
}

func TestNormalizeAddFromForBuildKit_PreservesPlainAdd(t *testing.T) {
	input := "ADD /src /dst\n"
	result := normalizeAddFromForBuildKit(input)
	if !strings.Contains(result, "ADD /src /dst") {
		t.Fatalf("ADD without --from should remain: %q", result)
	}
}

func TestParse_MultipleARGs(t *testing.T) {
	input := `ARG VERSION=1.0
ARG VARIANT
FROM ubuntu:${VERSION}
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	argCount := 0
	for _, i := range instrs {
		if i.Cmd == "ARG" {
			argCount++
		}
	}
	if argCount != 2 {
		t.Fatalf("got %d ARGs, want 2", argCount)
	}
}

func TestParse_LabelInstruction(t *testing.T) {
	input := `FROM scratch
LABEL maintainer="test@example.com" version="1.0"
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[1].Cmd != "LABEL" {
		t.Fatalf("cmd = %s", instrs[1].Cmd)
	}
}

func TestParse_NonDockerfile(t *testing.T) {
	// Content that does not contain any Dockerfile instructions
	// should either return nil instructions or an error
	input := "# just a comment\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs != nil {
		t.Fatalf("expected nil for comment-only Dockerfile, got %d instructions", len(instrs))
	}
}

func TestBuild_MissingDockerfile(t *testing.T) {
	_, err := Build(BuildOptions{
		DockerfilePath: "/nonexistent/Dockerfile",
		OutputDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for missing Dockerfile")
	}
}

func TestBuild_MissingDockerfileInContext(t *testing.T) {
	dir := t.TempDir()
	// No Dockerfile exists
	_, err := Build(BuildOptions{
		ContextDir: dir,
		OutputDir:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error when no Dockerfile exists in context")
	}
}

func TestResolveDockerfilePath_ExplicitPath(t *testing.T) {
	dir := t.TempDir()
	df := filepath.Join(dir, "MyDockerfile")
	if err := os.WriteFile(df, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, err := resolveDockerfilePath(df, "")
	if err != nil {
		t.Fatalf("resolveDockerfilePath: %v", err)
	}
	if path != df {
		t.Errorf("path = %q, want %q", path, df)
	}
}

func TestResolveDockerfilePath_ContextDirAuto(t *testing.T) {
	dir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, err := resolveDockerfilePath("", dir)
	if err != nil {
		t.Fatalf("resolveDockerfilePath: %v", err)
	}
	if path != df {
		t.Errorf("path = %q, want %q", path, df)
	}
}

func TestParse_AddInstruction(t *testing.T) {
	input := `FROM scratch
ADD file.tar.gz /app
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[1].Cmd != "ADD" {
		t.Fatalf("cmd = %s, want ADD", instrs[1].Cmd)
	}
}

func TestParse_CMDExecForm(t *testing.T) {
	input := `FROM scratch
CMD ["nginx", "-g", "daemon off;"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[1].Cmd != "CMD" {
		t.Fatalf("cmd = %s", instrs[1].Cmd)
	}
	if instrs[1].ShellForm {
		t.Error("exec form CMD should not be shell form")
	}
	if len(instrs[1].Args) != 3 {
		t.Errorf("args = %v, want 3 elements", instrs[1].Args)
	}
}

func TestParse_CMDShellForm(t *testing.T) {
	input := `FROM scratch
CMD echo hello world
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !instrs[1].ShellForm {
		t.Error("shell-form CMD should have ShellForm=true")
	}
}

func TestParse_Platform(t *testing.T) {
	input := `FROM --platform=linux/amd64 ubuntu:22.04
RUN uname -m
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[0].Platform != "linux/amd64" {
		t.Errorf("platform = %q, want linux/amd64", instrs[0].Platform)
	}
}

func TestParse_SecretMount(t *testing.T) {
	input := `FROM ubuntu:22.04
RUN --mount=type=secret,id=mysecret cat /run/secrets/mysecret
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs[1].RunMounts) == 0 {
		t.Fatal("expected secret mount")
	}
	if instrs[1].RunMounts[0].Type != "secret" {
		t.Errorf("mount type = %q", instrs[1].RunMounts[0].Type)
	}
}

func TestParse_MultipleFromPlatforms(t *testing.T) {
	input := `FROM --platform=linux/arm64 ubuntu:22.04 AS base
FROM --platform=linux/amd64 ubuntu:22.04 AS build
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[0].Platform != "linux/arm64" {
		t.Errorf("first platform = %q", instrs[0].Platform)
	}
	if instrs[1].Platform != "linux/amd64" {
		t.Errorf("second platform = %q", instrs[1].Platform)
	}
}
