package dockerfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/oci"
)

func TestParse_BasicInstructions(t *testing.T) {
	input := `FROM ubuntu:22.04
RUN apt-get update
COPY . /app
WORKDIR /app
CMD ["./myapp"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 5 {
		t.Fatalf("got %d instructions, want 5", len(instrs))
	}

	tests := []struct {
		cmd  string
		args []string
	}{
		{"FROM", []string{"ubuntu:22.04"}},
		{"RUN", []string{"apt-get update"}},
		{"COPY", []string{". /app"}},
		{"WORKDIR", []string{"/app"}},
		{"CMD", []string{"./myapp"}},
	}
	for i, tt := range tests {
		if instrs[i].Cmd != tt.cmd {
			t.Errorf("instr[%d].Cmd = %q, want %q", i, instrs[i].Cmd, tt.cmd)
		}
		gotArgs := strings.Join(instrs[i].Args, " ")
		wantArgs := strings.Join(tt.args, " ")
		if gotArgs != wantArgs {
			t.Errorf("instr[%d].Args = %q, want %q", i, gotArgs, wantArgs)
		}
	}
}

func TestParse_Comments(t *testing.T) {
	input := `# This is a comment
FROM scratch
# Another comment
RUN echo hello
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions, want 2 (comments should be skipped)", len(instrs))
	}
	if instrs[0].Cmd != "FROM" {
		t.Errorf("first instruction = %q, want FROM", instrs[0].Cmd)
	}
	if instrs[1].Cmd != "RUN" {
		t.Errorf("second instruction = %q, want RUN", instrs[1].Cmd)
	}
}

func TestParse_EmptyLines(t *testing.T) {
	input := `FROM scratch

RUN echo hello

`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions, want 2", len(instrs))
	}
}

func TestParse_LineContinuation(t *testing.T) {
	input := `FROM scratch
RUN apt-get update && \
    apt-get install -y \
    curl wget
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions, want 2", len(instrs))
	}
	// The continuation should be joined into a single RUN instruction
	if instrs[1].Cmd != "RUN" {
		t.Errorf("instruction = %q, want RUN", instrs[1].Cmd)
	}
	joinedArgs := strings.Join(instrs[1].Args, " ")
	if !strings.Contains(joinedArgs, "curl wget") {
		t.Errorf("continuation not joined properly: %q", joinedArgs)
	}
}

func TestParse_JSONForm(t *testing.T) {
	input := `FROM scratch
CMD ["echo","hello","world"]
ENTRYPOINT ["/bin/sh","-c"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 3 {
		t.Fatalf("got %d instructions, want 3", len(instrs))
	}

	// CMD in JSON form should have 3 args
	cmd := instrs[1]
	if cmd.Cmd != "CMD" {
		t.Errorf("cmd = %q, want CMD", cmd.Cmd)
	}
	if len(cmd.Args) != 3 {
		t.Fatalf("CMD args = %v (len %d), want 3 args", cmd.Args, len(cmd.Args))
	}
	if cmd.Args[0] != "echo" || cmd.Args[1] != "hello" || cmd.Args[2] != "world" {
		t.Errorf("CMD args = %v, want [echo hello world]", cmd.Args)
	}

	// ENTRYPOINT in JSON form
	ep := instrs[2]
	if len(ep.Args) != 2 {
		t.Fatalf("ENTRYPOINT args = %v (len %d), want 2", ep.Args, len(ep.Args))
	}
	if ep.Args[0] != "/bin/sh" || ep.Args[1] != "-c" {
		t.Errorf("ENTRYPOINT args = %v, want [/bin/sh -c]", ep.Args)
	}
}

func TestParse_ShellForm(t *testing.T) {
	input := `FROM scratch
CMD echo hello world
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cmd := instrs[1]
	// Shell form returns the whole thing as a single arg
	if len(cmd.Args) != 1 {
		t.Fatalf("shell form CMD args = %v (len %d), want 1", cmd.Args, len(cmd.Args))
	}
	if cmd.Args[0] != "echo hello world" {
		t.Errorf("CMD args[0] = %q, want %q", cmd.Args[0], "echo hello world")
	}
	if !cmd.ShellForm {
		t.Fatal("CMD shell form should be marked as ShellForm")
	}
}

func TestParse_FromPlatformPreserved(t *testing.T) {
	instr, err := parseInstruction(`FROM --platform=linux/amd64 scratch AS base`)
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "FROM" {
		t.Fatalf("cmd = %q, want FROM", instr.Cmd)
	}
	if instr.Platform != "linux/amd64" {
		t.Fatalf("platform = %q, want linux/amd64", instr.Platform)
	}
	if len(instr.Args) != 3 || instr.Args[0] != "scratch" || instr.Args[1] != "AS" || instr.Args[2] != "base" {
		t.Fatalf("args = %#v, want [scratch AS base]", instr.Args)
	}
}

func TestParse_RunMountsPreserved(t *testing.T) {
	instr, err := parseInstruction(`RUN --mount=type=cache,target=/root/.cache/uv --mount=type=bind,source=uv.lock,target=uv.lock uv sync --frozen`)
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "RUN" {
		t.Fatalf("cmd = %q, want RUN", instr.Cmd)
	}
	if len(instr.RunMounts) != 2 {
		t.Fatalf("run mounts = %#v, want 2 mounts", instr.RunMounts)
	}
	if instr.RunMounts[0].Type != "cache" || instr.RunMounts[0].Target != "/root/.cache/uv" {
		t.Fatalf("first mount = %#v, want cache target /root/.cache/uv", instr.RunMounts[0])
	}
	if instr.RunMounts[1].Type != "bind" || instr.RunMounts[1].Source != "uv.lock" || instr.RunMounts[1].Target != "uv.lock" {
		t.Fatalf("second mount = %#v, want bind source uv.lock target uv.lock", instr.RunMounts[1])
	}
}

func TestParse_RunHeredocSimple(t *testing.T) {
	// BuildKit only detects heredoc when `<<EOF` is the very start of the
	// RUN command; anything else is taken as a regular shell command. Both
	// forms must parse successfully now that we support concatenated
	// heredoc bodies in translateRunCommand.
	input := "FROM scratch\nRUN <<EOF\necho hi\nEOF\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var runInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "RUN" {
			runInstr = &instrs[i]
			break
		}
	}
	if runInstr == nil {
		t.Fatalf("no RUN instruction: %#v", instrs)
	}
	if !runInstr.ShellForm {
		t.Fatalf("RUN instruction should be shell form: %#v", runInstr)
	}
	if !strings.Contains(runInstr.Args[0], "echo hi") {
		t.Fatalf("RUN script = %q, want 'echo hi'", runInstr.Args[0])
	}
}

func TestParse_ENVInstruction(t *testing.T) {
	input := `FROM scratch
ENV MY_VAR=hello OTHER=world
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions, want 2", len(instrs))
	}
	env := instrs[1]
	if env.Cmd != "ENV" {
		t.Errorf("cmd = %q, want ENV", env.Cmd)
	}
}

