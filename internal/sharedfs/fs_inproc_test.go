package sharedfs

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fuseClient is a tiny test helper that frames FUSE requests onto the wire
// and decodes the reply header + body. It mirrors what the kernel's virtio-fs
// front-end does, minus the virtqueue glue.
type fuseClient struct {
	c       net.Conn
	unique  uint64
	tBuf    [4096]byte
}

func newFuseClient(c net.Conn) *fuseClient { return &fuseClient{c: c, unique: 1} }

func (fc *fuseClient) request(opcode uint32, nodeID uint64, body []byte) (errno int32, reply []byte, err error) {
	hdr := make([]byte, fuseInHeaderSize)
	total := uint32(fuseInHeaderSize + len(body))
	binary.LittleEndian.PutUint32(hdr[0:4], total)
	binary.LittleEndian.PutUint32(hdr[4:8], opcode)
	binary.LittleEndian.PutUint64(hdr[8:16], fc.unique)
	binary.LittleEndian.PutUint64(hdr[16:24], nodeID)
	unique := fc.unique
	fc.unique++
	if err := fc.c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, nil, err
	}
	if _, err := fc.c.Write(hdr); err != nil {
		return 0, nil, err
	}
	if len(body) > 0 {
		if _, err := fc.c.Write(body); err != nil {
			return 0, nil, err
		}
	}
	var rhdr [fuseOutHeaderSize]byte
	if _, err := io.ReadFull(fc.c, rhdr[:]); err != nil {
		return 0, nil, err
	}
	rlen := binary.LittleEndian.Uint32(rhdr[0:4])
	rerr := int32(binary.LittleEndian.Uint32(rhdr[4:8]))
	rid := binary.LittleEndian.Uint64(rhdr[8:16])
	if rid != unique {
		return 0, nil, &mismatchErr{got: rid, want: unique}
	}
	bodyLen := int(rlen) - fuseOutHeaderSize
	if bodyLen <= 0 {
		return rerr, nil, nil
	}
	out := make([]byte, bodyLen)
	if _, err := io.ReadFull(fc.c, out); err != nil {
		return 0, nil, err
	}
	return rerr, out, nil
}

type mismatchErr struct{ got, want uint64 }

func (m *mismatchErr) Error() string { return "reply unique id mismatch" }

func startTestServer(t *testing.T, root string) (*Backend, *fuseClient) {
	t.Helper()
	b, err := StartInProcess(Config{SharedDir: root, Tag: "testtag"})
	if err != nil {
		t.Fatalf("StartInProcess: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if !b.InProcess() {
		t.Fatal("Backend.InProcess() = false")
	}
	if b.SocketPath() != "" {
		t.Fatalf("in-proc backend should have empty SocketPath, got %q", b.SocketPath())
	}
	conn := b.Conn()
	if conn == nil {
		t.Fatal("Backend.Conn() returned nil")
	}
	return b, newFuseClient(conn)
}

func TestInProcStartRequiresSharedDir(t *testing.T) {
	if _, err := StartInProcess(Config{}); err == nil {
		t.Fatal("StartInProcess() with empty config: expected error")
	}
}

func TestInProcInit(t *testing.T) {
	root := t.TempDir()
	_, fc := startTestServer(t, root)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[0:4], fuseKernelVersion)
	binary.LittleEndian.PutUint32(body[4:8], fuseKernelMinorVersion)
	binary.LittleEndian.PutUint32(body[8:12], 128*1024)
	binary.LittleEndian.PutUint32(body[12:16], 0)
	errno, reply, err := fc.request(fuseInit, fuseRootID, body)
	if err != nil {
		t.Fatalf("INIT: %v", err)
	}
	if errno != 0 {
		t.Fatalf("INIT errno = %d", errno)
	}
	if len(reply) < 32 {
		t.Fatalf("INIT reply too short: %d", len(reply))
	}
	major := binary.LittleEndian.Uint32(reply[0:4])
	if major != fuseKernelVersion {
		t.Fatalf("INIT major = %d, want %d", major, fuseKernelVersion)
	}
}

