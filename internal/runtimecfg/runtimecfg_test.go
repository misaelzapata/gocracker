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