func TestParse_ARGInstruction(t *testing.T) {
	input := `ARG VERSION=1.0
FROM scratch
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 2 {
		t.Fatalf("got %d instructions, want 2", len(instrs))
	}
	if instrs[0].Cmd != "ARG" {
		t.Errorf("cmd = %q, want ARG", instrs[0].Cmd)
	}
}

func TestParse_CaseInsensitiveCommands(t *testing.T) {
	input := `from scratch
run echo hello
cmd ["test"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Commands should be uppercased
	if instrs[0].Cmd != "FROM" {
		t.Errorf("cmd = %q, want FROM", instrs[0].Cmd)
	}
	if instrs[1].Cmd != "RUN" {
		t.Errorf("cmd = %q, want RUN", instrs[1].Cmd)
	}
	if instrs[2].Cmd != "CMD" {
		t.Errorf("cmd = %q, want CMD", instrs[2].Cmd)
	}
}

func TestParse_AllSupportedInstructions(t *testing.T) {
	input := `FROM scratch
MAINTAINER test
LABEL version="1.0"
RUN echo hi
COPY . /app
ADD file.tar /app
ENV KEY=val
ARG NAME=default
WORKDIR /app
USER root
EXPOSE 8080
VOLUME /data
HEALTHCHECK CMD curl -f http://localhost/
SHELL ["/bin/bash","-c"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/start"]
CMD ["run"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 17 {
		t.Fatalf("got %d instructions, want 17", len(instrs))
	}
}

func TestSplitArgs_JSONArray(t *testing.T) {
	args := splitArgs("CMD", `["echo","hello","world"]`)
	if len(args) != 3 {
		t.Fatalf("got %d args, want 3", len(args))
	}
	if args[0] != "echo" {
		t.Errorf("args[0] = %q, want echo", args[0])
	}
}

func TestSplitArgs_ShellString(t *testing.T) {
	args := splitArgs("RUN", "apt-get update && apt-get install -y curl")
	if len(args) != 1 {
		t.Fatalf("shell form should produce 1 arg, got %d", len(args))
	}
}

func TestCommandConfigArgs_UsesShellForShellForm(t *testing.T) {
	b := &builder{shell: []string{"/bin/bash", "-c"}}
	got := b.commandConfigArgs(Instruction{
		Cmd:       "CMD",
		Args:      []string{"echo hi"},
		ShellForm: true,
	})
	want := []string{"/bin/bash", "-c", "echo hi"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("commandConfigArgs() = %#v, want %#v", got, want)
	}
}

func TestCopyPathPreservesOwnership(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root")
	}

	dir := t.TempDir()
	src := dir + "/src.txt"
	dst := dir + "/dst.txt"
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("WriteFile(src): %v", err)
	}
	if err := os.Chown(src, 1000, 1000); err != nil {
		t.Fatalf("Chown(src): %v", err)
	}

	if err := copyPath(src, dst, true); err != nil {
		t.Fatalf("copyPath(): %v", err)
	}
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("Lstat(dst): %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Fatalf("dst ownership = %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}
}

func TestExpand(t *testing.T) {
	b := &builder{
		env:  map[string]string{"HOME": "/root"},
		args: map[string]string{"VERSION": "1.0"},
	}
	tests := []struct {
		input string
		want  string
	}{
		{"$HOME/bin", "/root/bin"},
		{"${HOME}/bin", "/root/bin"},
		{"v$VERSION", "v1.0"},
		{"${VERSION}-release", "1.0-release"},
		{`"amd64"`, "amd64"},
		{"novar", "novar"},
	}
	for _, tt := range tests {
		got := b.expand(tt.input)
		if got != tt.want {
			t.Errorf("expand(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildStandardDevLinks(t *testing.T) {
	rootfs := t.TempDir()
	b := &builder{rootfs: rootfs}
	if err := b.ensureStandardDevLinks(); err != nil {
		t.Fatalf("ensureStandardDevLinks: %v", err)
	}

	for _, tc := range []struct {
		path   string
		target string
	}{
		{path: "/dev/fd", target: "/proc/self/fd"},
		{path: "/dev/stdin", target: "/proc/self/fd/0"},
		{path: "/dev/stdout", target: "/proc/self/fd/1"},
		{path: "/dev/stderr", target: "/proc/self/fd/2"},
	} {
		info, err := os.Lstat(filepath.Join(rootfs, filepath.FromSlash(tc.path)))
		if err != nil {
			t.Fatalf("Lstat(%s): %v", tc.path, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s = %v, want symlink", tc.path, info.Mode())
		}
		gotTarget, err := os.Readlink(filepath.Join(rootfs, filepath.FromSlash(tc.path)))
		if err != nil {
			t.Fatalf("Readlink(%s): %v", tc.path, err)
		}
		if gotTarget != tc.target {
			t.Fatalf("%s target = %q, want %q", tc.path, gotTarget, tc.target)
		}
	}
}

func TestBuildRunCommand_RespectsShellAndWorkdir(t *testing.T) {
	b := &builder{
		shell:   []string{"/bin/bash", "-c"},
		workdir: "/app",
	}
	got := b.buildRunCommand([]string{"echo hello"})
	want := []string{"/bin/bash", "-c", "cd '/app' && echo hello"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("buildRunCommand() = %#v, want %#v", got, want)
	}
}

func TestResolveContextMountSource_RejectsEscapes(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveContextMountSource(root, "../secret"); err == nil {
		t.Fatal("resolveContextMountSource should reject escaping source")
	}
}

func TestSanitizeRunMountCacheKey(t *testing.T) {
	got := sanitizeRunMountCacheKey("/root/.cache/uv")
	if got != "root_.cache_uv" {
		t.Fatalf("sanitizeRunMountCacheKey() = %q, want %q", got, "root_.cache_uv")
	}
}

func TestParseInstruction_NoArgs(t *testing.T) {
	instr, err := parseInstruction("EXPOSE")
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "EXPOSE" {
		t.Errorf("cmd = %q, want EXPOSE", instr.Cmd)
	}
}

func TestParse_EmptyInput(t *testing.T) {
	instrs, err := parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 0 {
		t.Errorf("got %d instructions from empty input, want 0", len(instrs))
	}
}

func TestParse_OnlyComments(t *testing.T) {
	input := `# comment 1
# comment 2
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 0 {
		t.Errorf("got %d instructions from comments-only, want 0", len(instrs))
	}
}

func TestParse_MultiStageBuilds(t *testing.T) {
	input := `FROM golang:1.21 AS builder
WORKDIR /src
COPY . .
RUN go build -o /app

FROM scratch
COPY --from=builder /app /app
ENTRYPOINT ["/app"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Count FROM instructions
	fromCount := 0
	for _, instr := range instrs {
		if instr.Cmd == "FROM" {
			fromCount++
		}
	}
	if fromCount != 2 {
		t.Fatalf("got %d FROM instructions, want 2", fromCount)
	}

	// First FROM should have AS alias
	if instrs[0].Cmd != "FROM" {
		t.Fatalf("first instruction = %q, want FROM", instrs[0].Cmd)
	}
	args := strings.Join(instrs[0].Args, " ")
	if !strings.Contains(args, "AS") || !strings.Contains(args, "builder") {
		t.Fatalf("first FROM args = %q, want AS builder", args)
	}

	// Find the COPY --from instruction
	var copyInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "COPY" && len(instrs[i].Args) > 0 && strings.HasPrefix(instrs[i].Args[0], "--from=") {
			copyInstr = &instrs[i]
			break
		}
	}
	if copyInstr == nil {
		t.Fatal("no COPY --from instruction found")
	}
	if copyInstr.Args[0] != "--from=builder" {
		t.Fatalf("COPY --from arg = %q, want --from=builder", copyInstr.Args[0])
	}
}

func TestParse_COPYFromHandling(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantFrom string
	}{
		{
			name:     "COPY from named stage",
			input:    "FROM scratch AS build\nCOPY app /app\nFROM scratch\nCOPY --from=build /app /app\n",
			wantFrom: "--from=build",
		},
		{
			name:     "COPY from numeric index",
			input:    "FROM scratch\nCOPY app /app\nFROM scratch\nCOPY --from=0 /app /app\n",
			wantFrom: "--from=0",
		},
		{
			name:     "COPY from remote image",
			input:    "FROM scratch\nCOPY --from=nginx:latest /etc/nginx/nginx.conf /etc/nginx/nginx.conf\n",
			wantFrom: "--from=nginx:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instrs, err := parse(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var found bool
			for _, instr := range instrs {
				if instr.Cmd == "COPY" {
					for _, arg := range instr.Args {
						if arg == tt.wantFrom {
							found = true
						}
					}
				}
			}
			if !found {
				t.Fatalf("did not find COPY with %s in instructions", tt.wantFrom)
			}
		})
	}
}

func TestParse_RUNMountTypes(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		mountType string
		target    string
	}{
		{
			name:      "cache mount",
			input:     "FROM scratch\nRUN --mount=type=cache,target=/var/cache/apt apt-get update\n",
			mountType: "cache",
			target:    "/var/cache/apt",
		},
		{
			name:      "bind mount",
			input:     "FROM scratch\nRUN --mount=type=bind,source=go.sum,target=go.sum echo ok\n",
			mountType: "bind",
			target:    "go.sum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instrs, err := parse(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var runInstr *Instruction
			for i := range instrs {
				if instrs[i].Cmd == "RUN" {
					runInstr = &instrs[i]
					break
				}
			}
			if runInstr == nil {
				t.Fatal("no RUN instruction found")
			}
			if len(runInstr.RunMounts) == 0 {
				t.Fatal("expected at least one run mount")
			}
			if runInstr.RunMounts[0].Type != tt.mountType {
				t.Fatalf("mount type = %q, want %q", runInstr.RunMounts[0].Type, tt.mountType)
			}
			if runInstr.RunMounts[0].Target != tt.target {
				t.Fatalf("mount target = %q, want %q", runInstr.RunMounts[0].Target, tt.target)
			}
		})
	}
}

func TestParse_ARGENVSubstitution(t *testing.T) {
	input := `ARG BASE=ubuntu
ARG TAG=22.04
FROM scratch
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	argCount := 0
	for _, instr := range instrs {
		if instr.Cmd == "ARG" {
			argCount++
		}
	}
	if argCount != 2 {
		t.Fatalf("got %d ARG instructions, want 2", argCount)
	}
	// Verify the first ARG has default value
	firstArg := instrs[0]
	if firstArg.Cmd != "ARG" {
		t.Fatalf("first instruction = %q, want ARG", firstArg.Cmd)
	}
	if len(firstArg.Args) != 1 || firstArg.Args[0] != "BASE=ubuntu" {
		t.Fatalf("ARG args = %v, want [BASE=ubuntu]", firstArg.Args)
	}
}

func TestParse_USERDirective(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple user",
			input: "FROM scratch\nUSER nobody\n",
			want:  "nobody",
		},
		{
			name:  "user:group",
			input: "FROM scratch\nUSER appuser:appgroup\n",
			want:  "appuser:appgroup",
		},
		{
			name:  "numeric uid",
			input: "FROM scratch\nUSER 1000\n",
			want:  "1000",
		},
		{
			name:  "numeric uid:gid",
			input: "FROM scratch\nUSER 1000:1000\n",
			want:  "1000:1000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instrs, err := parse(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var userInstr *Instruction
			for i := range instrs {
				if instrs[i].Cmd == "USER" {
					userInstr = &instrs[i]
					break
				}
			}
			if userInstr == nil {
				t.Fatal("no USER instruction found")
			}
			if len(userInstr.Args) != 1 || userInstr.Args[0] != tt.want {
				t.Fatalf("USER args = %v, want [%s]", userInstr.Args, tt.want)
			}
		})
	}
}

func TestParse_WORKDIRHandling(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "absolute path",
			input: "FROM scratch\nWORKDIR /app\n",
			want:  "/app",
		},
		{
			name:  "nested path",
			input: "FROM scratch\nWORKDIR /usr/local/share\n",
			want:  "/usr/local/share",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instrs, err := parse(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var workdirInstr *Instruction
			for i := range instrs {
				if instrs[i].Cmd == "WORKDIR" {
					workdirInstr = &instrs[i]
					break
				}
			}
			if workdirInstr == nil {
				t.Fatal("no WORKDIR instruction found")
			}
			if len(workdirInstr.Args) != 1 || workdirInstr.Args[0] != tt.want {
				t.Fatalf("WORKDIR args = %v, want [%s]", workdirInstr.Args, tt.want)
			}
		})
	}
}

func TestParse_HeredocMultipleBlocks(t *testing.T) {
	input := "FROM scratch\nRUN <<EOF\necho hello\necho world\nEOF\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var runInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "RUN" {
			runInstr = &instrs[i]
			break
		}
	}
	if runInstr == nil {
		t.Fatal("no RUN instruction found")
	}
	script := runInstr.Args[0]
	if !strings.Contains(script, "echo hello") || !strings.Contains(script, "echo world") {
		t.Fatalf("heredoc body = %q, want both echo lines", script)
	}
}

