//go:build linux

package jailer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidateRejectsRelativeExec(t *testing.T) {
	cfg := Config{
		ID:       "vm-1",
		ExecFile: "gocracker-vmm",
		UID:      123,
		GID:      456,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("expected absolute exec-file validation error, got %v", err)
	}
}

func TestConfigValidateRejectsInvalidID(t *testing.T) {
	cfg := Config{
		ID:       "bad/id",
		ExecFile: filepath.Join(t.TempDir(), "gocracker-vmm"),
		UID:      123,
		GID:      456,
	}
	if err := osWriteFile(cfg.ExecFile); err != nil {
		t.Fatalf("write exec file: %v", err)
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "unsupported character") {
		t.Fatalf("expected invalid id error, got %v", err)
	}
}

func TestConfigChrootDir(t *testing.T) {
	cfg := Config{
		ID:            "vm-123",
		ExecFile:      "/usr/local/bin/gocracker-vmm",
		ChrootBaseDir: "/srv/jailer",
	}
	got := cfg.chrootDir()
	want := "/srv/jailer/gocracker-vmm/vm-123/root"
	if got != want {
		t.Fatalf("chrootDir() = %q, want %q", got, want)
	}
}

func TestMkdirAllNoSymlinkRejectsSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := mkdirAllNoSymlink(filepath.Join(root, "link", "child"), 0755); err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("mkdirAllNoSymlink() error = %v, want symlink component error", err)
	}
}

func TestCopyRegularFileRejectsSymlinkDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.bin")
	if err := osWriteFile(src); err != nil {
		t.Fatalf("write src: %v", err)
	}
	realDst := filepath.Join(root, "real.bin")
	if err := osWriteFile(realDst); err != nil {
		t.Fatalf("write real dst: %v", err)
	}
	dst := filepath.Join(root, "dst.bin")
	if err := os.Symlink(realDst, dst); err != nil {
		t.Fatalf("symlink dst: %v", err)
	}
	if err := copyRegularFile(src, dst, 0755); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("copyRegularFile() error = %v, want symlink rejection", err)
	}
}

func osWriteFile(path string) error { return os.WriteFile(path, []byte("stub"), 0755) }

func TestConfigValidateRejectsEmptyID(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       "",
		ExecFile: execFile,
		UID:      1000,
		GID:      1000,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "--id is required") {
		t.Fatalf("expected --id required error, got %v", err)
	}
}

func TestConfigValidateRejectsLongID(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       strings.Repeat("a", maxVMIDLen+1),
		ExecFile: execFile,
		UID:      1000,
		GID:      1000,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected exceeds length error, got %v", err)
	}
}

func TestConfigValidateRejectsEmptyExecFile(t *testing.T) {
	cfg := Config{
		ID:       "test-vm",
		ExecFile: "",
		UID:      1000,
		GID:      1000,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "--exec-file is required") {
		t.Fatalf("expected exec-file required error, got %v", err)
	}
}

func TestConfigValidateRejectsMissingUID(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       "test-vm",
		ExecFile: execFile,
		UID:      -1,
		GID:      1000,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "--uid is required") {
		t.Fatalf("expected uid required error, got %v", err)
	}
}

func TestConfigValidateRejectsMissingGID(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       "test-vm",
		ExecFile: execFile,
		UID:      1000,
		GID:      -1,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "--gid is required") {
		t.Fatalf("expected gid required error, got %v", err)
	}
}

func TestConfigValidateRejectsInvalidCgroupVersion(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:            "test-vm",
		ExecFile:      execFile,
		UID:           1000,
		GID:           1000,
		CgroupVersion: 1,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "only cgroup v2") {
		t.Fatalf("expected cgroup version error, got %v", err)
	}
}

func TestConfigValidateAcceptsCgroupV2(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:            "test-vm",
		ExecFile:      execFile,
		UID:           1000,
		GID:           1000,
		CgroupVersion: 2,
	}
	err := cfg.validate()
	if err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}

