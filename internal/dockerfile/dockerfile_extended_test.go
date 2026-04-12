package dockerfile

import (
	"strings"
	"testing"
)

// ---------- extractInstructionStageRefs ----------

func TestExtractInstructionStageRefsCOPY(t *testing.T) {
	instr := Instruction{
		Cmd:  "COPY",
		Args: []string{"--from=builder", "/app/bin", "/usr/local/bin/"},
	}
	refs, err := extractInstructionStageRefs(instr, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(refs) != 1 || refs[0] != "builder" {
		t.Fatalf("refs = %v, want [builder]", refs)
	}
}

func TestExtractInstructionStageRefsCOPYNoFrom(t *testing.T) {
	instr := Instruction{
		Cmd:  "COPY",
		Args: []string{"src/", "/app/"},
	}
	refs, err := extractInstructionStageRefs(instr, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty", refs)
	}
}

func TestExtractInstructionStageRefsRUNMount(t *testing.T) {
	instr := Instruction{
		Cmd: "RUN",
		RunMounts: []RunMount{
			{From: "deps", Type: "bind", Source: "/go/pkg", Target: "/go/pkg"},
		},
	}
	refs, err := extractInstructionStageRefs(instr, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(refs) != 1 || refs[0] != "deps" {
		t.Fatalf("refs = %v, want [deps]", refs)
	}
}

func TestExtractInstructionStageRefsRUNMountEmptyFrom(t *testing.T) {
	instr := Instruction{
		Cmd: "RUN",
		RunMounts: []RunMount{
			{Type: "cache", Target: "/root/.cache"},
		},
	}
	refs, err := extractInstructionStageRefs(instr, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty", refs)
	}
}

func TestExtractInstructionStageRefsWithBuildArgs(t *testing.T) {
	instr := Instruction{
		Cmd:  "COPY",
		Args: []string{"--from=$STAGE", "/app", "/app"},
	}
	args := map[string]string{"STAGE": "compiler"}
	refs, err := extractInstructionStageRefs(instr, args)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(refs) != 1 || refs[0] != "compiler" {
		t.Fatalf("refs = %v, want [compiler]", refs)
	}
}

func TestExtractInstructionStageRefsENVNoRefs(t *testing.T) {
	instr := Instruction{Cmd: "ENV", Args: []string{"FOO=bar"}}
	refs, err := extractInstructionStageRefs(instr, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty", refs)
	}
}

// ---------- applyArgDefaults ----------

func TestApplyArgDefaultsKeyOnly(t *testing.T) {
	args := map[string]string{}
	applyArgDefaults(args, []string{"MY_ARG"})
	if v, ok := args["MY_ARG"]; !ok || v != "" {
		t.Fatalf("MY_ARG = %q, exists=%v, want empty string", v, ok)
	}
}

func TestApplyArgDefaultsKeyValue(t *testing.T) {
	args := map[string]string{}
	applyArgDefaults(args, []string{"VERSION=1.0"})
	if args["VERSION"] != "1.0" {
		t.Fatalf("VERSION = %q, want 1.0", args["VERSION"])
	}
}

func TestApplyArgDefaultsDoesNotOverrideExisting(t *testing.T) {
	args := map[string]string{"VERSION": "2.0"}
	applyArgDefaults(args, []string{"VERSION=1.0"})
	if args["VERSION"] != "2.0" {
		t.Fatalf("VERSION = %q, want 2.0 (should not override)", args["VERSION"])
	}
}

func TestApplyArgDefaultsWithExpansion(t *testing.T) {
	args := map[string]string{"BASE": "ubuntu"}
	applyArgDefaults(args, []string{"IMAGE=${BASE}:latest"})
	if args["IMAGE"] != "ubuntu:latest" {
		t.Fatalf("IMAGE = %q, want ubuntu:latest", args["IMAGE"])
	}
}

// ---------- expandBuildArgValue ----------

func TestExpandBuildArgValueSimple(t *testing.T) {
	args := map[string]string{"VERSION": "3.14"}
	result, err := expandBuildArgValue(args, "v${VERSION}")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result != "v3.14" {
		t.Fatalf("result = %q, want v3.14", result)
	}
}

func TestExpandBuildArgValueNoVars(t *testing.T) {
	result, err := expandBuildArgValue(nil, "literal")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result != "literal" {
		t.Fatalf("result = %q, want literal", result)
	}
}

func TestExpandBuildArgValueUnresolved(t *testing.T) {
	_, err := expandBuildArgValue(nil, "$MISSING")
	if err == nil {
		t.Fatal("expected error for unresolved variable")
	}
}

// ---------- splitArgsWithForm fallback paths ----------

func TestSplitArgsWithFormFallbackRUN(t *testing.T) {
	// RUN with unparseable content should fall back to shell form
	args, shellForm := splitArgsWithForm("RUN", "echo hello && echo world")
	if len(args) == 0 {
		t.Fatal("expected non-empty args")
	}
	if !shellForm {
		t.Fatal("expected shell form for RUN with shell commands")
	}
}

func TestSplitArgsWithFormCMDFallback(t *testing.T) {
	args, shellForm := splitArgsWithForm("CMD", "echo test")
	if len(args) == 0 {
		t.Fatal("expected non-empty args")
	}
	_ = shellForm
}

// ---------- parse with various Dockerfile content ----------

func TestParseMultiStageDockerfile(t *testing.T) {
	df := `FROM golang:1.21 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /app/bin ./cmd/server

FROM alpine:3.18
COPY --from=builder /app/bin /usr/local/bin/server
CMD ["/usr/local/bin/server"]
`
	instrs, err := parse(strings.NewReader(df))
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
		t.Fatalf("FROM count = %d, want 2", fromCount)
	}

	// Find COPY --from=builder
	found := false
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if strings.HasPrefix(arg, "--from=") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("expected COPY --from=builder instruction")
	}
}