func TestExpand_VariousCases(t *testing.T) {
	b := &builder{
		env: map[string]string{
			"APP_NAME": "myapp",
			"PORT":     "8080",
		},
		args: map[string]string{
			"VERSION": "2.0",
			"BUILD":   "release",
		},
	}
	tests := []struct {
		input string
		want  string
	}{
		{"$APP_NAME", "myapp"},
		{"${APP_NAME}", "myapp"},
		{"${APP_NAME}:${PORT}", "myapp:8080"},
		{"v$VERSION-$BUILD", "v2.0-release"},
		{"literal", "literal"},
		{"", ""},
	}
	for _, tt := range tests {
		got := b.expand(tt.input)
		if got != tt.want {
			t.Errorf("expand(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSanitizeRunMountCacheKey_VariousCases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/root/.cache/uv", "root_.cache_uv"},
		{"/var/cache/apt", "var_cache_apt"},
		{".", "root"},
		{"", "root"},
		{"/", "root"},
		{" /tmp/build ", "tmp_build"},
	}
	for _, tt := range tests {
		got := sanitizeRunMountCacheKey(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeRunMountCacheKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveContextMountSource_Cases(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		root    string
		src     string
		wantErr bool
	}{
		{"empty source returns root", root, "", false},
		{"relative path inside", root, "subdir", false},
		{"escape via ..", root, "../secret", true},
		{"double escape", root, "../../etc/passwd", true},
		{"empty root", "", "anything", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveContextMountSource(tt.root, tt.src)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveContextMountSource(%q, %q) error = %v, wantErr %v", tt.root, tt.src, err, tt.wantErr)
			}
		})
	}
}

func TestBuildRunCommand_VariousCases(t *testing.T) {
	tests := []struct {
		name    string
		shell   []string
		workdir string
		args    []string
		want    []string
	}{
		{
			name:    "no workdir",
			shell:   []string{"/bin/sh", "-c"},
			workdir: "",
			args:    []string{"echo hello"},
			want:    []string{"/bin/sh", "-c", "echo hello"},
		},
		{
			name:    "with workdir",
			shell:   []string{"/bin/sh", "-c"},
			workdir: "/app",
			args:    []string{"make build"},
			want:    []string{"/bin/sh", "-c", "cd '/app' && make build"},
		},
		{
			name:    "custom shell",
			shell:   []string{"/bin/bash", "-eo", "pipefail", "-c"},
			workdir: "",
			args:    []string{"pip install ."},
			want:    []string{"/bin/bash", "-eo", "pipefail", "-c", "pip install ."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &builder{
				shell:   tt.shell,
				workdir: tt.workdir,
			}
			got := b.buildRunCommand(tt.args)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("buildRunCommand() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCommandConfigArgs_ExecForm(t *testing.T) {
	b := &builder{shell: []string{"/bin/sh", "-c"}}
	got := b.commandConfigArgs(Instruction{
		Cmd:       "CMD",
		Args:      []string{"/app", "--port", "8080"},
		ShellForm: false,
	})
	want := []string{"/app", "--port", "8080"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("commandConfigArgs(exec form) = %#v, want %#v", got, want)
	}
}

func TestSplitArgs_VariousCases(t *testing.T) {
	tests := []struct {
		cmd  string
		raw  string
		want int
	}{
		{"CMD", `["echo","hello"]`, 2},
		{"CMD", `["single"]`, 1},
		{"RUN", "apt-get update && install", 1},
		{"EXPOSE", "8080 9090", 2},
	}
	for _, tt := range tests {
		args := splitArgs(tt.cmd, tt.raw)
		if len(args) != tt.want {
			t.Errorf("splitArgs(%q, %q) len = %d, want %d (args=%v)", tt.cmd, tt.raw, len(args), tt.want, args)
		}
	}
}

func TestParse_ENVMultipleForms(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "key=value form",
			input: "FROM scratch\nENV MY_VAR=hello OTHER=world\n",
		},
		{
			name:  "single pair",
			input: "FROM scratch\nENV SINGLE=value\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instrs, err := parse(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var envInstr *Instruction
			for i := range instrs {
				if instrs[i].Cmd == "ENV" {
					envInstr = &instrs[i]
					break
				}
			}
			if envInstr == nil {
				t.Fatal("no ENV instruction found")
			}
		})
	}
}

func TestParse_ARGWithoutDefault(t *testing.T) {
	input := "ARG MYVAR\nFROM scratch\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[0].Cmd != "ARG" {
		t.Fatalf("first instruction = %q, want ARG", instrs[0].Cmd)
	}
	if len(instrs[0].Args) != 1 || instrs[0].Args[0] != "MYVAR" {
		t.Fatalf("ARG args = %v, want [MYVAR]", instrs[0].Args)
	}
}

func TestHasDockerfileInstructions(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"only whitespace", "   \n  \n  ", false},
		{"only comments", "# comment\n# another\n", false},
		{"has FROM", "FROM scratch\n", true},
		{"comment then FROM", "# comment\nFROM scratch\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasDockerfileInstructions(tt.content)
			if got != tt.want {
				t.Fatalf("hasDockerfileInstructions(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestNormalizeAddFromForBuildKit(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "ADD --from becomes COPY --from",
			input: "ADD --from=builder /app /app",
			want:  "COPY --from=builder /app /app",
		},
		{
			name:  "regular ADD unchanged",
			input: "ADD file.tar.gz /app",
			want:  "ADD file.tar.gz /app",
		},
		{
			name:  "comment line unchanged",
			input: "# ADD --from=builder comment",
			want:  "# ADD --from=builder comment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeAddFromForBuildKit(tt.input)
			if got != tt.want {
				t.Fatalf("normalizeAddFromForBuildKit(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: parse instruction types comprehensively ----

func TestParse_ONBUILD(t *testing.T) {
	input := "FROM scratch\nONBUILD RUN echo triggered\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "ONBUILD" {
			found = true
			if len(instr.Args) != 1 || !strings.Contains(instr.Args[0], "RUN echo triggered") {
				t.Fatalf("ONBUILD args = %v", instr.Args)
			}
		}
	}
	if !found {
		t.Fatal("ONBUILD instruction not found")
	}
}

func TestParse_STOPSIGNAL(t *testing.T) {
	input := "FROM scratch\nSTOPSIGNAL SIGKILL\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "STOPSIGNAL" {
			found = true
			if len(instr.Args) != 1 || instr.Args[0] != "SIGKILL" {
				t.Fatalf("STOPSIGNAL args = %v", instr.Args)
			}
		}
	}
	if !found {
		t.Fatal("STOPSIGNAL instruction not found")
	}
}

func TestParse_SHELL(t *testing.T) {
	input := "FROM scratch\nSHELL [\"/bin/bash\", \"-eo\", \"pipefail\", \"-c\"]\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var shellInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "SHELL" {
			shellInstr = &instrs[i]
			break
		}
	}
	if shellInstr == nil {
		t.Fatal("SHELL instruction not found")
	}
	if len(shellInstr.Args) != 4 || shellInstr.Args[0] != "/bin/bash" {
		t.Fatalf("SHELL args = %v", shellInstr.Args)
	}
}

func TestParse_VOLUME(t *testing.T) {
	input := "FROM scratch\nVOLUME /data /logs\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var volInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "VOLUME" {
			volInstr = &instrs[i]
			break
		}
	}
	if volInstr == nil {
		t.Fatal("VOLUME instruction not found")
	}
	if len(volInstr.Args) < 2 {
		t.Fatalf("VOLUME args = %v, want at least 2", volInstr.Args)
	}
}

func TestParse_EXPOSE_Multiple(t *testing.T) {
	input := "FROM scratch\nEXPOSE 8080 9090/udp\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var expInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "EXPOSE" {
			expInstr = &instrs[i]
			break
		}
	}
	if expInstr == nil {
		t.Fatal("EXPOSE instruction not found")
	}
	if len(expInstr.Args) < 2 {
		t.Fatalf("EXPOSE args = %v, want at least 2", expInstr.Args)
	}
}

func TestParse_LABEL(t *testing.T) {
	input := "FROM scratch\nLABEL version=\"1.0\" maintainer=\"test@example.com\"\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var labelInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "LABEL" {
			labelInstr = &instrs[i]
			break
		}
	}
	if labelInstr == nil {
		t.Fatal("LABEL instruction not found")
	}
}

func TestParse_MultiStageNamedStages(t *testing.T) {
	input := `FROM golang:1.21 AS builder
WORKDIR /src
RUN echo build

FROM alpine:3.18 AS runner
COPY --from=builder /src /app

FROM scratch AS final
COPY --from=runner /app /app
CMD ["/app"]
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fromCount := 0
	for _, instr := range instrs {
		if instr.Cmd == "FROM" {
			fromCount++
		}
	}
	if fromCount != 3 {
		t.Fatalf("got %d FROM, want 3", fromCount)
	}
}

func TestParse_ARGBeforeFROM(t *testing.T) {
	input := `ARG BASE_IMAGE=ubuntu
ARG VERSION=22.04
FROM ${BASE_IMAGE}:${VERSION}
RUN echo hi
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[0].Cmd != "ARG" || instrs[1].Cmd != "ARG" {
		t.Fatalf("expected first two instructions to be ARG, got %s and %s", instrs[0].Cmd, instrs[1].Cmd)
	}
	if instrs[2].Cmd != "FROM" {
		t.Fatalf("expected FROM after ARGs, got %s", instrs[2].Cmd)
	}
}

func TestParse_HEALTHCHECK_NONE(t *testing.T) {
	input := "FROM scratch\nHEALTHCHECK NONE\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var hcInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "HEALTHCHECK" {
			hcInstr = &instrs[i]
			break
		}
	}
	if hcInstr == nil {
		t.Fatal("HEALTHCHECK instruction not found")
	}
	if len(hcInstr.Args) != 1 || !strings.EqualFold(hcInstr.Args[0], "NONE") {
		t.Fatalf("HEALTHCHECK args = %v, want [NONE]", hcInstr.Args)
	}
}

func TestParse_HEALTHCHECK_WithOptions(t *testing.T) {
	input := "FROM scratch\nHEALTHCHECK --interval=30s --timeout=10s --retries=3 CMD curl -f http://localhost/\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var hcInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "HEALTHCHECK" {
			hcInstr = &instrs[i]
			break
		}
	}
	if hcInstr == nil {
		t.Fatal("HEALTHCHECK instruction not found")
	}
	argsJoined := strings.Join(hcInstr.Args, " ")
	if !strings.Contains(argsJoined, "--interval=") {
		t.Fatalf("HEALTHCHECK args = %q, missing interval", argsJoined)
	}
}

func TestParse_COPYChmod(t *testing.T) {
	input := "FROM scratch\nCOPY --chmod=755 app /app\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if arg == "--chmod=755" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("COPY --chmod=755 not found")
	}
}

func TestParse_COPYChown(t *testing.T) {
	input := "FROM scratch\nCOPY --chown=1000:1000 app /app\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if arg == "--chown=1000:1000" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("COPY --chown not found")
	}
}

func TestParse_COPYLink(t *testing.T) {
	input := "FROM scratch\nCOPY --link app /app\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if arg == "--link" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("COPY --link not found")
	}
}

