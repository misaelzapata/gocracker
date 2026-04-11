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
