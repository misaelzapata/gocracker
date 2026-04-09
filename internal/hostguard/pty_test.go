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