func TestParse_COPYExclude(t *testing.T) {
	input := "FROM scratch\nCOPY --exclude=*.tmp . /app\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if strings.HasPrefix(arg, "--exclude=") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("COPY --exclude not found")
	}
}

func TestParse_COPYParents(t *testing.T) {
	input := "FROM scratch\nCOPY --parents src/dir /app/\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if arg == "--parents" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("COPY --parents not found")
	}
}

func TestParse_ADDChecksum(t *testing.T) {
	input := "FROM scratch\nADD --checksum=sha256:abcdef1234567890 https://example.com/file.tar.gz /app/\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "ADD" {
			for _, arg := range instr.Args {
				if strings.HasPrefix(arg, "--checksum=") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("ADD --checksum not found")
	}
}

func TestParse_ADDKeepGitDir(t *testing.T) {
	input := "FROM scratch\nADD --keep-git-dir=true https://github.com/example/repo.git /app/\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "ADD" {
			for _, arg := range instr.Args {
				if strings.HasPrefix(arg, "--keep-git-dir=") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("ADD --keep-git-dir not found")
	}
}

func TestParse_RUNMountTmpfs(t *testing.T) {
	input := "FROM scratch\nRUN --mount=type=tmpfs,target=/tmp/build echo hi\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var runInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "RUN" {
			runInstr = &instrs[i]
			break
		}
	}
	if runInstr == nil {
		t.Fatal("no RUN instruction found")
	}
	if len(runInstr.RunMounts) != 1 || runInstr.RunMounts[0].Type != "tmpfs" {
		t.Fatalf("mount type = %v, want tmpfs", runInstr.RunMounts)
	}
}

func TestParse_RUNMountSecret(t *testing.T) {
	input := "FROM scratch\nRUN --mount=type=secret,id=mysecret,target=/run/secret cat /run/secret\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var runInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "RUN" {
			runInstr = &instrs[i]
			break
		}
	}
	if runInstr == nil {
		t.Fatal("no RUN instruction found")
	}
	if len(runInstr.RunMounts) != 1 || runInstr.RunMounts[0].Type != "secret" {
		t.Fatalf("mount = %#v, want type=secret", runInstr.RunMounts)
	}
	if runInstr.RunMounts[0].SecretID == "" {
		t.Fatal("expected SecretID to be set for secret mount")
	}
}

func TestParse_ENVOldForm(t *testing.T) {
	// Old single-pair form: ENV KEY VALUE (no equals sign)
	input := "FROM scratch\nENV MY_KEY my value with spaces\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var envInstr *Instruction
	for i := range instrs {
		if instrs[i].Cmd == "ENV" {
			envInstr = &instrs[i]
			break
		}
	}
	if envInstr == nil {
		t.Fatal("ENV instruction not found")
	}
	if len(envInstr.Args) < 2 {
		t.Fatalf("ENV args = %v, expected at least 2", envInstr.Args)
	}
}

// ---- NEW TESTS: expand with default/alt syntax ----

func TestExpand_DefaultSyntax(t *testing.T) {
	b := &builder{
		env:  map[string]string{},
		args: map[string]string{},
	}
	// ${VAR:-default} should return "default" when VAR is not set
	got := b.expand("${UNSET:-fallback}")
	if got != "fallback" {
		t.Fatalf("expand(${UNSET:-fallback}) = %q, want fallback", got)
	}
}

func TestExpand_AltSyntax(t *testing.T) {
	b := &builder{
		env:  map[string]string{"SET": "value"},
		args: map[string]string{},
	}
	// ${VAR:+alt} should return "alt" when VAR is set
	got := b.expand("${SET:+replacement}")
	if got != "replacement" {
		t.Fatalf("expand(${SET:+replacement}) = %q, want replacement", got)
	}
}

func TestExpand_AltSyntax_Unset(t *testing.T) {
	b := &builder{
		env:  map[string]string{},
		args: map[string]string{},
	}
	// ${VAR:+alt} should return "" when VAR is not set
	got := b.expand("${UNSET:+replacement}")
	if got != "" {
		t.Fatalf("expand(${UNSET:+replacement}) = %q, want empty", got)
	}
}

// ---- NEW TESTS: splitArgs edge cases ----

func TestSplitArgs_EmptyRaw(t *testing.T) {
	args := splitArgs("CMD", "")
	if args != nil {
		t.Fatalf("splitArgs(CMD, \"\") = %v, want nil", args)
	}
}

func TestSplitArgs_WhitespaceOnly(t *testing.T) {
	args := splitArgs("CMD", "   ")
	if args != nil {
		t.Fatalf("splitArgs(CMD, \"   \") = %v, want nil", args)
	}
}

func TestSplitArgs_ExecFormEntrypoint(t *testing.T) {
	args := splitArgs("ENTRYPOINT", `["/usr/bin/app","--config","/etc/app.conf"]`)
	if len(args) != 3 || args[0] != "/usr/bin/app" {
		t.Fatalf("splitArgs(ENTRYPOINT, exec form) = %v", args)
	}
}

// ---- NEW TESTS: hasDockerfileInstructions edge cases ----