func TestInProcLookupAndGetattr(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "hello.txt")
	content := []byte("hello fuse")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, fc := startTestServer(t, root)

	// LOOKUP "hello.txt" under root.
	name := append([]byte("hello.txt"), 0)
	errno, reply, err := fc.request(fuseLookup, fuseRootID, name)
	if err != nil {
		t.Fatalf("LOOKUP: %v", err)
	}
	if errno != 0 {
		t.Fatalf("LOOKUP errno = %d", errno)
	}
	if len(reply) < 128 {
		t.Fatalf("LOOKUP reply too short: %d", len(reply))
	}
	nodeID := binary.LittleEndian.Uint64(reply[0:8])
	if nodeID == 0 || nodeID == fuseRootID {
		t.Fatalf("LOOKUP returned bogus nodeID %d", nodeID)
	}
	attrSize := binary.LittleEndian.Uint64(reply[40+8 : 40+16])
	if attrSize != uint64(len(content)) {
		t.Fatalf("LOOKUP size = %d, want %d", attrSize, len(content))
	}

	// GETATTR on the same node.
	errno, reply, err = fc.request(fuseGetattr, nodeID, nil)
	if err != nil {
		t.Fatalf("GETATTR: %v", err)
	}
	if errno != 0 {
		t.Fatalf("GETATTR errno = %d", errno)
	}
	if len(reply) < 16+88 {
		t.Fatalf("GETATTR reply short: %d", len(reply))
	}
	gsize := binary.LittleEndian.Uint64(reply[16+8 : 16+16])
	if gsize != uint64(len(content)) {
		t.Fatalf("GETATTR size = %d, want %d", gsize, len(content))
	}
}

func TestInProcOpenAndRead(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "data.bin")
	content := []byte("the quick brown fox jumps over the lazy dog")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, fc := startTestServer(t, root)
	name := append([]byte("data.bin"), 0)
	errno, reply, err := fc.request(fuseLookup, fuseRootID, name)
	if err != nil || errno != 0 {
		t.Fatalf("LOOKUP: err=%v errno=%d", err, errno)
	}
	nodeID := binary.LittleEndian.Uint64(reply[0:8])

	openBody := make([]byte, 8)
	errno, reply, err = fc.request(fuseOpen, nodeID, openBody)
	if err != nil || errno != 0 {
		t.Fatalf("OPEN: err=%v errno=%d", err, errno)
	}
	fh := binary.LittleEndian.Uint64(reply[0:8])

	readBody := make([]byte, 40)
	binary.LittleEndian.PutUint64(readBody[0:8], fh)
	binary.LittleEndian.PutUint64(readBody[8:16], 0)
	binary.LittleEndian.PutUint32(readBody[16:20], uint32(len(content)))
	errno, reply, err = fc.request(fuseRead, nodeID, readBody)
	if err != nil || errno != 0 {
		t.Fatalf("READ: err=%v errno=%d", err, errno)
	}
	if !bytes.Equal(reply, content) {
		t.Fatalf("READ returned %q, want %q", reply, content)
	}

	// RELEASE the handle.
	rel := make([]byte, 24)
	binary.LittleEndian.PutUint64(rel[0:8], fh)
	errno, _, err = fc.request(fuseRelease, nodeID, rel)
	if err != nil || errno != 0 {
		t.Fatalf("RELEASE: err=%v errno=%d", err, errno)
	}
}

