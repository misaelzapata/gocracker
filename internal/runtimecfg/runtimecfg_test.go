package runtimecfg

import (
	"reflect"
	"strings"
	"testing"
)

func TestDefaultKernelArgs_WithSerialConsole(t *testing.T) {
	args := DefaultKernelArgs(true)
	if args[0] != "console=ttyS0" {
		t.Fatalf("first arg = %q, want console=ttyS0", args[0])
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"reboot=k",
		"panic=1",
		"nomodule",
		"i8042.noaux",
		"i8042.nomux",
		"i8042.dumbkbd",
		"swiotlb=noforce",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, SerialDisable8250) {
		t.Fatalf("serial console args should not disable 8250: %q", joined)
	}
}

func TestDefaultKernelArgs_WithoutSerialConsole(t *testing.T) {
	args := DefaultKernelArgs(false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, SerialDisable8250) {
		t.Fatalf("missing %q in %q", SerialDisable8250, joined)
	}
	if strings.Contains(joined, "console=ttyS0") {
		t.Fatalf("unexpected console in %q", joined)
	}
}

func TestResolveProcess(t *testing.T) {
	tests := []struct {
		name       string
		entrypoint []string
		cmd        []string
		want       Process
	}{
		{
			name: "cmd only",
			cmd:  []string{"/bin/sh", "-l"},
			want: Process{Exec: "/bin/sh", Args: []string{"-l"}},
		},
		{
			name:       "entrypoint only",
			entrypoint: []string{"/app"},
			want:       Process{Exec: "/app"},
		},
		{
			name:       "entrypoint plus cmd",
			entrypoint: []string{"/docker-entrypoint.sh", "-c"},
			cmd:        []string{"nginx", "-g", "daemon off;"},
			want: Process{
				Exec: "/docker-entrypoint.sh",
				Args: []string{"-c", "nginx", "-g", "daemon off;"},
			},
		},
		{
			name: "empty",
			want: Process{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveProcess(tt.entrypoint, tt.cmd); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ResolveProcess() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGuestSpecKernelArgsRoundTrip(t *testing.T) {
	spec := GuestSpec{
		Process: Process{
			Exec: "/usr/bin/python3",
			Args: []string{"-c", `print("hello world")`},
		},
		Env: []string{
			`MSG=hello world`,
			`JSON={"ok":true}`,
		},
		Hosts: []string{
			"db=172.20.0.2",
			"cache=172.20.0.3",
		},
		SharedFS: []SharedFSMount{
			{Tag: "gocracker-fs-0", Target: "/data", ReadOnly: false},
			{Tag: "assets", Target: "/srv/assets", ReadOnly: true},
		},
		WorkDir: "/srv/app data",
		User:    "appuser:appgroup",
	}

	parts := spec.AppendKernelArgs([]string{"console=ttyS0"})
	fields := ParseKernelCmdline(strings.Join(parts, " "))
	got, ok, err := DecodeGuestSpec(fields)
	if err != nil {
		t.Fatalf("DecodeGuestSpec() error = %v", err)
	}
	if !ok {
		t.Fatal("DecodeGuestSpec() ok = false, want true")
	}
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("DecodeGuestSpec() = %#v, want %#v", got, spec)
	}
}

func TestGuestSpecKernelArgsSkippedWhenEmpty(t *testing.T) {
	parts := GuestSpec{}.AppendKernelArgs([]string{"console=ttyS0"})
	if len(parts) != 1 || parts[0] != "console=ttyS0" {
		t.Fatalf("parts = %#v, want only console", parts)
	}
}

func TestGuestSpecJSONRoundTrip(t *testing.T) {
	spec := GuestSpec{
		Process: Process{
			Exec: "/bin/server",
			Args: []string{"--port", "8080"},
		},
		Env:   []string{"PATH=/usr/bin", "DEBUG=1"},
		Hosts: []string{"db=172.20.0.2"},
		SharedFS: []SharedFSMount{
			{Tag: "gocracker-fs-0", Target: "/data", ReadOnly: false},
		},
		WorkDir:  "/srv/app",
		User:     "1000:1000",
		PID1Mode: PID1ModeSupervised,
	}

	data, err := spec.MarshalJSONBytes()
	if err != nil {
		t.Fatalf("MarshalJSONBytes() error = %v", err)
	}
	got, err := UnmarshalGuestSpecJSON(data)
	if err != nil {
		t.Fatalf("UnmarshalGuestSpecJSON() error = %v", err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("guest spec round-trip = %#v, want %#v", got, spec)
	}
}

func TestParseKernelCmdlinePreservesEscapes(t *testing.T) {
	fields := ParseKernelCmdline(`foo=bar gc.env.MSG=hello\ world gc.env.JSON=\x20`)
	if got := fields["foo"]; got != "bar" {
		t.Fatalf("foo = %q, want bar", got)
	}
	if got := fields["gc.env.MSG"]; got != "hello world" {
		t.Fatalf("gc.env.MSG = %q, want %q", got, "hello world")
	}
	if got := fields["gc.env.JSON"]; got != `\x20` {
		t.Fatalf(`gc.env.JSON = %q, want %q`, got, `\x20`)
	}
}

func TestLegacyEnvDecodesEscapedSpaces(t *testing.T) {
	fields := map[string]string{
		"gc.env.MSG":  `hello\x20world`,
		"gc.env.PATH": "/usr/bin",
	}
	got := LegacyEnv(fields)
	want := []string{"MSG=hello world", "PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LegacyEnv() = %#v, want %#v", got, want)
	}
}

func TestLegacyUserDecodesEscapedSpaces(t *testing.T) {
	fields := map[string]string{
		UserKey: `app\x20user`,
	}
	if got := LegacyUser(fields); got != "app user" {
		t.Fatalf("LegacyUser() = %q, want %q", got, "app user")
	}
}

func TestSplitCommandLine(t *testing.T) {
	got, err := SplitCommandLine(`python3 -c "print(123)" --name 'hello world'`)
	if err != nil {
		t.Fatalf("SplitCommandLine() error = %v", err)
	}
	want := []string{"python3", "-c", "print(123)", "--name", "hello world"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitCommandLine() = %#v, want %#v", got, want)
	}
}

func TestDefaultKernelCmdline(t *testing.T) {
	tests := []struct {
		name          string
		serialConsole bool
		wantContains  []string
		wantAbsent    []string
	}{
		{
			name:          "with serial console",
			serialConsole: true,
			wantContains:  []string{"console=ttyS0", "reboot=k", "panic=1", PCIDisable},
			wantAbsent:    []string{SerialDisable8250},
		},
		{
			name:          "without serial console",
			serialConsole: false,
			wantContains:  []string{SerialDisable8250, PCIDisable, "reboot=k"},
			wantAbsent:    []string{"console=ttyS0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmdline := DefaultKernelCmdline(tt.serialConsole)
			for _, want := range tt.wantContains {
				if !strings.Contains(cmdline, want) {
					t.Errorf("DefaultKernelCmdline(%v) missing %q in %q", tt.serialConsole, want, cmdline)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(cmdline, absent) {
					t.Errorf("DefaultKernelCmdline(%v) unexpected %q in %q", tt.serialConsole, absent, cmdline)
				}
			}
		})
	}
}

func TestDefaultKernelArgsForRuntime_AllowKernelModules(t *testing.T) {
	args := DefaultKernelArgsForRuntime(true, true)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "nomodule") {
		t.Fatalf("allowKernelModules=true should omit nomodule, got %q", joined)
	}
	if !strings.Contains(joined, "console=ttyS0") {
		t.Fatalf("withSerialConsole=true missing console=ttyS0 in %q", joined)
	}

	args2 := DefaultKernelArgsForRuntime(false, false)
	joined2 := strings.Join(args2, " ")
	if !strings.Contains(joined2, "nomodule") {
		t.Fatalf("allowKernelModules=false should include nomodule, got %q", joined2)
	}
}

func TestGuestSpecJSONRoundTrip_WithExecConfig(t *testing.T) {
	spec := GuestSpec{
		Process: Process{
			Exec: "/bin/app",
			Args: []string{"--verbose"},
		},
		Env:      []string{"KEY=value"},
		WorkDir:  "/app",
		User:     "nobody",
		PID1Mode: PID1ModeHandoff,
	}
	data, err := spec.MarshalJSONBytes()
	if err != nil {
		t.Fatalf("MarshalJSONBytes() error = %v", err)
	}
	got, err := UnmarshalGuestSpecJSON(data)
	if err != nil {
		t.Fatalf("UnmarshalGuestSpecJSON() error = %v", err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("round-trip mismatch: got %#v, want %#v", got, spec)
	}
}

func TestUnmarshalGuestSpecJSON_InvalidJSON(t *testing.T) {
	_, err := UnmarshalGuestSpecJSON([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestEncodeDecodeValue(t *testing.T) {
	tests := []struct {
		input string
	}{
		{""},
		{"hello"},
		{"/usr/local/bin/app"},
		{"value with spaces and special=chars"},
		{`{"json":"value"}`},
	}
	for _, tt := range tests {
		encoded := EncodeValue(tt.input)
		decoded, err := DecodeValue(encoded)
		if err != nil {
			t.Fatalf("DecodeValue(%q) error = %v", encoded, err)
		}
		if decoded != tt.input {
			t.Fatalf("EncodeValue/DecodeValue(%q) = %q", tt.input, decoded)
		}
	}
}

func TestDecodeValue_InvalidBase64(t *testing.T) {
	_, err := DecodeValue("!!!invalid!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestProcessIsZero(t *testing.T) {
	tests := []struct {
		name string
		proc Process
		want bool
	}{
		{"zero value", Process{}, true},
		{"has exec", Process{Exec: "/bin/sh"}, false},
		{"has args only", Process{Args: []string{"-l"}}, false},
		{"has both", Process{Exec: "/bin/sh", Args: []string{"-l"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.proc.IsZero(); got != tt.want {
				t.Fatalf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGuestSpecHasStructuredFields(t *testing.T) {
	tests := []struct {
		name string
		spec GuestSpec
		want bool
	}{
		{"empty", GuestSpec{}, false},
		{"has process", GuestSpec{Process: Process{Exec: "/app"}}, true},
		{"has env", GuestSpec{Env: []string{"FOO=bar"}}, true},
		{"has workdir", GuestSpec{WorkDir: "/app"}, true},
		{"has user", GuestSpec{User: "root"}, true},
		{"has pid1 mode", GuestSpec{PID1Mode: PID1ModeHandoff}, true},
		{"has hosts", GuestSpec{Hosts: []string{"db=1.2.3.4"}}, true},
		{"has shared fs", GuestSpec{SharedFS: []SharedFSMount{{Tag: "t", Target: "/m"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.HasStructuredFields(); got != tt.want {
				t.Fatalf("HasStructuredFields() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitKernelFields(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"foo", []string{"foo"}},
		{"foo bar baz", []string{"foo", "bar", "baz"}},
		{`foo\ bar baz`, []string{"foo bar", "baz"}},
		{"  spaces  everywhere  ", []string{"spaces", "everywhere"}},
		{`a\\b`, []string{`a\b`}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SplitKernelFields(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SplitKernelFields(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSplitCommandLine_ErrorCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"unterminated double quote", `echo "hello`},
		{"unterminated single quote", `echo 'hello`},
		{"unterminated escape", `echo hello\`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SplitCommandLine(tt.input)
			if err == nil {
				t.Fatalf("SplitCommandLine(%q) succeeded, want error", tt.input)
			}
		})
	}
}

func TestSplitCommandLine_VariousCases(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{``, nil},
		{`simple`, []string{"simple"}},
		{`a "b c" d`, []string{"a", "b c", "d"}},
		{`a 'b c' d`, []string{"a", "b c", "d"}},
		{`a \"b c`, []string{"a", `"b`, "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := SplitCommandLine(tt.input)
			if err != nil {
				t.Fatalf("SplitCommandLine(%q) error = %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SplitCommandLine(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseKernelCmdline(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]string
	}{
		{
			input: "console=ttyS0 pci=off nomodule",
			want: map[string]string{
				"console":  "ttyS0",
				"pci":      "off",
				"nomodule": "1",
			},
		},
		{
			input: "",
			want:  map[string]string{},
		},
		{
			input: "key=val=ue",
			want:  map[string]string{"key": "val=ue"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseKernelCmdline(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseKernelCmdline(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecodeGuestSpec_NoFormat(t *testing.T) {
	fields := map[string]string{
		"console": "ttyS0",
	}
	_, ok, err := DecodeGuestSpec(fields)
	if err != nil {
		t.Fatalf("DecodeGuestSpec() error = %v", err)
	}
	if ok {
		t.Fatal("DecodeGuestSpec() ok = true, want false (no format key)")
	}
}

func TestDecodeGuestSpec_WrongFormat(t *testing.T) {
	fields := map[string]string{
		FormatKey: "99",
	}
	_, ok, err := DecodeGuestSpec(fields)
	if err != nil {
		t.Fatalf("DecodeGuestSpec() error = %v", err)
	}
	if ok {
		t.Fatal("DecodeGuestSpec() ok = true, want false (wrong format version)")
	}
}

func TestLegacyProcess(t *testing.T) {
	fields := map[string]string{
		"gc.entrypoint": "/docker-entrypoint.sh",
		"gc.cmd":        "nginx -g daemon\\ off;",
	}
	proc := LegacyProcess(fields)
	if proc.Exec != "/docker-entrypoint.sh" {
		t.Fatalf("Exec = %q, want /docker-entrypoint.sh", proc.Exec)
	}
	if len(proc.Args) != 3 {
		t.Fatalf("Args = %#v, want 3 args", proc.Args)
	}
}

func TestLegacyWorkDir(t *testing.T) {
	fields := map[string]string{
		WorkDirKey: `/app\x20data`,
	}
	got := LegacyWorkDir(fields)
	if got != "/app data" {
		t.Fatalf("LegacyWorkDir() = %q, want %q", got, "/app data")
	}
}

func TestSharedFSMountKernelRoundTrip(t *testing.T) {
	spec := GuestSpec{
		Process: Process{Exec: "/bin/sh"},
		SharedFS: []SharedFSMount{
			{Tag: "myfs", Target: "/mnt/data", ReadOnly: true},
			{Tag: "logs", Target: "/var/log", ReadOnly: false},
		},
	}
	parts := spec.AppendKernelArgs(nil)
	fields := ParseKernelCmdline(strings.Join(parts, " "))
	got, ok, err := DecodeGuestSpec(fields)
	if err != nil {
		t.Fatalf("DecodeGuestSpec() error = %v", err)
	}
	if !ok {
		t.Fatal("DecodeGuestSpec() ok = false")
	}
	if !reflect.DeepEqual(got.SharedFS, spec.SharedFS) {
		t.Fatalf("SharedFS round-trip: got %#v, want %#v", got.SharedFS, spec.SharedFS)
	}
}

func TestHasNumericSuffix(t *testing.T) {
	tests := []struct {
		value  string
		prefix string
		want   bool
	}{
		{"gc.env.0", "gc.env.", true},
		{"gc.env.123", "gc.env.", true},
		{"gc.env.PATH", "gc.env.", false},
		{"gc.env.MSG", "gc.env.", false},
		{"unrelated", "gc.env.", false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got := hasNumericSuffix(tt.value, tt.prefix)
			if got != tt.want {
				t.Fatalf("hasNumericSuffix(%q, %q) = %v, want %v", tt.value, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestLegacyEnvSkipsNumericKeys(t *testing.T) {
	fields := map[string]string{
		"gc.env.0":    EncodeValue("PATH=/usr/bin"),
		"gc.env.1":    EncodeValue("HOME=/root"),
		"gc.env.MSG":  "hello",
		"gc.env.PATH": "/usr/bin",
	}
	got := LegacyEnv(fields)
	// Should only include MSG and PATH (non-numeric suffixes)
	if len(got) != 2 {
		t.Fatalf("LegacyEnv() = %#v, want 2 entries (MSG and PATH)", got)
	}
}