func TestHasDockerfileInstructions_TabsAndSpaces(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"tabs only", "\t\t\n\t", false},
		{"instruction with leading space", "  FROM scratch", true},
		{"mixed comments and blank", "# comment\n\n# more\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasDockerfileInstructions(tt.content)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: normalizeAddFromForBuildKit additional cases ----

func TestNormalizeAddFromForBuildKit_Lowercase(t *testing.T) {
	input := "add --from=builder /app /app"
	got := normalizeAddFromForBuildKit(input)
	if !strings.HasPrefix(got, "COPY") {
		t.Fatalf("lowercase add --from should be converted, got %q", got)
	}
}

func TestNormalizeAddFromForBuildKit_NoFrom(t *testing.T) {
	input := "ADD file.tar /dest"
	got := normalizeAddFromForBuildKit(input)
	if got != input {
		t.Fatalf("ADD without --from should be unchanged, got %q", got)
	}
}

func TestNormalizeAddFromForBuildKit_IndentedAdd(t *testing.T) {
	input := "  ADD --from=builder /app /app"
	got := normalizeAddFromForBuildKit(input)
	if !strings.Contains(got, "COPY") {
		t.Fatalf("indented ADD --from should be converted, got %q", got)
	}
}

func TestNormalizeAddFromForBuildKit_MultipleLines(t *testing.T) {
	input := "FROM scratch\nADD --from=builder /app /app\nADD file.tar /dest\n"
	got := normalizeAddFromForBuildKit(input)
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[1], "COPY") {
		t.Fatalf("second line should be COPY, got %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "ADD") {
		t.Fatalf("third line should remain ADD, got %q", lines[2])
	}
}

// ---- NEW TESTS: commandConfigArgs ----

func TestCommandConfigArgs_EmptyArgs(t *testing.T) {
	b := &builder{shell: []string{"/bin/sh", "-c"}}
	got := b.commandConfigArgs(Instruction{
		Cmd:       "CMD",
		Args:      []string{},
		ShellForm: true,
	})
	if got != nil {
		t.Fatalf("commandConfigArgs(empty shell form) = %v, want nil", got)
	}
}

func TestCommandConfigArgs_ShellFormWithCustomShell(t *testing.T) {
	b := &builder{shell: []string{"/bin/bash", "-eo", "pipefail", "-c"}}
	got := b.commandConfigArgs(Instruction{
		Cmd:       "ENTRYPOINT",
		Args:      []string{"start.sh"},
		ShellForm: true,
	})
	want := []string{"/bin/bash", "-eo", "pipefail", "-c", "start.sh"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// ---- NEW TESTS: buildRunCommand additional cases ----

func TestBuildRunCommand_ExecForm(t *testing.T) {
	b := &builder{
		shell:   []string{"/bin/sh", "-c"},
		workdir: "",
	}
	got := b.buildRunCommand([]string{"/usr/bin/app", "--flag"})
	if len(got) != 2 || got[0] != "/usr/bin/app" {
		t.Fatalf("exec form buildRunCommand = %v", got)
	}
}

func TestBuildRunCommand_ExecFormWithWorkdir(t *testing.T) {
	b := &builder{
		shell:   []string{"/bin/sh", "-c"},
		workdir: "/app",
	}
	got := b.buildRunCommand([]string{"/usr/bin/app", "--flag"})
	// Should wrap with shell to cd first
	if len(got) < 4 {
		t.Fatalf("buildRunCommand with workdir and exec form = %v", got)
	}
}

func TestBuildRunCommand_EmptyArgs(t *testing.T) {
	b := &builder{shell: []string{"/bin/sh", "-c"}}
	got := b.buildRunCommand(nil)
	if got != nil {
		t.Fatalf("buildRunCommand(nil) = %v, want nil", got)
	}
}

// ---- NEW TESTS: parseInstruction edge cases ----

func TestParseInstruction_EmptyInput(t *testing.T) {
	_, err := parseInstruction("")
	if err == nil {
		t.Fatal("expected error for empty instruction")
	}
}

func TestParseInstruction_CMDExecForm(t *testing.T) {
	instr, err := parseInstruction(`CMD ["echo", "hello"]`)
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "CMD" {
		t.Fatalf("cmd = %q, want CMD", instr.Cmd)
	}
	if instr.ShellForm {
		t.Fatal("exec form should not be marked as ShellForm")
	}
}

func TestParseInstruction_CMDShellForm(t *testing.T) {
	instr, err := parseInstruction("CMD echo hello world")
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if !instr.ShellForm {
		t.Fatal("shell form should be marked as ShellForm")
	}
}

func TestParseInstruction_ENTRYPOINTExecForm(t *testing.T) {
	instr, err := parseInstruction(`ENTRYPOINT ["/bin/app"]`)
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "ENTRYPOINT" || instr.ShellForm {
		t.Fatalf("cmd=%q shellForm=%v", instr.Cmd, instr.ShellForm)
	}
	if len(instr.Args) != 1 || instr.Args[0] != "/bin/app" {
		t.Fatalf("args = %v", instr.Args)
	}
}

// ---- NEW TESTS: resolveContextMountSource ----

func TestResolveContextMountSource_EmptySource(t *testing.T) {
	root := t.TempDir()
	got, err := resolveContextMountSource(root, "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got != root {
		t.Fatalf("got %q, want %q", got, root)
	}
}

func TestResolveContextMountSource_ValidSubdir(t *testing.T) {
	root := t.TempDir()
	got, err := resolveContextMountSource(root, "subdir/path")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	want := filepath.Join(root, "subdir/path")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveContextMountSource_AbsoluteSource(t *testing.T) {
	root := t.TempDir()
	got, err := resolveContextMountSource(root, "/absolute/path")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	want := filepath.Join(root, "absolute/path")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ---- NEW TESTS: sanitizeRunMountCacheKey additional cases ----

func TestSanitizeRunMountCacheKey_Complex(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/root/.cache/pip", "root_.cache_pip"},
		{"/go/pkg/mod", "go_pkg_mod"},
		{"relative/path", "relative_path"},
		{"  /trimmed/  ", "trimmed"},
	}
	for _, tt := range tests {
		got := sanitizeRunMountCacheKey(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeRunMountCacheKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- NEW TESTS: parseFromArgs ----

func TestParseFromArgs_Empty(t *testing.T) {
	_, _, err := parseFromArgs(nil)
	if err == nil {
		t.Fatal("expected error for empty args")
	}
}

func TestParseFromArgs_ImageOnly(t *testing.T) {
	image, alias, err := parseFromArgs([]string{"ubuntu:22.04"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if image != "ubuntu:22.04" || alias != "" {
		t.Fatalf("image=%q alias=%q", image, alias)
	}
}

func TestParseFromArgs_WithAS(t *testing.T) {
	image, alias, err := parseFromArgs([]string{"golang:1.21", "AS", "builder"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if image != "golang:1.21" || alias != "builder" {
		t.Fatalf("image=%q alias=%q", image, alias)
	}
}

// ---- NEW TESTS: MAINTAINER (deprecated) ----

func TestParse_MAINTAINER(t *testing.T) {
	input := "FROM scratch\nMAINTAINER test@example.com\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "MAINTAINER" {
			found = true
			if len(instr.Args) != 1 || instr.Args[0] != "test@example.com" {
				t.Fatalf("MAINTAINER args = %v", instr.Args)
			}
		}
	}
	if !found {
		t.Fatal("MAINTAINER instruction not found")
	}
}

// ---- NEW TESTS: COPY --from with numeric index ----

func TestParse_COPYFromNumeric(t *testing.T) {
	input := "FROM scratch\nRUN echo build\nFROM scratch\nCOPY --from=0 /app /app\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if arg == "--from=0" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("COPY --from=0 not found")
	}
}

// ---- NEW TESTS: ADD --from becomes COPY (via normalizeAddFromForBuildKit) ----

func TestParse_ADDFromBecomesCOPY(t *testing.T) {
	input := "FROM scratch AS builder\nRUN echo build\nFROM scratch\nADD --from=builder /app /app\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// ADD --from= should have been converted to COPY --from= by normalizeAddFromForBuildKit
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if arg == "--from=builder" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("ADD --from=builder should have been converted to COPY --from=builder")
	}
}

// --- New coverage-boosting tests ---

func TestParseFromArgs_Comprehensive(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantImage string
		wantAlias string
		wantErr   bool
	}{
		{"nil args", nil, "", "", true},
		{"empty args", []string{}, "", "", true},
		{"image only", []string{"ubuntu:22.04"}, "ubuntu:22.04", "", false},
		{"image with AS", []string{"golang:1.21", "AS", "builder"}, "golang:1.21", "builder", false},
		{"image with lowercase as", []string{"golang:1.21", "as", "builder"}, "golang:1.21", "builder", false},
		{"image with As mixed case", []string{"golang:1.21", "As", "builder"}, "golang:1.21", "builder", false},
		{"scratch", []string{"scratch"}, "scratch", "", false},
		{"scratch with alias", []string{"scratch", "AS", "base"}, "scratch", "base", false},
		{"two args invalid", []string{"ubuntu", "extra"}, "", "", true},
		{"four args invalid", []string{"ubuntu", "AS", "b", "extra"}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			image, alias, err := parseFromArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFromArgs(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if image != tt.wantImage {
				t.Errorf("image = %q, want %q", image, tt.wantImage)
			}
			if alias != tt.wantAlias {
				t.Errorf("alias = %q, want %q", alias, tt.wantAlias)
			}
		})
	}
}

func TestParseHealthcheck_Comprehensive(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(t *testing.T, hc *oci.Healthcheck)
	}{
		{
			name:    "empty args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "NONE",
			args:    []string{"NONE"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if len(hc.Test) != 1 || hc.Test[0] != "NONE" {
					t.Fatalf("test = %v, want [NONE]", hc.Test)
				}
			},
		},
		{
			name:    "none lowercase",
			args:    []string{"none"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if len(hc.Test) != 1 {
					t.Fatalf("test = %v", hc.Test)
				}
			},
		},
		{
			name:    "CMD only",
			args:    []string{"CMD", "curl", "-f", "http://localhost/"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if len(hc.Test) != 4 || hc.Test[0] != "CMD" {
					t.Fatalf("test = %v", hc.Test)
				}
			},
		},
		{
			name:    "with interval",
			args:    []string{"--interval=30s", "CMD", "curl", "-f", "http://localhost/"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if hc.Interval != 30*time.Second {
					t.Fatalf("interval = %v, want 30s", hc.Interval)
				}
			},
		},
		{
			name:    "with timeout",
			args:    []string{"--timeout=10s", "CMD", "true"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if hc.Timeout != 10*time.Second {
					t.Fatalf("timeout = %v, want 10s", hc.Timeout)
				}
			},
		},
		{
			name:    "with retries",
			args:    []string{"--retries=5", "CMD", "true"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if hc.Retries != 5 {
					t.Fatalf("retries = %d, want 5", hc.Retries)
				}
			},
		},
		{
			name:    "with start-period",
			args:    []string{"--start-period=1m", "CMD", "true"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if hc.StartPeriod != time.Minute {
					t.Fatalf("start-period = %v, want 1m", hc.StartPeriod)
				}
			},
		},
		{
			name:    "with start-interval",
			args:    []string{"--start-interval=5s", "CMD", "true"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if hc.StartInterval != 5*time.Second {
					t.Fatalf("start-interval = %v, want 5s", hc.StartInterval)
				}
			},
		},
		{
			name:    "all options",
			args:    []string{"--interval=30s", "--timeout=10s", "--start-period=1m", "--retries=3", "CMD", "curl", "localhost"},
			wantErr: false,
			check: func(t *testing.T, hc *oci.Healthcheck) {
				if hc.Interval != 30*time.Second {
					t.Errorf("interval = %v", hc.Interval)
				}
				if hc.Timeout != 10*time.Second {
					t.Errorf("timeout = %v", hc.Timeout)
				}
				if hc.StartPeriod != time.Minute {
					t.Errorf("start-period = %v", hc.StartPeriod)
				}
				if hc.Retries != 3 {
					t.Errorf("retries = %d", hc.Retries)
				}
			},
		},
		{
			name:    "invalid interval",
			args:    []string{"--interval=xyz", "CMD", "true"},
			wantErr: true,
		},
		{
			name:    "invalid retries",
			args:    []string{"--retries=abc", "CMD", "true"},
			wantErr: true,
		},
		{
			name:    "unsupported option",
			args:    []string{"--unknown=val", "CMD", "true"},
			wantErr: true,
		},
		{
			name:    "flag without value",
			args:    []string{"--interval", "CMD", "true"},
			wantErr: true,
		},
		{
			name:    "only flags, no command",
			args:    []string{"--interval=5s"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, err := parseHealthcheck(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseHealthcheck(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.check != nil {
				tt.check(t, hc)
			}
		})
	}
}

func TestRootfsPath_Comprehensive(t *testing.T) {
	tests := []struct {
		rootfs string
		path   string
		want   string
	}{
		{"/rootfs", "/", "/rootfs"},
		{"/rootfs", ".", "/rootfs"},
		{"/rootfs", "/app", "/rootfs/app"},
		{"/rootfs", "/usr/bin", "/rootfs/usr/bin"},
		{"/rootfs", "relative", "/rootfs/relative"},
		{"/rootfs", "/./normalized/../app", "/rootfs/app"},
	}
	for _, tt := range tests {
		got := rootfsPath(tt.rootfs, tt.path)
		if got != tt.want {
			t.Errorf("rootfsPath(%q, %q) = %q, want %q", tt.rootfs, tt.path, got, tt.want)
		}
	}
}

func TestShellQuote_Comprehensive(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"with spaces", "'with spaces'"},
		{"", "''"},
		{"has'quote", "has'quote"}, // verify it contains escaped quote
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if tt.input == "has'quote" {
			// The result should contain the escaped form
			if !strings.Contains(got, `'"'"'`) {
				t.Errorf("shellQuote(%q) = %q, want escaped single quote", tt.input, got)
			}
		} else if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCloneStringSlice(t *testing.T) {
	tests := []struct {
		name  string
		input []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"one", []string{"a"}},
		{"multiple", []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cloneStringSlice(tt.input)
			if len(tt.input) == 0 {
				if got != nil {
					t.Fatalf("cloneStringSlice(%v) = %v, want nil", tt.input, got)
				}
				return
			}
			if len(got) != len(tt.input) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.input))
			}
			for i := range got {
				if got[i] != tt.input[i] {
					t.Fatalf("mismatch at %d: %q vs %q", i, got[i], tt.input[i])
				}
			}
			// Verify it's a deep copy
			if len(got) > 0 {
				got[0] = "modified"
				if tt.input[0] == "modified" {
					t.Fatal("modifying clone affected original")
				}
			}
		})
	}
}

func TestCloneStringMap(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]string
	}{
		{"nil", nil},
		{"empty", map[string]string{}},
		{"one", map[string]string{"a": "1"}},
		{"multiple", map[string]string{"a": "1", "b": "2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cloneStringMap(tt.input)
			if len(got) != len(tt.input) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.input))
			}
			for k, v := range tt.input {
				if got[k] != v {
					t.Fatalf("mismatch for key %q: %q vs %q", k, got[k], v)
				}
			}
			// Verify deep copy
			if len(got) > 0 {
				got["__test__"] = "modified"
				if tt.input["__test__"] == "modified" {
					t.Fatal("modifying clone affected original")
				}
			}
		})
	}
}

func TestAppendUnique(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		value  string
		want   int
	}{
		{"add to empty", nil, "a", 1},
		{"add new", []string{"a"}, "b", 2},
		{"duplicate", []string{"a", "b"}, "a", 2},
		{"add to many", []string{"a", "b", "c"}, "d", 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendUnique(tt.values, tt.value)
			if len(got) != tt.want {
				t.Fatalf("len = %d, want %d; got %v", len(got), tt.want, got)
			}
		})
	}
}