func TestConfigValidateRejectsIDWithSpecialChars(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		id   string
	}{
		{"slash", "bad/id"},
		{"space", "bad id"},
		{"dot", "bad.id"},
		{"underscore", "bad_id"},
		{"at", "bad@id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				ID:       tt.id,
				ExecFile: execFile,
				UID:      1000,
				GID:      1000,
			}
			err := cfg.validate()
			if err == nil {
				t.Fatalf("validate() with id %q succeeded, want error", tt.id)
			}
		})
	}
}

func TestConfigValidateAcceptsValidIDs(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	tests := []string{
		"vm-1",
		"a",
		"myVM",
		"test-vm-ABC-123",
		strings.Repeat("x", maxVMIDLen),
	}
	for _, id := range tests {
		t.Run(id, func(t *testing.T) {
			cfg := Config{
				ID:       id,
				ExecFile: execFile,
				UID:      1000,
				GID:      1000,
			}
			if err := cfg.validate(); err != nil {
				t.Fatalf("validate() with id %q = %v, want nil", id, err)
			}
		})
	}
}

func TestConfigChrootDirVariants(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		want     string
	}{
		{
			name: "custom base dir",
			cfg: Config{
				ID:            "vm-1",
				ExecFile:      "/opt/bin/myapp",
				ChrootBaseDir: "/jail",
			},
			want: "/jail/myapp/vm-1/root",
		},
		{
			name: "default base dir",
			cfg: Config{
				ID:       "vm-2",
				ExecFile: "/usr/bin/gocracker",
			},
			want: "/srv/jailer/gocracker/vm-2/root",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.chrootDir()
			if got != tt.want {
				t.Fatalf("chrootDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseMount(t *testing.T) {
	tests := []struct {
		name     string
		entry    string
		wantRO   bool
		wantSrc  string
		wantDst  string
		wantErr  bool
	}{
		{
			name:    "read-only mount",
			entry:   "ro:/usr/lib:/usr/lib",
			wantRO:  true,
			wantSrc: "/usr/lib",
			wantDst: "/usr/lib",
		},
		{
			name:    "read-write mount",
			entry:   "rw:/data:/data",
			wantRO:  false,
			wantSrc: "/data",
			wantDst: "/data",
		},
		{
			name:    "uppercase mode normalizes",
			entry:   "RO:/lib:/lib",
			wantRO:  true,
			wantSrc: "/lib",
			wantDst: "/lib",
		},
		{
			name:    "invalid mode",
			entry:   "rx:/lib:/lib",
			wantErr: true,
		},
		{
			name:    "too few parts",
			entry:   "ro:/lib",
			wantErr: true,
		},
		{
			name:    "relative source",
			entry:   "ro:lib:/lib",
			wantErr: true,
		},
		{
			name:    "relative target",
			entry:   "ro:/lib:lib",
			wantErr: true,
		},
		{
			name:    "root target",
			entry:   "ro:/lib:/",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := parseMount(tt.entry)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMount(%q) error = %v, wantErr %v", tt.entry, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if spec.readOnly != tt.wantRO {
				t.Fatalf("readOnly = %v, want %v", spec.readOnly, tt.wantRO)
			}
			if spec.source != tt.wantSrc {
				t.Fatalf("source = %q, want %q", spec.source, tt.wantSrc)
			}
			if spec.target != tt.wantDst {
				t.Fatalf("target = %q, want %q", spec.target, tt.wantDst)
			}
		})
	}
}

func TestConfigValidateRejectsInvalidMounts(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       "test-vm",
		ExecFile: execFile,
		UID:      1000,
		GID:      1000,
		Mounts:   []string{"bad-mount-format"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate() with bad mount succeeded, want error")
	}
}

func TestConfigValidateRejectsInvalidEnv(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       "test-vm",
		ExecFile: execFile,
		UID:      1000,
		GID:      1000,
		Env:      []string{"NO_EQUALS_SIGN"},
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "invalid --env") {
		t.Fatalf("expected invalid env error, got %v", err)
	}
}

func TestExecEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     []string
		hasPath bool
	}{
		{
			name:    "empty env adds PATH",
			env:     nil,
			hasPath: true,
		},
		{
			name:    "custom env without PATH gets PATH added",
			env:     []string{"FOO=bar"},
			hasPath: true,
		},
		{
			name:    "custom env with PATH keeps it",
			env:     []string{"PATH=/custom/bin"},
			hasPath: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Env: tt.env}
			result := cfg.execEnv()
			hasPath := false
			for _, entry := range result {
				if strings.HasPrefix(entry, "PATH=") {
					hasPath = true
				}
			}
			if hasPath != tt.hasPath {
				t.Fatalf("execEnv() hasPath = %v, want %v; result = %v", hasPath, tt.hasPath, result)
			}
		})
	}
}

func TestAppendUniqueStrings(t *testing.T) {
	tests := []struct {
		name   string
		dst    []string
		values []string
		want   []string
	}{
		{
			name:   "no duplicates",
			dst:    []string{"a", "b"},
			values: []string{"c", "d"},
			want:   []string{"a", "b", "c", "d"},
		},
		{
			name:   "with duplicates",
			dst:    []string{"a", "b"},
			values: []string{"b", "c", "a"},
			want:   []string{"a", "b", "c"},
		},
		{
			name:   "empty dst",
			dst:    nil,
			values: []string{"a", "b"},
			want:   []string{"a", "b"},
		},
		{
			name:   "empty values",
			dst:    []string{"a"},
			values: nil,
			want:   []string{"a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendUniqueStrings(tt.dst, tt.values...)
			if len(got) != len(tt.want) {
				t.Fatalf("appendUniqueStrings() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("appendUniqueStrings() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestMkdirAllNoSymlinkCreatesNestedDirs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "c")
	if err := mkdirAllNoSymlink(path, 0755); err != nil {
		t.Fatalf("mkdirAllNoSymlink() = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestMkdirAllNoSymlinkEmptyAndDot(t *testing.T) {
	// Empty and "." should not fail
	if err := mkdirAllNoSymlink("", 0755); err != nil {
		t.Fatalf("mkdirAllNoSymlink('') = %v", err)
	}
	if err := mkdirAllNoSymlink(".", 0755); err != nil {
		t.Fatalf("mkdirAllNoSymlink('.') = %v", err)
	}
}

func TestMultiFlag(t *testing.T) {
	var f multiFlag
	if s := f.String(); s != "" {
		t.Fatalf("empty multiFlag.String() = %q, want empty", s)
	}
	_ = f.Set("a")
	_ = f.Set("b")
	if s := f.String(); s != "a,b" {
		t.Fatalf("multiFlag.String() = %q, want %q", s, "a,b")
	}
}

// --- Coverage-boosting tests ---

func TestMultiFlag_SetReturnsNil(t *testing.T) {
	var f multiFlag
	if err := f.Set("value"); err != nil {
		t.Fatalf("Set() = %v, want nil", err)
	}
	if len(f) != 1 || f[0] != "value" {
		t.Fatalf("after Set: %v, want [value]", f)
	}
}

func TestMultiFlag_Multiple(t *testing.T) {
	var f multiFlag
	_ = f.Set("a")
	_ = f.Set("b")
	_ = f.Set("c")
	if len(f) != 3 {
		t.Fatalf("len = %d, want 3", len(f))
	}
	if f.String() != "a,b,c" {
		t.Fatalf("String() = %q", f.String())
	}
}

func TestConfigValidateRejectsExecFileDir(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		ID:       "test-vm",
		ExecFile: dir,
		UID:      1000,
		GID:      1000,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "must be a file") {
		t.Fatalf("expected 'must be a file' error, got %v", err)
	}
}

func TestConfigValidateRejectsNonexistentExecFile(t *testing.T) {
	cfg := Config{
		ID:       "test-vm",
		ExecFile: "/nonexistent/path/to/binary",
		UID:      1000,
		GID:      1000,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "stat exec-file") {
		t.Fatalf("expected stat error, got %v", err)
	}
}

func TestConfigValidateAcceptsZeroCgroupVersion(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:            "test-vm",
		ExecFile:      execFile,
		UID:           1000,
		GID:           1000,
		CgroupVersion: 0,
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for CgroupVersion=0", err)
	}
}

func TestConfigValidateAcceptsValidMounts(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       "test-vm",
		ExecFile: execFile,
		UID:      1000,
		GID:      1000,
		Mounts:   []string{"ro:/usr/lib:/usr/lib", "rw:/data:/data"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}

func TestConfigValidateAcceptsValidEnv(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ID:       "test-vm",
		ExecFile: execFile,
		UID:      1000,
		GID:      1000,
		Env:      []string{"FOO=bar", "PATH=/usr/bin"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}

func TestParseMountExtended(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantRO  bool
		wantSrc string
		wantDst string
		wantErr bool
		errMsg  string
	}{
		{"ro basic", "ro:/src:/dst", true, "/src", "/dst", false, ""},
		{"rw basic", "rw:/src:/dst", false, "/src", "/dst", false, ""},
		{"RO uppercase", "RO:/src:/dst", true, "/src", "/dst", false, ""},
		{"RW uppercase", "RW:/src:/dst", false, "/src", "/dst", false, ""},
		{"spaces in mode", " ro :/src:/dst", true, "/src", "/dst", false, ""},
		{"spaces in paths", "ro: /src : /dst ", true, "/src", "/dst", false, ""},
		{"invalid mode", "xx:/src:/dst", false, "", "", true, "invalid --mount mode"},
		{"empty mode", ":/src:/dst", false, "", "", true, "invalid --mount mode"},
		{"too few parts", "ro:/src", false, "", "", true, "invalid --mount"},
		{"relative source", "ro:src:/dst", false, "", "", true, "must be absolute"},
		{"relative target", "ro:/src:dst", false, "", "", true, "must be absolute"},
		{"root target", "ro:/src:/", false, "", "", true, "not allowed"},
		{"path normalization", "ro:/src//extra:/dst/../dst", true, "/src//extra", "/dst", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := parseMount(tt.entry)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMount(%q) error = %v, wantErr %v", tt.entry, err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("error = %q, want to contain %q", err.Error(), tt.errMsg)
				}
				return
			}
			if spec.readOnly != tt.wantRO {
				t.Fatalf("readOnly = %v, want %v", spec.readOnly, tt.wantRO)
			}
			if spec.source != tt.wantSrc {
				t.Fatalf("source = %q, want %q", spec.source, tt.wantSrc)
			}
		})
	}
}

func TestChrootDirDefaultBase(t *testing.T) {
	cfg := Config{
		ID:       "vm-1",
		ExecFile: "/usr/bin/myapp",
	}
	got := cfg.chrootDir()
	want := "/srv/jailer/myapp/vm-1/root"
	if got != want {
		t.Fatalf("chrootDir() = %q, want %q", got, want)
	}
}

func TestChrootDirEmptyBase(t *testing.T) {
	cfg := Config{
		ID:            "vm-1",
		ExecFile:      "/usr/bin/myapp",
		ChrootBaseDir: "",
	}
	got := cfg.chrootDir()
	want := "/srv/jailer/myapp/vm-1/root"
	if got != want {
		t.Fatalf("chrootDir() with empty base = %q, want %q", got, want)
	}
}

func TestAppendUniqueStrings_Extended(t *testing.T) {
	tests := []struct {
		name   string
		dst    []string
		values []string
		want   int
	}{
		{"all unique", []string{"a"}, []string{"b", "c"}, 3},
		{"all duplicates", []string{"a", "b"}, []string{"a", "b"}, 2},
		{"mixed", []string{"a", "b"}, []string{"b", "c", "a", "d"}, 4},
		{"empty both", nil, nil, 0},
		{"empty values", []string{"a"}, nil, 1},
		{"empty dst", nil, []string{"a", "a"}, 1},
		{"duplicate in values", nil, []string{"a", "a", "b", "b"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendUniqueStrings(tt.dst, tt.values...)
			if len(got) != tt.want {
				t.Fatalf("len = %d, want %d; got %v", len(got), tt.want, got)
			}
		})
	}
}

func TestMkdirAllNoSymlink_ExistingDir(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	// Should succeed without error on existing directory
	if err := mkdirAllNoSymlink(path, 0755); err != nil {
		t.Fatalf("mkdirAllNoSymlink on existing dir = %v", err)
	}
}

func TestMkdirAllNoSymlink_RootPath(t *testing.T) {
	if err := mkdirAllNoSymlink("/", 0755); err != nil {
		t.Fatalf("mkdirAllNoSymlink('/') = %v", err)
	}
}

func TestMkdirAllNoSymlink_FileNotDir(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "file")
	if err := os.WriteFile(filePath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	err := mkdirAllNoSymlink(filepath.Join(filePath, "child"), 0755)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected 'not a directory' error, got %v", err)
	}
}

func TestExecEnv_Extended(t *testing.T) {
	t.Run("empty env returns PATH only", func(t *testing.T) {
		cfg := Config{}
		result := cfg.execEnv()
		if len(result) != 1 {
			t.Fatalf("expected 1 entry, got %d: %v", len(result), result)
		}
		if !strings.HasPrefix(result[0], "PATH=") {
			t.Fatalf("expected PATH=..., got %q", result[0])
		}
	})
	t.Run("custom env without PATH gets PATH appended", func(t *testing.T) {
		cfg := Config{Env: []string{"FOO=bar", "BAZ=qux"}}
		result := cfg.execEnv()
		if len(result) != 3 {
			t.Fatalf("expected 3 entries, got %d: %v", len(result), result)
		}
		hasPath := false
		for _, e := range result {
			if strings.HasPrefix(e, "PATH=") {
				hasPath = true
			}
		}
		if !hasPath {
			t.Fatal("expected PATH to be appended")
		}
	})
	t.Run("custom env with PATH keeps original", func(t *testing.T) {
		cfg := Config{Env: []string{"PATH=/custom/bin", "FOO=bar"}}
		result := cfg.execEnv()
		if len(result) != 2 {
			t.Fatalf("expected 2 entries (no extra PATH), got %d: %v", len(result), result)
		}
		if result[0] != "PATH=/custom/bin" {
			t.Fatalf("expected original PATH, got %q", result[0])
		}
	})
}

func TestConfigValidateRejectsMultipleMountErrors(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mounts []string
	}{
		{"invalid format", []string{"invalid"}},
		{"invalid mode", []string{"xx:/src:/dst"}},
		{"relative source", []string{"ro:src:/dst"}},
		{"relative target", []string{"ro:/src:dst"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				ID:       "test-vm",
				ExecFile: execFile,
				UID:      1000,
				GID:      1000,
				Mounts:   tt.mounts,
			}
			if err := cfg.validate(); err == nil {
				t.Fatal("expected error for invalid mount")
			}
		})
	}
}

