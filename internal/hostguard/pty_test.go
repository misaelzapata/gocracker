package hostguard

import "testing"

func TestDevPtsMountOptions(t *testing.T) {
	t.Parallel()
	opts, ok := devPtsMountOptions("30 29 0:26 / /dev/pts rw,nosuid,noexec,relatime shared:3 - devpts devpts rw,gid=5,mode=620,ptmxmode=666\n")
	if !ok {
		t.Fatalf("expected devpts options")
	}
	if opts != "rw,gid=5,mode=620,ptmxmode=666" {
		t.Fatalf("unexpected opts: %q", opts)
	}
}

func TestDevPtsMountOptionsMissing(t *testing.T) {
	t.Parallel()
	if _, ok := devPtsMountOptions(""); ok {
		t.Fatalf("expected missing devpts options")
	}
}

// --- Additional tests ---

func TestDevPtsMountOptions_RestrictedMode(t *testing.T) {
	t.Parallel()
	opts, ok := devPtsMountOptions("30 29 0:26 / /dev/pts rw,nosuid shared:3 - devpts devpts rw,gid=5,mode=620,ptmxmode=000\n")
	if !ok {
		t.Fatal("expected devpts options to be found")
	}
	if opts != "rw,gid=5,mode=620,ptmxmode=000" {
		t.Fatalf("opts = %q, want rw,gid=5,mode=620,ptmxmode=000", opts)
	}
}

func TestDevPtsMountOptions_MultipleLines(t *testing.T) {
	t.Parallel()
	mountInfo := `22 1 0:21 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
30 29 0:26 / /dev/pts rw,nosuid,noexec,relatime shared:3 - devpts devpts rw,gid=5,mode=620,ptmxmode=666
31 29 0:27 / /dev/shm rw,nosuid,nodev shared:4 - tmpfs tmpfs rw
`
	opts, ok := devPtsMountOptions(mountInfo)
	if !ok {
		t.Fatal("expected devpts options from multiline input")
	}
	if opts != "rw,gid=5,mode=620,ptmxmode=666" {
		t.Fatalf("opts = %q", opts)
	}
}

func TestDevPtsMountOptions_NoDevpts(t *testing.T) {
	t.Parallel()
	mountInfo := `22 1 0:21 / /sys rw,nosuid shared:7 - sysfs sysfs rw
31 29 0:27 / /dev/shm rw,nosuid,nodev shared:4 - tmpfs tmpfs rw
`
	_, ok := devPtsMountOptions(mountInfo)
	if ok {
		t.Fatal("expected no devpts options when devpts is not mounted")
	}
}

func TestDevPtsMountOptions_MalformedLine(t *testing.T) {
	t.Parallel()
	// Line with /dev/pts but no " - " separator
	mountInfo := "30 29 0:26 / /dev/pts rw,nosuid devpts devpts rw,gid=5\n"
	_, ok := devPtsMountOptions(mountInfo)
	if ok {
		t.Fatal("expected no match for malformed line")
	}
}

func TestDevPtsMountOptions_WrongFSType(t *testing.T) {
	t.Parallel()
	// Line with /dev/pts but wrong fs type after " - "
	mountInfo := "30 29 0:26 / /dev/pts rw,nosuid shared:3 - tmpfs tmpfs rw\n"
	_, ok := devPtsMountOptions(mountInfo)
	if ok {
		t.Fatal("expected no match when fs type is not devpts")
	}
}

// ---- NEW TESTS ----

func TestDevPtsMountOptions_EmptyAfterSeparator(t *testing.T) {
	t.Parallel()
	// Line with " - " separator but insufficient fields after it
	mountInfo := "30 29 0:26 / /dev/pts rw shared:3 - devpts\n"
	_, ok := devPtsMountOptions(mountInfo)
	if ok {
		t.Fatal("expected no match with insufficient fields after separator")
	}
}

func TestDevPtsMountOptions_WrongFSNameAfterSeparator(t *testing.T) {
	t.Parallel()
	// fields after " - " have devpts in wrong position
	mountInfo := "30 29 0:26 / /dev/pts rw shared:3 - sysfs devpts rw,gid=5\n"
	_, ok := devPtsMountOptions(mountInfo)
	if ok {
		t.Fatal("expected no match when first field after separator is not devpts")
	}
}

func TestDevPtsMountOptions_ValidMinimalOpts(t *testing.T) {
	t.Parallel()
	mountInfo := "30 29 0:26 / /dev/pts rw shared:3 - devpts devpts rw\n"
	opts, ok := devPtsMountOptions(mountInfo)
	if !ok {
		t.Fatal("expected match for minimal devpts line")
	}
	if opts != "rw" {
		t.Fatalf("opts = %q, want rw", opts)
	}
}

func TestDevPtsMountOptions_MiddleOfManyLines(t *testing.T) {
	t.Parallel()
	mountInfo := `1 0 0:1 / / rw shared:1 - ext4 /dev/sda1 rw
2 1 0:2 / /proc rw shared:2 - proc proc rw
3 1 0:3 / /sys rw shared:3 - sysfs sysfs rw
4 1 0:4 / /dev rw shared:4 - devtmpfs devtmpfs rw
5 4 0:5 / /dev/pts rw,nosuid shared:5 - devpts devpts rw,gid=5,mode=620,ptmxmode=666
6 4 0:6 / /dev/shm rw shared:6 - tmpfs tmpfs rw
7 1 0:7 / /run rw shared:7 - tmpfs tmpfs rw
`
	opts, ok := devPtsMountOptions(mountInfo)
	if !ok {
		t.Fatal("expected match in multiline input")
	}
	if opts != "rw,gid=5,mode=620,ptmxmode=666" {
		t.Fatalf("opts = %q", opts)
	}
}

func TestCheckPTYSupport(t *testing.T) {
	// Just test it does not panic. On a normal system /dev/ptmx should exist.
	_ = CheckPTYSupport()
}