func TestSortedStringMapKeys(t *testing.T) {
	m := map[string]string{"c": "3", "a": "1", "b": "2"}
	keys := sortedStringMapKeys(m)
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Fatalf("sortedStringMapKeys = %v, want [a b c]", keys)
	}
}

func TestSortedStringMapKeys_Empty(t *testing.T) {
	keys := sortedStringMapKeys(nil)
	if len(keys) != 0 {
		t.Fatalf("sortedStringMapKeys(nil) = %v, want empty", keys)
	}
}

func TestSortedKeys(t *testing.T) {
	set := map[string]struct{}{"z": {}, "a": {}, "m": {}}
	keys := sortedKeys(set)
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "m" || keys[2] != "z" {
		t.Fatalf("sortedKeys = %v, want [a m z]", keys)
	}
}

func TestDefaultBuildArgs(t *testing.T) {
	args := defaultBuildArgs()
	requiredKeys := []string{"BUILDOS", "BUILDARCH", "BUILDPLATFORM", "TARGETOS", "TARGETARCH", "TARGETPLATFORM"}
	for _, key := range requiredKeys {
		if _, ok := args[key]; !ok {
			t.Errorf("defaultBuildArgs missing key %q", key)
		}
	}
	if args["BUILDOS"] == "" {
		t.Error("BUILDOS should not be empty")
	}
	if args["BUILDARCH"] == "" {
		t.Error("BUILDARCH should not be empty")
	}
}

func TestValidateBuildPlatform(t *testing.T) {
	hostPlatform := runtime.GOOS + "/" + runtime.GOARCH
	tests := []struct {
		platform string
		wantErr  bool
	}{
		{hostPlatform, false},
		{"fakeos/fakearch", true}, // cross-compile not supported
		{"linux", true},           // missing arch
		{"", true},
	}
	for _, tt := range tests {
		err := validateBuildPlatform(tt.platform)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateBuildPlatform(%q) err = %v, wantErr %v", tt.platform, err, tt.wantErr)
		}
	}
}

func TestHasDockerfileInstructions_Comprehensive(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"whitespace", "   \n  \n", false},
		{"comments only", "# comment\n# another\n", false},
		{"FROM", "FROM scratch\n", true},
		{"comment then FROM", "# comment\nFROM scratch\n", true},
		{"blank then FROM", "\n\nFROM scratch\n", true},
		{"tab only", "\t\n", false},
		{"non-FROM instruction", "RUN echo hi\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasDockerfileInstructions(tt.content)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParse_FROM_PlatformFlag(t *testing.T) {
	input := `FROM --platform=linux/amd64 ubuntu:22.04 AS builder
RUN echo hi
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if instrs[0].Cmd != "FROM" {
		t.Fatalf("first instruction = %q, want FROM", instrs[0].Cmd)
	}
	if instrs[0].Platform != "linux/amd64" {
		t.Errorf("platform = %q, want linux/amd64", instrs[0].Platform)
	}
	args := strings.Join(instrs[0].Args, " ")
	if !strings.Contains(args, "AS") || !strings.Contains(args, "builder") {
		t.Errorf("FROM args = %q, want AS builder", args)
	}
}

func TestParse_MultiStageWithNumberedFrom(t *testing.T) {
	input := `FROM alpine:3.18 AS stage0
RUN echo build0

FROM alpine:3.18 AS stage1
COPY --from=0 /app /app
RUN echo build1

FROM scratch
COPY --from=1 /app /final
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fromCount := 0
	copyFromCount := 0
	for _, instr := range instrs {
		if instr.Cmd == "FROM" {
			fromCount++
		}
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if strings.HasPrefix(arg, "--from=") {
					copyFromCount++
				}
			}
		}
	}
	if fromCount != 3 {
		t.Errorf("FROM count = %d, want 3", fromCount)
	}
	if copyFromCount != 2 {
		t.Errorf("COPY --from count = %d, want 2", copyFromCount)
	}
}

