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