func TestConfigValidateMultipleEnvEntries(t *testing.T) {
	tmp := t.TempDir()
	execFile := filepath.Join(tmp, "vmm")
	if err := osWriteFile(execFile); err != nil {
		t.Fatal(err)
	}

	t.Run("first valid, second invalid", func(t *testing.T) {
		cfg := Config{
			ID:       "test-vm",
			ExecFile: execFile,
			UID:      1000,
			GID:      1000,
			Env:      []string{"GOOD=val", "BADENTRY"},
		}
		err := cfg.validate()
		if err == nil || !strings.Contains(err.Error(), "invalid --env") {
			t.Fatalf("expected invalid env error, got %v", err)
		}
	})

	t.Run("all valid", func(t *testing.T) {
		cfg := Config{
			ID:       "test-vm",
			ExecFile: execFile,
			UID:      1000,
			GID:      1000,
			Env:      []string{"A=1", "B=2", "C="},
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("validate() = %v, want nil", err)
		}
	})
}

func TestMkdirAllNoSymlink_DeepNested(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "c", "d", "e")
	if err := mkdirAllNoSymlink(path, 0755); err != nil {
		t.Fatalf("mkdirAllNoSymlink deep path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestCopyRegularFileRejectsSymlinkSource(t *testing.T) {
	root := t.TempDir()
	realSrc := filepath.Join(root, "real.bin")
	if err := osWriteFile(realSrc); err != nil {
		t.Fatal(err)
	}
	linkSrc := filepath.Join(root, "link.bin")
	if err := os.Symlink(realSrc, linkSrc); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst.bin")
	err := copyRegularFile(linkSrc, dst, 0755)
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("expected symlink source rejection, got %v", err)
	}
}
