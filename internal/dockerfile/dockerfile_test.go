package dockerfile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