func TestParse_CommentsBetweenInstructions(t *testing.T) {
	input := `# Build stage
FROM scratch
# This is a comment between instructions
RUN echo first
# Another comment
# Multi-line comment
RUN echo second
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 3 {
		t.Fatalf("got %d instructions, want 3 (FROM + 2 RUN)", len(instrs))
	}
}

func TestParse_BlankLinesBetweenInstructions(t *testing.T) {
	input := `FROM scratch


RUN echo first


RUN echo second

`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 3 {
		t.Fatalf("got %d instructions, want 3", len(instrs))
	}
}

func TestExpand_NestedVariables(t *testing.T) {
	b := &builder{
		env:  map[string]string{"INNER": "world"},
		args: map[string]string{"OUTER": "hello"},
	}
	got := b.expand("${OUTER} ${INNER}")
	if got != "hello world" {
		t.Errorf("expand with two vars = %q, want 'hello world'", got)
	}
}

func TestExpand_EscapedDollar(t *testing.T) {
	b := &builder{
		env:  map[string]string{},
		args: map[string]string{},
	}
	// \$ should produce literal $
	got := b.expand(`\$HOME`)
	// The lex processor should treat \$ as literal $
	if !strings.Contains(got, "$") {
		t.Errorf("expand(\\$HOME) = %q, want literal $", got)
	}
}

func TestExpand_UndefinedVariable(t *testing.T) {
	b := &builder{
		env:  map[string]string{},
		args: map[string]string{},
	}
	// Undefined variable should expand to empty
	got := b.expand("$UNDEFINED_VAR")
	if got != "" {
		t.Errorf("expand($UNDEFINED_VAR) = %q, want empty", got)
	}
}

func TestBuildRunCommand_MultipleArgs(t *testing.T) {
	b := &builder{
		shell:   []string{"/bin/sh", "-c"},
		workdir: "/app",
	}
	got := b.buildRunCommand([]string{"/usr/bin/app", "--port", "8080"})
	// With workdir and multiple args, should wrap with cd
	if len(got) < 5 {
		t.Fatalf("buildRunCommand = %v, expected wrapping", got)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "/app") {
		t.Errorf("expected workdir in result: %v", got)
	}
}

func TestBuildRunCommand_NoShell(t *testing.T) {
	b := &builder{
		shell:   nil,
		workdir: "",
	}
	got := b.buildRunCommand([]string{"echo hello"})
	// With nil shell, should still use default shell behavior
	if len(got) == 0 {
		t.Fatal("buildRunCommand with nil shell returned empty")
	}
}

func TestSplitArgsWithForm_ShellFormRUN(t *testing.T) {
	args, shellForm := splitArgsWithForm("RUN", "apt-get update")
	if !shellForm {
		t.Error("RUN should be shell form")
	}
	if len(args) != 1 || args[0] != "apt-get update" {
		t.Fatalf("args = %v", args)
	}
}

func TestSplitArgsWithForm_ExecFormCMD(t *testing.T) {
	args, shellForm := splitArgsWithForm("CMD", `["echo","hello"]`)
	if shellForm {
		t.Error("JSON form should not be shell form")
	}
	if len(args) != 2 || args[0] != "echo" {
		t.Fatalf("args = %v", args)
	}
}

func TestSplitArgsWithForm_Empty(t *testing.T) {
	args, shellForm := splitArgsWithForm("CMD", "")
	if args != nil {
		t.Fatalf("empty input should return nil, got %v", args)
	}
	if shellForm {
		t.Error("empty should not be shell form")
	}
}

func TestParseInstruction_WorkdirArg(t *testing.T) {
	instr, err := parseInstruction("WORKDIR /usr/local/app")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "WORKDIR" {
		t.Fatalf("cmd = %q, want WORKDIR", instr.Cmd)
	}
	if len(instr.Args) != 1 || instr.Args[0] != "/usr/local/app" {
		t.Fatalf("args = %v", instr.Args)
	}
}

func TestParseInstruction_UserArg(t *testing.T) {
	instr, err := parseInstruction("USER nobody:nogroup")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "USER" || len(instr.Args) != 1 || instr.Args[0] != "nobody:nogroup" {
		t.Fatalf("cmd=%q args=%v", instr.Cmd, instr.Args)
	}
}

func TestParseInstruction_EnvKeyValue(t *testing.T) {
	instr, err := parseInstruction("ENV MY_VAR=hello")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "ENV" {
		t.Fatalf("cmd = %q, want ENV", instr.Cmd)
	}
}

func TestParseInstruction_VolumeMultiple(t *testing.T) {
	instr, err := parseInstruction("VOLUME /data /logs /tmp")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "VOLUME" {
		t.Fatalf("cmd = %q, want VOLUME", instr.Cmd)
	}
	if len(instr.Args) < 2 {
		t.Fatalf("args = %v, want at least 2", instr.Args)
	}
}

func TestParseInstruction_LabelInstruction(t *testing.T) {
	instr, err := parseInstruction(`LABEL version="1.0" maintainer="test"`)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "LABEL" {
		t.Fatalf("cmd = %q, want LABEL", instr.Cmd)
	}
}

func TestParseInstruction_StopsignalInstruction(t *testing.T) {
	instr, err := parseInstruction("STOPSIGNAL SIGTERM")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "STOPSIGNAL" || len(instr.Args) != 1 || instr.Args[0] != "SIGTERM" {
		t.Fatalf("cmd=%q args=%v", instr.Cmd, instr.Args)
	}
}

func TestEnsureSymlink_CreateNew(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "link")
	if err := ensureSymlink(path, "/proc/self/fd"); err != nil {
		t.Fatalf("ensureSymlink: %v", err)
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "/proc/self/fd" {
		t.Fatalf("target = %q, want /proc/self/fd", target)
	}
}

func TestEnsureSymlink_UpdateExisting(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "link")
	os.Symlink("/old/target", path)

	if err := ensureSymlink(path, "/new/target"); err != nil {
		t.Fatalf("ensureSymlink: %v", err)
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "/new/target" {
		t.Fatalf("target = %q, want /new/target", target)
	}
}

func TestEnsureSymlink_SameTarget(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "link")
	os.Symlink("/same/target", path)

	if err := ensureSymlink(path, "/same/target"); err != nil {
		t.Fatalf("ensureSymlink: %v", err)
	}
	target, _ := os.Readlink(path)
	if target != "/same/target" {
		t.Fatalf("target = %q", target)
	}
}

func TestEnsureSymlink_ReplaceFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "link")
	os.WriteFile(path, []byte("not a symlink"), 0644)

	if err := ensureSymlink(path, "/some/target"); err != nil {
		t.Fatalf("ensureSymlink: %v", err)
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "/some/target" {
		t.Fatalf("target = %q", target)
	}
}

func TestParse_SHELLAffectsSubsequentRUN(t *testing.T) {
	input := `FROM scratch
SHELL ["/bin/bash", "-eo", "pipefail", "-c"]
RUN echo hello | wc -l
`
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 3 {
		t.Fatalf("got %d instructions, want 3", len(instrs))
	}
	if instrs[1].Cmd != "SHELL" {
		t.Fatalf("second instruction = %q, want SHELL", instrs[1].Cmd)
	}
	if instrs[2].Cmd != "RUN" {
		t.Fatalf("third instruction = %q, want RUN", instrs[2].Cmd)
	}
}

func TestParse_RUN_HeredocChomp(t *testing.T) {
	// Heredoc with chomp option (<<-) is handled by BuildKit parser
	input := "FROM scratch\nRUN <<EOF\nline1\nline2\nEOF\n"
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, instr := range instrs {
		if instr.Cmd == "RUN" {
			found = true
			if !strings.Contains(strings.Join(instr.Args, " "), "line1") {
				t.Fatalf("heredoc should contain line1: %v", instr.Args)
			}
		}
	}
	if !found {
		t.Fatal("RUN instruction not found")
	}
}

// ---- Coverage: parseFromArgs comprehensive ----

func TestParseFromArgs_TwoArgs(t *testing.T) {
	// Two args without AS is an error
	_, _, err := parseFromArgs([]string{"ubuntu", "extra"})
	if err == nil {
		t.Fatal("expected error for two args without AS")
	}
}

func TestParseFromArgs_FourArgs(t *testing.T) {
	_, _, err := parseFromArgs([]string{"ubuntu", "AS", "base", "extra"})
	if err == nil {
		t.Fatal("expected error for four args")
	}
}

func TestParseFromArgs_CaseInsensitiveAS(t *testing.T) {
	image, alias, err := parseFromArgs([]string{"ubuntu", "as", "base"})
	if err != nil {
		t.Fatalf("parseFromArgs: %v", err)
	}
	if image != "ubuntu" || alias != "base" {
		t.Fatalf("image=%q alias=%q", image, alias)
	}
}

// ---- Coverage: parseHealthcheck comprehensive ----

func TestParseHealthcheck_Empty(t *testing.T) {
	_, err := parseHealthcheck(nil)
	if err == nil {
		t.Fatal("expected error for empty args")
	}
}

func TestParseHealthcheck_NONE(t *testing.T) {
	hc, err := parseHealthcheck([]string{"NONE"})
	if err != nil {
		t.Fatalf("parseHealthcheck: %v", err)
	}
	if len(hc.Test) != 1 || hc.Test[0] != "NONE" {
		t.Fatalf("test = %v", hc.Test)
	}
}

func TestParseHealthcheck_AllOptions(t *testing.T) {
	hc, err := parseHealthcheck([]string{
		"--interval=30s",
		"--timeout=10s",
		"--start-period=5s",
		"--start-interval=2s",
		"--retries=3",
		"CMD", "curl", "-f", "http://localhost/",
	})
	if err != nil {
		t.Fatalf("parseHealthcheck: %v", err)
	}
	if hc.Interval != 30*time.Second {
		t.Fatalf("Interval = %s", hc.Interval)
	}
	if hc.Timeout != 10*time.Second {
		t.Fatalf("Timeout = %s", hc.Timeout)
	}
	if hc.StartPeriod != 5*time.Second {
		t.Fatalf("StartPeriod = %s", hc.StartPeriod)
	}
	if hc.StartInterval != 2*time.Second {
		t.Fatalf("StartInterval = %s", hc.StartInterval)
	}
	if hc.Retries != 3 {
		t.Fatalf("Retries = %d", hc.Retries)
	}
	// Test includes CMD + the remaining args
	if len(hc.Test) != 4 {
		t.Fatalf("Test = %v, want 4 elements", hc.Test)
	}
}

func TestParseHealthcheck_InvalidDuration(t *testing.T) {
	_, err := parseHealthcheck([]string{"--interval=badvalue", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestParseHealthcheck_InvalidRetries(t *testing.T) {
	_, err := parseHealthcheck([]string{"--retries=abc", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for invalid retries")
	}
}

func TestParseHealthcheck_UnknownOption(t *testing.T) {
	_, err := parseHealthcheck([]string{"--unknown=value", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for unknown option")
	}
}

func TestParseHealthcheck_MissingCommand(t *testing.T) {
	_, err := parseHealthcheck([]string{"--interval=5s"})
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestParseHealthcheck_FlagWithoutEquals(t *testing.T) {
	_, err := parseHealthcheck([]string{"--interval", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for flag without equals")
	}
}

func TestParseHealthcheck_TimeoutInvalid(t *testing.T) {
	_, err := parseHealthcheck([]string{"--timeout=bad", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for invalid timeout")
	}
}

func TestParseHealthcheck_StartPeriodInvalid(t *testing.T) {
	_, err := parseHealthcheck([]string{"--start-period=bad", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for invalid start-period")
	}
}

func TestParseHealthcheck_StartIntervalInvalid(t *testing.T) {
	_, err := parseHealthcheck([]string{"--start-interval=bad", "CMD", "true"})
	if err == nil {
		t.Fatal("expected error for invalid start-interval")
	}
}

// ---- Coverage: rootfsPath ----

func TestRootfsPath_Cases(t *testing.T) {
	tests := []struct {
		rootfs, path, want string
	}{
		{"/rootfs", "/", "/rootfs"},
		{"/rootfs", ".", "/rootfs"},
		{"/rootfs", "/etc/passwd", "/rootfs/etc/passwd"},
		{"/rootfs", "relative/path", "/rootfs/relative/path"},
		{"/rootfs", "/./normalized", "/rootfs/normalized"},
	}
	for _, tt := range tests {
		got := rootfsPath(tt.rootfs, tt.path)
		if got != tt.want {
			t.Errorf("rootfsPath(%q, %q) = %q, want %q", tt.rootfs, tt.path, got, tt.want)
		}
	}
}

// ---- Coverage: shellQuote ----

func TestShellQuote_Cases(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"hello", "'hello'"},
		{"it's", "'it'\"'\"'s'"},
		{"", "''"},
		{"a b c", "'a b c'"},
		{"/path/to/dir", "'/path/to/dir'"},
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- Coverage: cloneStringSlice ----

func TestCloneStringSlice_Nil(t *testing.T) {
	if got := cloneStringSlice(nil); got != nil {
		t.Fatalf("cloneStringSlice(nil) = %v", got)
	}
}

func TestCloneStringSlice_Empty(t *testing.T) {
	if got := cloneStringSlice([]string{}); got != nil {
		t.Fatalf("cloneStringSlice([]) = %v", got)
	}
}

func TestCloneStringSlice_Independent(t *testing.T) {
	orig := []string{"a", "b"}
	clone := cloneStringSlice(orig)
	clone[0] = "x"
	if orig[0] == "x" {
		t.Fatal("clone should be independent of original")
	}
}

// ---- Coverage: cloneStringMap ----

func TestCloneStringMap_Empty(t *testing.T) {
	got := cloneStringMap(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("cloneStringMap(nil) = %v", got)
	}
}

func TestCloneStringMap_Independent(t *testing.T) {
	orig := map[string]string{"a": "1"}
	clone := cloneStringMap(orig)
	clone["a"] = "2"
	if orig["a"] == "2" {
		t.Fatal("clone should be independent")
	}
}

// ---- Coverage: appendUnique ----

func TestAppendUnique_Cases(t *testing.T) {
	got := appendUnique([]string{"a", "b"}, "c")
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	got = appendUnique([]string{"a", "b"}, "a")
	if len(got) != 2 {
		t.Fatalf("expected 2 (no dup), got %d", len(got))
	}
	got = appendUnique(nil, "x")
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
}

// ---- Coverage: sortedKeys and sortedStringMapKeys ----

func TestSortedKeys_Cases(t *testing.T) {
	got := sortedKeys(map[string]struct{}{"c": {}, "a": {}, "b": {}})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %v", got)
	}
	got = sortedKeys(nil)
	if len(got) != 0 {
		t.Fatalf("got %v for nil", got)
	}
}

func TestSortedStringMapKeys_Cases(t *testing.T) {
	got := sortedStringMapKeys(map[string]string{"z": "1", "a": "2"})
	if len(got) != 2 || got[0] != "a" || got[1] != "z" {
		t.Fatalf("got %v", got)
	}
}

// ---- Coverage: defaultBuildArgs ----

func TestDefaultBuildArgs_HasExpectedKeys(t *testing.T) {
	args := defaultBuildArgs()
	expectedKeys := []string{"BUILDOS", "BUILDARCH", "BUILDPLATFORM", "TARGETOS", "TARGETARCH", "TARGETPLATFORM"}
	for _, key := range expectedKeys {
		if _, ok := args[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	if args["BUILDOS"] != runtime.GOOS {
		t.Fatalf("BUILDOS = %q, want %q", args["BUILDOS"], runtime.GOOS)
	}
	if args["BUILDARCH"] != runtime.GOARCH {
		t.Fatalf("BUILDARCH = %q, want %q", args["BUILDARCH"], runtime.GOARCH)
	}
}

// ---- Coverage: validateBuildPlatform ----

func TestValidateBuildPlatform_Cases(t *testing.T) {
	// Current platform should pass
	currentPlatform := runtime.GOOS + "/" + runtime.GOARCH
	if err := validateBuildPlatform(currentPlatform); err != nil {
		t.Fatalf("validateBuildPlatform(%q): %v", currentPlatform, err)
	}
	// No slash should fail
	if err := validateBuildPlatform("noslash"); err == nil {
		t.Fatal("expected error for no slash")
	}
	// Different platform should fail
	if err := validateBuildPlatform("fakeos/fakearch"); err == nil {
		t.Fatal("expected error for different platform")
	}
	// With variant should also work if OS/ARCH match
	withVariant := currentPlatform + "/v7"
	if err := validateBuildPlatform(withVariant); err != nil {
		t.Fatalf("validateBuildPlatform(%q): %v", withVariant, err)
	}
}

// ---- Coverage: ensureSymlink (edge case: non-symlink file exists) ----

func TestEnsureSymlink_NonSymlinkFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	// Create a regular file where the symlink should go
	if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ensureSymlink(path, "/target"); err != nil {
		t.Fatalf("ensureSymlink: %v", err)
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != "/target" {
		t.Fatalf("target = %q, want /target", got)
	}
}

func TestEnsureSymlink_DirExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	// Create a directory where the symlink should go
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := ensureSymlink(path, "/target"); err != nil {
		t.Fatalf("ensureSymlink: %v", err)
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != "/target" {
		t.Fatalf("target = %q, want /target", got)
	}
}

func TestEnsureSymlink_WrongTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	if err := os.Symlink("/old-target", path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := ensureSymlink(path, "/new-target"); err != nil {
		t.Fatalf("ensureSymlink: %v", err)
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != "/new-target" {
		t.Fatalf("target = %q, want /new-target", got)
	}
}

// ---- Coverage: parseInstruction for various instruction types ----

func TestParseInstruction_RUNExecForm(t *testing.T) {
	instr, err := parseInstruction(`RUN ["ls", "-la"]`)
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "RUN" {
		t.Fatalf("cmd = %q", instr.Cmd)
	}
	if instr.ShellForm {
		t.Fatal("expected exec form")
	}
}

func TestParseInstruction_ExposeMultiple(t *testing.T) {
	instr, err := parseInstruction("EXPOSE 8080 9090")
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "EXPOSE" {
		t.Fatalf("cmd = %q", instr.Cmd)
	}
	if len(instr.Args) != 2 {
		t.Fatalf("args = %v, want 2 ports", instr.Args)
	}
}

func TestParseInstruction_MaintainerInstruction(t *testing.T) {
	instr, err := parseInstruction("MAINTAINER test@example.com")
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "MAINTAINER" {
		t.Fatalf("cmd = %q", instr.Cmd)
	}
}

func TestParseInstruction_ShellInstruction(t *testing.T) {
	instr, err := parseInstruction(`SHELL ["/bin/bash", "-c"]`)
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "SHELL" {
		t.Fatalf("cmd = %q", instr.Cmd)
	}
}

func TestParseInstruction_OnbuildInstruction(t *testing.T) {
	instr, err := parseInstruction("ONBUILD RUN echo hi")
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "ONBUILD" {
		t.Fatalf("cmd = %q", instr.Cmd)
	}
}

func TestParseInstruction_HealthcheckNone(t *testing.T) {
	instr, err := parseInstruction("HEALTHCHECK NONE")
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "HEALTHCHECK" {
		t.Fatalf("cmd = %q", instr.Cmd)
	}
}

func TestParseInstruction_HealthcheckCMD(t *testing.T) {
	instr, err := parseInstruction(`HEALTHCHECK --interval=5s CMD curl -f http://localhost/`)
	if err != nil {
		t.Fatalf("parseInstruction: %v", err)
	}
	if instr.Cmd != "HEALTHCHECK" {
		t.Fatalf("cmd = %q", instr.Cmd)
	}
}

