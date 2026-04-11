package usercfg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveUsernameWithSupplementalGroups(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1001::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "app:x:1001:\nwheel:x:10:app\nstaff:x:20:app,other\n")

	got, err := Resolve(root, "app")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := Resolved{
		UID:    1000,
		GID:    1001,
		Groups: []uint32{10, 20},
		Home:   "/home/app",
		Name:   "app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveNumericUserDefaultsGroupToUID(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve(root, "1234")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.UID != 1234 || got.GID != 1234 {
		t.Fatalf("Resolve() = %#v, want uid=gid=1234", got)
	}
}

func TestResolveExplicitGroupOverridesSupplementals(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1001::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "app:x:1001:\nwheel:x:10:app\n")

	got, err := Resolve(root, "app:2000")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.GID != 2000 {
		t.Fatalf("Resolve().GID = %d, want 2000", got.GID)
	}
	if len(got.Groups) != 0 {
		t.Fatalf("Resolve().Groups = %#v, want none", got.Groups)
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func TestResolveEmptySpec(t *testing.T) {
	root := t.TempDir()
	_, err := Resolve(root, "")
	if err == nil {
		t.Fatal("expected error for empty spec")
	}
}

func TestResolveEmptyUserPart(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "root:x:0:0::/root:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "root:x:0:\n")
	_, err := Resolve(root, ":1000")
	if err == nil {
		t.Fatal("expected error for spec with empty user part")
	}
}

func TestResolveUnknownUser(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "root:x:0:0::/root:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "root:x:0:\n")
	_, err := Resolve(root, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestResolveUnknownGroup(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1000::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "app:x:1000:\n")
	_, err := Resolve(root, "app:nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestResolveNumericGroup(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1001::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "app:x:1001:\n")
	got, err := Resolve(root, "app:5000")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.GID != 5000 {
		t.Fatalf("GID = %d, want 5000", got.GID)
	}
}

func TestResolveNamedGroup(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1001::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "app:x:1001:\nwheel:x:10:app\n")
	got, err := Resolve(root, "app:wheel")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.GID != 10 {
		t.Fatalf("GID = %d, want 10", got.GID)
	}
}

func TestResolveNumericUserInPasswd(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1001::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "app:x:1001:\nwheel:x:10:app\n")
	got, err := Resolve(root, "1000")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.UID != 1000 || got.GID != 1001 {
		t.Fatalf("UID=%d GID=%d, want 1000:1001", got.UID, got.GID)
	}
	if got.Home != "/home/app" {
		t.Fatalf("Home = %q, want /home/app", got.Home)
	}
	if got.Name != "app" {
		t.Fatalf("Name = %q, want app", got.Name)
	}
}

func TestResolveNumericUserNotInPasswd(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "root:x:0:0::/root:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "root:x:0:\n")
	got, err := Resolve(root, "9999")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// When not in passwd, GID defaults to UID
	if got.UID != 9999 || got.GID != 9999 {
		t.Fatalf("UID=%d GID=%d, want 9999:9999", got.UID, got.GID)
	}
}

func TestResolveNoPasswdFile(t *testing.T) {
	root := t.TempDir()
	// No /etc/passwd at all
	_, err := Resolve(root, "1000")
	if err != nil {
		t.Fatalf("Resolve with no passwd should not error for numeric: %v", err)
	}
}

func TestResolveNoGroupFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1001::/home/app:/bin/sh\n")
	// No /etc/group file
	got, err := Resolve(root, "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.UID != 1000 || got.GID != 1001 {
		t.Fatalf("UID=%d GID=%d, want 1000:1001", got.UID, got.GID)
	}
	// No supplemental groups because no group file
	if len(got.Groups) != 0 {
		t.Fatalf("Groups = %v, want empty", got.Groups)
	}
}

func TestReadPasswd_CommentsAndBlanks(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"),
		"# comment line\n\nroot:x:0:0::/root:/bin/sh\n  \napp:x:1000:1000::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "")
	got, err := Resolve(root, "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.UID != 1000 {
		t.Fatalf("UID = %d, want 1000", got.UID)
	}
}

func TestReadGroups_CommentsAndBlanks(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1000::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"),
		"# comment\n\napp:x:1000:\n  \nwheel:x:10:app\n")
	got, err := Resolve(root, "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Groups) != 1 || got.Groups[0] != 10 {
		t.Fatalf("Groups = %v, want [10]", got.Groups)
	}
}

func TestReadPasswd_ShortLines(t *testing.T) {
	root := t.TempDir()
	// Line with too few fields should be skipped
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "short:line\napp:x:1000:1000:gecos:/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "")
	got, err := Resolve(root, "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.UID != 1000 {
		t.Fatalf("UID = %d, want 1000", got.UID)
	}
}

func TestReadGroups_ShortLines(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1000::/home/app:/bin/sh\n")
	// Line with too few fields should be skipped
	writeTestFile(t, filepath.Join(root, "etc", "group"), "short:x\nstaff:x:50:app\n")
	got, err := Resolve(root, "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Groups) != 1 || got.Groups[0] != 50 {
		t.Fatalf("Groups = %v, want [50]", got.Groups)
	}
}

func TestParseUint32(t *testing.T) {
	tests := []struct {
		input   string
		want    uint32
		wantErr bool
	}{
		{"0", 0, false},
		{"1000", 1000, false},
		{"4294967295", 4294967295, false},
		{" 42 ", 42, false},
		{"abc", 0, true},
		{"-1", 0, true},
		{"4294967296", 0, true},
	}
	for _, tt := range tests {
		got, err := parseUint32(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseUint32(%q) err=%v, wantErr=%v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseUint32(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestReadPasswd_BadUID(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:baduid:1000::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "")
	_, err := Resolve(root, "app")
	if err == nil {
		t.Fatal("expected error for bad uid in passwd")
	}
}

func TestReadGroups_BadGID(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1000::/home/app:/bin/sh\n")
	writeTestFile(t, filepath.Join(root, "etc", "group"), "staff:x:badgid:app\n")
	_, err := Resolve(root, "app")
	if err == nil {
		t.Fatal("expected error for bad gid in group")
	}
}

func TestSupplementalGroups_NoDuplicatePrimary(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc", "passwd"), "app:x:1000:1001::/home/app:/bin/sh\n")
	// "app" is a member of its own primary group - should be excluded from supplemental
	writeTestFile(t, filepath.Join(root, "etc", "group"), "app:x:1001:app\nwheel:x:10:app\n")
	got, err := Resolve(root, "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Groups) != 1 || got.Groups[0] != 10 {
		t.Fatalf("Groups = %v, want [10]", got.Groups)
	}
}

func TestResolveWhitespaceSpec(t *testing.T) {
	root := t.TempDir()
	_, err := Resolve(root, "   ")
	if err == nil {
		t.Fatal("expected error for whitespace-only spec")
	}
}