func TestInProcCreateWriteRead(t *testing.T) {
	root := t.TempDir()
	_, fc := startTestServer(t, root)

	// CREATE "new.txt".
	body := make([]byte, 16+len("new.txt")+1)
	binary.LittleEndian.PutUint32(body[0:4], 0o2)       // O_RDWR
	binary.LittleEndian.PutUint32(body[4:8], 0o644)     // mode
	copy(body[16:], "new.txt")
	errno, reply, err := fc.request(fuseCreate, fuseRootID, body)
	if err != nil || errno != 0 {
		t.Fatalf("CREATE: err=%v errno=%d", err, errno)
	}
	if len(reply) < 128+16 {
		t.Fatalf("CREATE reply too short: %d", len(reply))
	}
	nodeID := binary.LittleEndian.Uint64(reply[0:8])
	fh := binary.LittleEndian.Uint64(reply[128:136])

	// WRITE "payload".
	payload := []byte("payload")
	wbody := make([]byte, 40+len(payload))
	binary.LittleEndian.PutUint64(wbody[0:8], fh)
	binary.LittleEndian.PutUint64(wbody[8:16], 0)
	binary.LittleEndian.PutUint32(wbody[16:20], uint32(len(payload)))
	copy(wbody[40:], payload)
	errno, reply, err = fc.request(fuseWrite, nodeID, wbody)
	if err != nil || errno != 0 {
		t.Fatalf("WRITE: err=%v errno=%d", err, errno)
	}
	if binary.LittleEndian.Uint32(reply[0:4]) != uint32(len(payload)) {
		t.Fatalf("WRITE returned size %d", binary.LittleEndian.Uint32(reply[0:4]))
	}

	// Read back via the same handle.
	rbody := make([]byte, 40)
	binary.LittleEndian.PutUint64(rbody[0:8], fh)
	binary.LittleEndian.PutUint32(rbody[16:20], 64)
	errno, reply, err = fc.request(fuseRead, nodeID, rbody)
	if err != nil || errno != 0 {
		t.Fatalf("READ: err=%v errno=%d", err, errno)
	}
	if !bytes.Equal(reply, payload) {
		t.Fatalf("READ back = %q, want %q", reply, payload)
	}

	// RELEASE before reading from disk (Windows blocks unlink on open fd).
	rel := make([]byte, 24)
	binary.LittleEndian.PutUint64(rel[0:8], fh)
	if errno, _, err = fc.request(fuseRelease, nodeID, rel); err != nil || errno != 0 {
		t.Fatalf("RELEASE: err=%v errno=%d", err, errno)
	}

	// Confirm on disk.
	got, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("on-disk contents = %q (err %v)", got, err)
	}
}

func TestInProcMkdirAndReaddir(t *testing.T) {
	root := t.TempDir()
	_, fc := startTestServer(t, root)

	// MKDIR "sub".
	body := append(make([]byte, 8), append([]byte("sub"), 0)...)
	binary.LittleEndian.PutUint32(body[0:4], 0o755)
	errno, reply, err := fc.request(fuseMkdir, fuseRootID, body)
	if err != nil || errno != 0 {
		t.Fatalf("MKDIR: err=%v errno=%d", err, errno)
	}
	subID := binary.LittleEndian.Uint64(reply[0:8])

	// Create a couple files inside sub via host.
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, "sub", name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// OPENDIR sub.
	errno, reply, err = fc.request(fuseOpendir, subID, make([]byte, 8))
	if err != nil || errno != 0 {
		t.Fatalf("OPENDIR: err=%v errno=%d", err, errno)
	}
	fh := binary.LittleEndian.Uint64(reply[0:8])

	// READDIR.
	rbody := make([]byte, 40)
	binary.LittleEndian.PutUint64(rbody[0:8], fh)
	binary.LittleEndian.PutUint32(rbody[16:20], 4096)
	errno, reply, err = fc.request(fuseReaddir, subID, rbody)
	if err != nil || errno != 0 {
		t.Fatalf("READDIR: err=%v errno=%d", err, errno)
	}
	// Decode dirents and look for "a" and "b".
	found := map[string]bool{}
	off := 0
	for off+24 <= len(reply) {
		nameLen := binary.LittleEndian.Uint32(reply[off+16 : off+20])
		nameEnd := off + 24 + int(nameLen)
		if nameEnd > len(reply) {
			break
		}
		name := string(reply[off+24 : nameEnd])
		found[name] = true
		pad := (fuseDirentAlign - (int(nameLen) % fuseDirentAlign)) % fuseDirentAlign
		off = nameEnd + pad
	}
	if !found["a"] || !found["b"] {
		t.Fatalf("READDIR missing entries; got %v", found)
	}
}