// ---- Coverage: hasDockerfileInstructions ----

func TestHasDockerfileInstructions_Empty(t *testing.T) {
	if hasDockerfileInstructions("") {
		t.Fatal("empty should return false")
	}
}

func TestHasDockerfileInstructions_OnlyWhitespace(t *testing.T) {
	if hasDockerfileInstructions("   \n\n  ") {
		t.Fatal("whitespace should return false")
	}
}

func TestHasDockerfileInstructions_OnlyCommentsAndWhitespace(t *testing.T) {
	if hasDockerfileInstructions("# comment\n  \n# another") {
		t.Fatal("comments should return false")
	}
}

func TestHasDockerfileInstructions_WithInstruction(t *testing.T) {
	if !hasDockerfileInstructions("FROM scratch") {
		t.Fatal("should return true for FROM")
	}
}

// ---- Coverage: normalizeAddFromForBuildKit ----

func TestNormalizeAddFromForBuildKit_NoChange(t *testing.T) {
	input := "COPY --from=builder /app /app"
	got := normalizeAddFromForBuildKit(input)
	if got != input {
		t.Fatalf("should not change COPY: %q", got)
	}
}

func TestNormalizeAddFromForBuildKit_ConvertADDFrom(t *testing.T) {
	input := "ADD --from=builder /app /app"
	got := normalizeAddFromForBuildKit(input)
	if !strings.HasPrefix(got, "COPY") {
		t.Fatalf("should convert ADD --from to COPY: %q", got)
	}
}

func TestNormalizeAddFromForBuildKit_RegularADD(t *testing.T) {
	input := "ADD file.tar /app"
	got := normalizeAddFromForBuildKit(input)
	if got != input {
		t.Fatalf("should not change ADD without --from: %q", got)
	}
}

// ---- Coverage: joinUint32s ----

func TestJoinUint32s(t *testing.T) {
	tests := []struct {
		input []uint32
		want  string
	}{
		{nil, ""},
		{[]uint32{}, ""},
		{[]uint32{1}, "1"},
		{[]uint32{1, 2, 3}, "1,2,3"},
		{[]uint32{0, 4294967295}, "0,4294967295"},
	}
	for _, tt := range tests {
		got := joinUint32s(tt.input)
		if got != tt.want {
			t.Errorf("joinUint32s(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- Coverage: envMap ----

func TestEnvMap(t *testing.T) {
	env := []string{"HOME=/root", "PATH=/usr/bin", "EMPTY="}
	m, order := envMap(env)
	if m["HOME"] != "/root" {
		t.Fatalf("HOME = %q", m["HOME"])
	}
	if m["EMPTY"] != "" {
		t.Fatalf("EMPTY = %q", m["EMPTY"])
	}
	if len(order) != 3 {
		t.Fatalf("order len = %d", len(order))
	}
}

func TestEnvMap_Empty(t *testing.T) {
	m, order := envMap(nil)
	if len(m) != 0 || len(order) != 0 {
		t.Fatalf("expected empty, got m=%v order=%v", m, order)
	}
}

// ---- Coverage: resolveDockerfilePath ----

func TestResolveDockerfilePath_Absolute(t *testing.T) {
	got, err := resolveDockerfilePath("/absolute/Dockerfile", "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "/absolute/Dockerfile" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveDockerfilePath_RelativeWithContext(t *testing.T) {
	dir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := resolveDockerfilePath("Dockerfile", dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != df {
		t.Fatalf("got %q, want %q", got, df)
	}
}

func TestResolveDockerfilePath_Empty(t *testing.T) {
	dir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := resolveDockerfilePath("", dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != df {
		t.Fatalf("got %q, want %q", got, df)
	}
}

// ---- Coverage: cloneHealthcheck ----

func TestCloneHealthcheck_Nil(t *testing.T) {
	if got := cloneHealthcheck(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestCloneHealthcheck_WithData(t *testing.T) {
	orig := &oci.Healthcheck{
		Test:     []string{"CMD", "curl"},
		Interval: 30 * time.Second,
		Retries:  3,
	}
	clone := cloneHealthcheck(orig)
	if clone == orig {
		t.Fatal("should be different pointer")
	}
	clone.Test[0] = "CHANGED"
	if orig.Test[0] == "CHANGED" {
		t.Fatal("clone should be independent")
	}
}

// ---- Coverage: ensureDirMode ----

func TestEnsureDirMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subdir")
	if err := ensureDirMode(dir, 0755); err != nil {
		t.Fatalf("ensureDirMode: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

// ---- Coverage: buildStagePlan and markReachableStages ----

func TestBuildStagePlan_SimpleStages(t *testing.T) {
	instrs := []Instruction{
		{Cmd: "ARG", Args: []string{"VERSION=1.0"}},
		{Cmd: "FROM", Args: []string{"ubuntu", "AS", "builder"}},
		{Cmd: "RUN", Args: []string{"echo hi"}},
		{Cmd: "FROM", Args: []string{"scratch"}},
		{Cmd: "COPY", Args: []string{"--from=builder", "/app", "/app"}},
	}
	preamble, stages, err := buildStagePlan(instrs, map[string]string{})
	if err != nil {
		t.Fatalf("buildStagePlan: %v", err)
	}
	if len(preamble) != 1 {
		t.Fatalf("preamble = %d, want 1", len(preamble))
	}
	if len(stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(stages))
	}
	if stages[0].alias != "builder" {
		t.Fatalf("stage 0 alias = %q", stages[0].alias)
	}
}

func TestMarkReachableStages_Empty(t *testing.T) {
	reachable := markReachableStages(nil)
	if len(reachable) != 0 {
		t.Fatalf("expected empty, got %v", reachable)
	}
}

func TestMarkReachableStages_SingleStage(t *testing.T) {
	stages := []plannedStage{{index: 0, fromRef: "ubuntu"}}
	reachable := markReachableStages(stages)
	if !reachable[0] {
		t.Fatal("stage 0 should be reachable")
	}
}

func TestMarkReachableStages_WithDeps(t *testing.T) {
	stages := []plannedStage{
		{index: 0, alias: "builder", fromRef: "ubuntu"},
		{index: 1, alias: "runner", fromRef: "scratch", deps: []string{"builder"}},
	}
	reachable := markReachableStages(stages)
	if !reachable[0] || !reachable[1] {
		t.Fatalf("both stages should be reachable: %v", reachable)
	}
}

// ---- Coverage: applyArgDefaults ----

func TestApplyArgDefaults_ExistingNotOverwritten(t *testing.T) {
	args := map[string]string{"KEY": "existing"}
	applyArgDefaults(args, []string{"KEY=new"})
	if args["KEY"] != "existing" {
		t.Fatalf("KEY = %q, want existing", args["KEY"])
	}
}

func TestApplyArgDefaults_NewKeyAdded(t *testing.T) {
	args := map[string]string{}
	applyArgDefaults(args, []string{"NEW_KEY=value"})
	if args["NEW_KEY"] != "value" {
		t.Fatalf("NEW_KEY = %q, want value", args["NEW_KEY"])
	}
}

func TestApplyArgDefaults_NoValueKey(t *testing.T) {
	args := map[string]string{}
	applyArgDefaults(args, []string{"EMPTY_KEY"})
	if args["EMPTY_KEY"] != "" {
		t.Fatalf("EMPTY_KEY = %q, want empty", args["EMPTY_KEY"])
	}
}