func TestParseDockerfileWithARG(t *testing.T) {
	df := `ARG BASE_IMAGE=ubuntu:22.04
FROM ${BASE_IMAGE}
RUN apt-get update
`
	instrs, err := parse(strings.NewReader(df))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) < 2 {
		t.Fatalf("expected at least 2 instructions, got %d", len(instrs))
	}
	if instrs[0].Cmd != "ARG" {
		t.Fatalf("first instruction = %q, want ARG", instrs[0].Cmd)
	}
}

func TestParseDockerfileWithHealthcheck(t *testing.T) {
	df := `FROM alpine
HEALTHCHECK --interval=30s --timeout=5s CMD curl -f http://localhost/ || exit 1
`
	instrs, err := parse(strings.NewReader(df))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, instr := range instrs {
		if instr.Cmd == "HEALTHCHECK" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected HEALTHCHECK instruction")
	}
}

func TestParseEmptyDockerfile(t *testing.T) {
	instrs, err := parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 0 {
		t.Fatalf("expected 0 instructions for empty content, got %d", len(instrs))
	}
}

func TestParseCommentOnlyDockerfile(t *testing.T) {
	instrs, err := parse(strings.NewReader("# just a comment\n# another comment\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(instrs) != 0 {
		t.Fatalf("expected 0 instructions for comment-only content, got %d", len(instrs))
	}
}

func TestParseDockerfileWithADDFromNormalization(t *testing.T) {
	df := `FROM alpine AS base
RUN echo hello

FROM alpine
ADD --from=base /etc/hosts /tmp/hosts
`
	instrs, err := parse(strings.NewReader(df))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// ADD --from= should be normalized to COPY --from=
	found := false
	for _, instr := range instrs {
		if instr.Cmd == "COPY" {
			for _, arg := range instr.Args {
				if strings.HasPrefix(arg, "--from=") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("ADD --from= should be normalized to COPY --from=")
	}
}

func TestNormalizeAddFromForBuildKitTabIndent(t *testing.T) {
	got := normalizeAddFromForBuildKit("\tADD --from=x /a /b")
	want := "\tCOPY --from=x /a /b"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------- parseInstruction ----------

func TestParseInstructionEmpty(t *testing.T) {
	_, err := parseInstruction("")
	if err == nil {
		t.Fatal("expected error for empty instruction")
	}
}

func TestParseInstructionSingleKeyword(t *testing.T) {
	instr, err := parseInstruction("FROM")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "FROM" {
		t.Fatalf("Cmd = %q, want FROM", instr.Cmd)
	}
}

func TestParseInstructionENV(t *testing.T) {
	instr, err := parseInstruction("ENV MY_VAR=hello")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "ENV" {
		t.Fatalf("Cmd = %q, want ENV", instr.Cmd)
	}
	if len(instr.Args) == 0 {
		t.Fatal("expected non-empty args")
	}
}

// ---------- parseRemoteURL ----------

func TestParseRemoteURLValid(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"https://example.com/file.tar.gz", true},
		{"http://example.com/path", true},
		{"ftp://example.com/file", false},
		{"not-a-url", false},
		{"https://", false},
	}
	for _, tt := range tests {
		_, ok := parseRemoteURL(tt.input)
		if ok != tt.valid {
			t.Errorf("parseRemoteURL(%q) = _, %v, want %v", tt.input, ok, tt.valid)
		}
	}
}

// ---------- parseChecksumSpec ----------

func TestParseChecksumSpecValid(t *testing.T) {
	digest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	spec, err := parseChecksumSpec("sha256:" + digest)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if spec.algorithm != "sha256" || spec.expected != digest {
		t.Fatalf("unexpected spec: %+v", spec)
	}
}

func TestParseChecksumSpecInvalid(t *testing.T) {
	tests := []string{
		"nocolon",
		"md5:abc123",
		"sha256:tooshort",
		"sha256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	}
	for _, tt := range tests {
		_, err := parseChecksumSpec(tt)
		if err == nil {
			t.Errorf("parseChecksumSpec(%q) = nil error, want error", tt)
		}
	}
}

// ---------- normalizeMatcherPath ----------

func TestNormalizeMatcherPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"./src/main.go", "src/main.go"},
		{"/absolute/path", "absolute/path"},
		{"", "."},
		{"simple", "simple"},
		{"./", "."},
		{"a/../b", "b"},
	}
	for _, tt := range tests {
		got := normalizeMatcherPath(tt.input)
		if got != tt.want {
			t.Errorf("normalizeMatcherPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---------- parseCopyMode ----------

func TestParseCopyModeValid(t *testing.T) {
	mode, err := parseCopyMode("755")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if *mode != 0755 {
		t.Fatalf("mode = %o, want 755", *mode)
	}
}

func TestParseCopyModeInvalid(t *testing.T) {
	_, err := parseCopyMode("not-octal")
	if err == nil {
		t.Fatal("expected error for non-octal value")
	}
}

// ---------- hasWildcards ----------

func TestHasWildcards(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"*.go", true},
		{"file?.txt", true},
		{"dir/[abc]", true},
		{"normal/path", false},
		{"", false},
	}
	for _, tt := range tests {
		got := hasWildcards(tt.input)
		if got != tt.want {
			t.Errorf("hasWildcards(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// ---------- matchTransferSegments ----------

func TestMatchTransferSegments(t *testing.T) {
	tests := []struct {
		pattern []string
		rel     []string
		match   bool
	}{
		{nil, nil, true},
		{[]string{"*.go"}, []string{"main.go"}, true},
		{[]string{"*.go"}, []string{"main.txt"}, false},
		{[]string{"src", "*.go"}, []string{"src", "app.go"}, true},
		{[]string{"src", "*.go"}, []string{"lib", "app.go"}, false},
	}
	for _, tt := range tests {
		got, err := matchTransferSegments(tt.pattern, tt.rel)
		if err != nil {
			t.Fatalf("matchTransferSegments(%v, %v): %v", tt.pattern, tt.rel, err)
		}
		if got != tt.match {
			t.Errorf("matchTransferSegments(%v, %v) = %v, want %v", tt.pattern, tt.rel, got, tt.match)
		}
	}
}

func TestParseInstructionWORKDIR(t *testing.T) {
	instr, err := parseInstruction("WORKDIR /app")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if instr.Cmd != "WORKDIR" || len(instr.Args) != 1 || instr.Args[0] != "/app" {
		t.Fatalf("unexpected: %+v", instr)
	}
}