func TestInProcUnlinkAndRename(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("aa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b"), []byte("bb"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, fc := startTestServer(t, root)

	// RENAME a -> c
	rb := make([]byte, 0, 16)
	rb = append(rb, make([]byte, 8)...)
	binary.LittleEndian.PutUint64(rb[0:8], fuseRootID)
	rb = append(rb, []byte("a")...)
	rb = append(rb, 0)
	rb = append(rb, []byte("c")...)
	rb = append(rb, 0)
	errno, _, err := fc.request(fuseRename, fuseRootID, rb)
	if err != nil || errno != 0 {
		t.Fatalf("RENAME: err=%v errno=%d", err, errno)
	}
	if _, err := os.Stat(filepath.Join(root, "c")); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatalf("original 'a' still exists: %v", err)
	}

	// UNLINK b
	ub := append([]byte("b"), 0)
	errno, _, err = fc.request(fuseUnlink, fuseRootID, ub)
	if err != nil || errno != 0 {
		t.Fatalf("UNLINK: err=%v errno=%d", err, errno)
	}
	if _, err := os.Stat(filepath.Join(root, "b")); !os.IsNotExist(err) {
		t.Fatalf("UNLINK left file behind: %v", err)
	}
}

func TestInProcLookupRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	// Create a sibling next to root that we shouldn't be able to reach.
	parent := filepath.Dir(root)
	outsideName := "outside-" + filepath.Base(root)
	outside := filepath.Join(parent, outsideName)
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	_, fc := startTestServer(t, root)

	// LOOKUP ".."
	name := append([]byte(".."), 0)
	errno, reply, err := fc.request(fuseLookup, fuseRootID, name)
	if err != nil {
		t.Fatalf("LOOKUP ..: %v", err)
	}
	if errno != 0 {
		t.Fatalf("LOOKUP .. errno = %d (expected to stay at root)", errno)
	}
	// The returned node should refer back to the root.
	gotNode := binary.LittleEndian.Uint64(reply[0:8])
	if gotNode != fuseRootID {
		t.Fatalf("LOOKUP .. returned node %d, want root %d", gotNode, fuseRootID)
	}

	// LOOKUP with embedded slash must be rejected.
	bad := append([]byte("../"+outsideName), 0)
	errno, _, err = fc.request(fuseLookup, fuseRootID, bad)
	if err != nil {
		t.Fatalf("LOOKUP traversal: %v", err)
	}
	if errno == 0 {
		t.Fatal("LOOKUP with '../sibling' was accepted; expected rejection")
	}
	if errno != -int32(syscall.EINVAL) && errno != -int32(syscall.EACCES) {
		t.Fatalf("LOOKUP traversal returned errno %d (want EINVAL or EACCES)", errno)
	}
}

func TestInProcDestroyCloses(t *testing.T) {
	root := t.TempDir()
	b, fc := startTestServer(t, root)
	if _, _, err := fc.request(fuseDestroy, fuseRootID, nil); err != nil {
		t.Logf("DESTROY: %v", err)
	}
	// Close after DESTROY should be idempotent.
	if err := b.Close(); err != nil {
		t.Fatalf("Close after DESTROY: %v", err)
	}
}

func TestInProcStartWithConfig(t *testing.T) {
	root := t.TempDir()
	b, err := StartWithConfig(Config{SharedDir: root, Tag: "t", InProcess: true})
	if err != nil {
		t.Fatalf("StartWithConfig: %v", err)
	}
	defer b.Close()
	if !b.InProcess() {
		t.Fatal("expected InProcess backend")
	}
}
