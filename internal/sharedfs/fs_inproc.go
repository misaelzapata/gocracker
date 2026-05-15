// Package sharedfs — in-process FUSE-over-virtio server.
//
// On platforms where virtiofsd is unavailable (notably Windows), the host
// cannot spawn an external binary to back virtio-fs. This file implements a
// pure-Go FUSE protocol server that reads FUSE requests from a duplex
// connection, executes the corresponding os.* operations against a host
// directory, and writes FUSE replies back.
//
// The server speaks the FUSE ABI (major 7, ops 1-46), which is the same
// wire protocol virtiofsd uses on the virtio-fs request virtqueue. The
// embedded server is opt-in via Config.InProcess; the existing
// virtiofsd-spawning path is unchanged for Linux.
//
// Scope: covers the operations a Linux guest uses for normal file I/O
// (LOOKUP, GETATTR, READ, WRITE, READDIR, MKDIR, CREATE, UNLINK, RENAME,
// SETATTR, SYMLINK, READLINK, OPEN/RELEASE, OPENDIR/RELEASEDIR, FORGET,
// STATFS, INIT, DESTROY, FLUSH, FSYNC). Extended attributes, locking,
// ioctls, and poll are stubbed with ENOSYS.
package sharedfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// FUSE wire-protocol constants and structs (subset of <linux/fuse.h>).
// ---------------------------------------------------------------------------

const (
	fuseKernelVersion      uint32 = 7
	fuseKernelMinorVersion uint32 = 31
	fuseRootID             uint64 = 1
	fuseMaxWrite           uint32 = 1024 * 1024
	fuseMaxRead            uint32 = 1024 * 1024
	fuseInHeaderSize              = 40
	fuseOutHeaderSize             = 16
	fuseDirentAlign               = 8
)

// FUSE opcodes (a subset — operations not implemented are replied to with
// ENOSYS so the kernel falls back gracefully).
const (
	fuseLookup      uint32 = 1
	fuseForget      uint32 = 2
	fuseGetattr     uint32 = 3
	fuseSetattr     uint32 = 4
	fuseReadlink    uint32 = 5
	fuseSymlink     uint32 = 6
	fuseMknod       uint32 = 8
	fuseMkdir       uint32 = 9
	fuseUnlink      uint32 = 10
	fuseRmdir       uint32 = 11
	fuseRename      uint32 = 12
	fuseLink        uint32 = 13
	fuseOpen        uint32 = 14
	fuseRead        uint32 = 15
	fuseWrite       uint32 = 16
	fuseStatfs      uint32 = 17
	fuseRelease     uint32 = 18
	fuseFsync       uint32 = 20
	fuseFlush       uint32 = 25
	fuseInit        uint32 = 26
	fuseOpendir     uint32 = 27
	fuseReaddir     uint32 = 28
	fuseReleasedir  uint32 = 29
	fuseFsyncdir    uint32 = 30
	fuseAccess      uint32 = 34
	fuseCreate      uint32 = 35
	fuseDestroy     uint32 = 38
	fuseBatchForget uint32 = 42
	fuseReaddirplus uint32 = 44
)

// fuse_in_header: 40 bytes. All fields little-endian.
type fuseInHeader struct {
	Len    uint32
	Opcode uint32
	Unique uint64
	NodeID uint64
	UID    uint32
	GID    uint32
	PID    uint32
	Pad    uint32
}

// fuse_out_header: 16 bytes.
type fuseOutHeader struct {
	Len    uint32
	Error  int32
	Unique uint64
}

// fuse_attr: 88 bytes. Mirrors struct fuse_attr.
type fuseAttr struct {
	Ino       uint64
	Size      uint64
	Blocks    uint64
	Atime     uint64
	Mtime     uint64
	Ctime     uint64
	AtimeNsec uint32
	MtimeNsec uint32
	CtimeNsec uint32
	Mode      uint32
	NLink     uint32
	UID       uint32
	GID       uint32
	Rdev      uint32
	Blksize   uint32
	Pad       uint32
}

// fuse_entry_out: 16 + 16 + 88 = 120 bytes.
type fuseEntryOut struct {
	NodeID         uint64
	Generation     uint64
	EntryValid     uint64
	AttrValid      uint64
	EntryValidNsec uint32
	AttrValidNsec  uint32
	Attr           fuseAttr
}

// fuse_attr_out: 16 + 88 = 104 bytes.
type fuseAttrOut struct {
	AttrValid     uint64
	AttrValidNsec uint32
	Dummy         uint32
	Attr          fuseAttr
}

// fuse_open_out: 16 bytes.
type fuseOpenOut struct {
	FH        uint64
	OpenFlags uint32
	Pad       uint32
}

// fuse_init_in: 16 bytes (legacy) or 64 bytes (modern).
type fuseInitIn struct {
	Major        uint32
	Minor        uint32
	MaxReadahead uint32
	Flags        uint32
}

// fuse_init_out: 64 bytes.
type fuseInitOut struct {
	Major               uint32
	Minor               uint32
	MaxReadahead        uint32
	Flags               uint32
	MaxBackground       uint16
	CongestionThreshold uint16
	MaxWrite            uint32
	TimeGran            uint32
	MaxPages            uint16
	MapAlignment        uint16
	Unused              [8]uint32
}

// fuse_read_in: 40 bytes.
type fuseReadIn struct {
	FH        uint64
	Offset    uint64
	Size      uint32
	ReadFlags uint32
	LockOwner uint64
	Flags     uint32
	Pad       uint32
}

// fuse_write_in: 40 bytes.
type fuseWriteIn struct {
	FH         uint64
	Offset     uint64
	Size       uint32
	WriteFlags uint32
	LockOwner  uint64
	Flags      uint32
	Pad        uint32
}

// fuse_write_out: 8 bytes.
type fuseWriteOut struct {
	Size uint32
	Pad  uint32
}

// fuse_release_in: 24 bytes.
type fuseReleaseIn struct {
	FH           uint64
	Flags        uint32
	ReleaseFlags uint32
	LockOwner    uint64
}

// fuse_setattr_in: 88 bytes. Only the fields we honour are read.
type fuseSetattrIn struct {
	Valid     uint32
	Pad       uint32
	FH        uint64
	Size      uint64
	LockOwner uint64
	Atime     uint64
	Mtime     uint64
	Ctime     uint64
	AtimeNsec uint32
	MtimeNsec uint32
	CtimeNsec uint32
	Mode      uint32
	Unused4   uint32
	UID       uint32
	GID       uint32
	Unused5   uint32
}

const (
	fattrMode    uint32 = 1 << 0
	fattrUID     uint32 = 1 << 1
	fattrGID     uint32 = 1 << 2
	fattrSize    uint32 = 1 << 3
	fattrAtime   uint32 = 1 << 4
	fattrMtime   uint32 = 1 << 5
	fattrFH      uint32 = 1 << 6
	fattrAtimeNow uint32 = 1 << 7
	fattrMtimeNow uint32 = 1 << 8
)

// fuse_create_in: 16 bytes followed by filename.
type fuseCreateIn struct {
	Flags uint32
	Mode  uint32
	Umask uint32
	Pad   uint32
}

// fuse_mkdir_in: 8 bytes followed by name.
type fuseMkdirIn struct {
	Mode  uint32
	Umask uint32
}

// fuse_forget_in: 8 bytes.
type fuseForgetIn struct {
	NLookup uint64
}

// fuse_kstatfs: 80 bytes.
type fuseKStatfs struct {
	Blocks  uint64
	BFree   uint64
	BAvail  uint64
	Files   uint64
	FFree   uint64
	BSize   uint32
	NameLen uint32
	FrSize  uint32
	Pad     uint32
	Spare   [6]uint32
}

// fuse_statfs_out wraps fuseKStatfs.
type fuseStatfsOut struct {
	Statfs fuseKStatfs
}

// fuse_dirent: variable. Header is 24 bytes + name + padding.
type fuseDirent struct {
	Ino     uint64
	Off     uint64
	NameLen uint32
	Type    uint32
}

// ---------------------------------------------------------------------------
// Inode and handle tables.
// ---------------------------------------------------------------------------

type inode struct {
	id     uint64
	path   string // path relative to root, cleaned, "" for root
	lookup int64  // refcount from kernel LOOKUP/FORGET
}

type fileHandle struct {
	f   *os.File
	dir bool
	// For dir handles, we read the directory eagerly on opendir.
	entries []dirEntry
}

type dirEntry struct {
	name string
	mode os.FileMode
	ino  uint64
}

// ---------------------------------------------------------------------------
// Config and Server.
// ---------------------------------------------------------------------------

// Config holds parameters shared by all backends. Filled in by callers of
// Start/StartAt; the new InProcess field switches to the embedded server.
type Config struct {
	// SharedDir is the host directory whose contents are exposed to the guest.
	SharedDir string
	// Tag is the virtio-fs tag the guest mounts by.
	Tag string
	// SocketPath, when non-empty for the spawn path, overrides the temp socket
	// location (mirrors StartAt).
	SocketPath string
	// InProcess selects the embedded Go FUSE server instead of execing
	// virtiofsd. The Backend's Conn() returns the host-side endpoint of the
	// internal channel; SocketPath() is empty in this mode.
	InProcess bool
}

// inProcServer drives the FUSE protocol over a duplex connection.
type inProcServer struct {
	root string
	conn net.Conn
	// peer is the guest-side end of the pipe (handed to virtio-fs).
	peer net.Conn

	mu        sync.Mutex
	inodes    map[uint64]*inode
	pathIndex map[string]*inode
	nextIno   uint64
	handles   map[uint64]*fileHandle
	nextFH    uint64

	closed atomic.Bool
	done   chan struct{}
}

// startInProcServer wires up the duplex pipe, seeds the root inode, and
// kicks off the request loop in a goroutine. The returned Backend exposes
// the peer connection via Conn().
func startInProcServer(cfg Config) (*Backend, error) {
	root := cfg.SharedDir
	if root == "" {
		return nil, fmt.Errorf("sharedfs: SharedDir is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("sharedfs: resolve SharedDir: %w", err)
	}
	if info, err := os.Stat(absRoot); err != nil {
		return nil, fmt.Errorf("sharedfs: SharedDir %q: %w", absRoot, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("sharedfs: SharedDir %q is not a directory", absRoot)
	}

	host, peer := net.Pipe()
	s := &inProcServer{
		root:      absRoot,
		conn:      host,
		peer:      peer,
		inodes:    make(map[uint64]*inode),
		pathIndex: make(map[string]*inode),
		nextIno:   fuseRootID + 1,
		handles:   make(map[uint64]*fileHandle),
		nextFH:    1,
		done:      make(chan struct{}),
	}
	rootInode := &inode{id: fuseRootID, path: ""}
	s.inodes[fuseRootID] = rootInode
	s.pathIndex[""] = rootInode

	go s.serve()
	return &Backend{inproc: s}, nil
}

// Conn returns the guest-side endpoint of the in-process FUSE channel when
// the backend is running in-process. For spawn-based backends it returns nil.
func (b *Backend) Conn() net.Conn {
	if b == nil || b.inproc == nil {
		return nil
	}
	return b.inproc.peer
}

// InProcess reports whether the backend is running in-process (no external
// virtiofsd).
func (b *Backend) InProcess() bool {
	return b != nil && b.inproc != nil
}

// resolve converts a node-relative name into a sanitised absolute host path,
// guarding against `..` traversal. The returned path is guaranteed to live
// under s.root.
func (s *inProcServer) resolve(parent *inode, name string) (string, string, error) {
	// Reject embedded separators or absolute references in name.
	if strings.ContainsAny(name, "/\\") || name == ".." || name == "." || name == "" {
		// Lookups for "." and ".." are legal; tag them so callers can decide.
		switch name {
		case "":
			return s.root, "", nil
		case ".":
			return filepath.Join(s.root, parent.path), parent.path, nil
		case "..":
			// Stay at root: refuse to step outside.
			if parent.path == "" {
				return s.root, "", nil
			}
			parentRel := filepath.Dir(parent.path)
			if parentRel == "." {
				parentRel = ""
			}
			return filepath.Join(s.root, parentRel), parentRel, nil
		default:
			return "", "", os.ErrInvalid
		}
	}
	rel := filepath.Join(parent.path, name)
	abs := filepath.Join(s.root, rel)
	// Belt-and-suspenders: confirm abs is rooted under s.root.
	relCheck, err := filepath.Rel(s.root, abs)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return "", "", os.ErrPermission
	}
	return abs, rel, nil
}

func (s *inProcServer) lookupByID(id uint64) (*inode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.inodes[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return n, nil
}

// allocInode either returns an existing inode for rel or creates one. Must be
// called with s.mu held.
func (s *inProcServer) allocInodeLocked(rel string) *inode {
	if existing, ok := s.pathIndex[rel]; ok {
		return existing
	}
	id := s.nextIno
	s.nextIno++
	n := &inode{id: id, path: rel}
	s.inodes[id] = n
	s.pathIndex[rel] = n
	return n
}

// ---------------------------------------------------------------------------
// Serve loop.
// ---------------------------------------------------------------------------

func (s *inProcServer) serve() {
	defer close(s.done)
	defer s.conn.Close()
	for {
		if s.closed.Load() {
			return
		}
		var hdrBuf [fuseInHeaderSize]byte
		if _, err := io.ReadFull(s.conn, hdrBuf[:]); err != nil {
			return
		}
		hdr := fuseInHeader{
			Len:    binary.LittleEndian.Uint32(hdrBuf[0:4]),
			Opcode: binary.LittleEndian.Uint32(hdrBuf[4:8]),
			Unique: binary.LittleEndian.Uint64(hdrBuf[8:16]),
			NodeID: binary.LittleEndian.Uint64(hdrBuf[16:24]),
			UID:    binary.LittleEndian.Uint32(hdrBuf[24:28]),
			GID:    binary.LittleEndian.Uint32(hdrBuf[28:32]),
			PID:    binary.LittleEndian.Uint32(hdrBuf[32:36]),
		}
		if hdr.Len < fuseInHeaderSize {
			return
		}
		bodyLen := int(hdr.Len) - fuseInHeaderSize
		body := make([]byte, bodyLen)
		if bodyLen > 0 {
			if _, err := io.ReadFull(s.conn, body); err != nil {
				return
			}
		}
		s.dispatch(hdr, body)
	}
}

// ---------------------------------------------------------------------------
// Dispatch.
// ---------------------------------------------------------------------------

func (s *inProcServer) dispatch(hdr fuseInHeader, body []byte) {
	switch hdr.Opcode {
	case fuseInit:
		s.opInit(hdr, body)
	case fuseDestroy:
		s.opDestroy(hdr)
	case fuseLookup:
		s.opLookup(hdr, body)
	case fuseForget:
		s.opForget(hdr, body)
	case fuseBatchForget:
		s.opBatchForget(hdr, body)
	case fuseGetattr:
		s.opGetattr(hdr)
	case fuseSetattr:
		s.opSetattr(hdr, body)
	case fuseOpen:
		s.opOpen(hdr, body, false)
	case fuseOpendir:
		s.opOpen(hdr, body, true)
	case fuseRead:
		s.opRead(hdr, body)
	case fuseReaddir:
		s.opReaddir(hdr, body, false)
	case fuseReaddirplus:
		s.opReaddir(hdr, body, true)
	case fuseWrite:
		s.opWrite(hdr, body)
	case fuseRelease, fuseReleasedir:
		s.opRelease(hdr, body)
	case fuseFlush, fuseFsync, fuseFsyncdir:
		s.replyOK(hdr.Unique)
	case fuseCreate:
		s.opCreate(hdr, body)
	case fuseMkdir:
		s.opMkdir(hdr, body)
	case fuseUnlink:
		s.opUnlink(hdr, body, false)
	case fuseRmdir:
		s.opUnlink(hdr, body, true)
	case fuseRename:
		s.opRename(hdr, body)
	case fuseSymlink:
		s.opSymlink(hdr, body)
	case fuseReadlink:
		s.opReadlink(hdr)
	case fuseLink:
		s.opLink(hdr, body)
	case fuseStatfs:
		s.opStatfs(hdr)
	case fuseAccess:
		s.replyOK(hdr.Unique)
	default:
		s.replyError(hdr.Unique, syscall.ENOSYS)
	}
}

// ---------------------------------------------------------------------------
// Operation handlers.
// ---------------------------------------------------------------------------

func (s *inProcServer) opInit(hdr fuseInHeader, body []byte) {
	if len(body) < 8 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	out := fuseInitOut{
		Major:               fuseKernelVersion,
		Minor:               fuseKernelMinorVersion,
		MaxReadahead:        128 * 1024,
		Flags:               0,
		MaxBackground:       16,
		CongestionThreshold: 8,
		MaxWrite:            fuseMaxWrite,
		TimeGran:            1,
		MaxPages:            uint16(fuseMaxWrite / 4096),
	}
	buf := make([]byte, 64)
	binary.LittleEndian.PutUint32(buf[0:], out.Major)
	binary.LittleEndian.PutUint32(buf[4:], out.Minor)
	binary.LittleEndian.PutUint32(buf[8:], out.MaxReadahead)
	binary.LittleEndian.PutUint32(buf[12:], out.Flags)
	binary.LittleEndian.PutUint16(buf[16:], out.MaxBackground)
	binary.LittleEndian.PutUint16(buf[18:], out.CongestionThreshold)
	binary.LittleEndian.PutUint32(buf[20:], out.MaxWrite)
	binary.LittleEndian.PutUint32(buf[24:], out.TimeGran)
	binary.LittleEndian.PutUint16(buf[28:], out.MaxPages)
	binary.LittleEndian.PutUint16(buf[30:], 0)
	s.reply(hdr.Unique, 0, buf)
}

func (s *inProcServer) opDestroy(hdr fuseInHeader) {
	s.replyOK(hdr.Unique)
	s.closed.Store(true)
	_ = s.conn.Close()
}

func (s *inProcServer) opLookup(hdr fuseInHeader, body []byte) {
	parent, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	name := cstr(body)
	abs, rel, err := s.resolve(parent, name)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.mu.Lock()
	n := s.allocInodeLocked(rel)
	n.lookup++
	id := n.id
	s.mu.Unlock()
	entry := buildEntryOut(id, fi)
	s.reply(hdr.Unique, 0, encodeEntryOut(entry))
}

func (s *inProcServer) opForget(hdr fuseInHeader, body []byte) {
	if len(body) < 8 {
		return
	}
	n := binary.LittleEndian.Uint64(body[:8])
	s.forget(hdr.NodeID, n)
	// FORGET has no reply.
}

func (s *inProcServer) opBatchForget(hdr fuseInHeader, body []byte) {
	if len(body) < 8 {
		return
	}
	count := binary.LittleEndian.Uint32(body[:4])
	off := 8
	for i := uint32(0); i < count; i++ {
		if off+16 > len(body) {
			break
		}
		nid := binary.LittleEndian.Uint64(body[off : off+8])
		nlookup := binary.LittleEndian.Uint64(body[off+8 : off+16])
		s.forget(nid, nlookup)
		off += 16
	}
}

func (s *inProcServer) forget(id, nlookup uint64) {
	if id == fuseRootID {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.inodes[id]
	if !ok {
		return
	}
	n.lookup -= int64(nlookup)
	if n.lookup <= 0 {
		delete(s.inodes, id)
		delete(s.pathIndex, n.path)
	}
}

func (s *inProcServer) opGetattr(hdr fuseInHeader) {
	n, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	abs := filepath.Join(s.root, n.path)
	fi, err := os.Lstat(abs)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	out := fuseAttrOut{AttrValid: 1, Attr: buildFuseAttr(n.id, fi)}
	s.reply(hdr.Unique, 0, encodeAttrOut(out))
}

func (s *inProcServer) opSetattr(hdr fuseInHeader, body []byte) {
	if len(body) < 88 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	n, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	valid := binary.LittleEndian.Uint32(body[0:4])
	size := binary.LittleEndian.Uint64(body[24:32])
	atime := binary.LittleEndian.Uint64(body[40:48])
	mtime := binary.LittleEndian.Uint64(body[48:56])
	mode := binary.LittleEndian.Uint32(body[72:76])

	abs := filepath.Join(s.root, n.path)
	if valid&fattrSize != 0 {
		if err := os.Truncate(abs, int64(size)); err != nil {
			s.replyError(hdr.Unique, errnoFromError(err))
			return
		}
	}
	if valid&fattrMode != 0 {
		if err := os.Chmod(abs, os.FileMode(mode&0o7777)); err != nil {
			s.replyError(hdr.Unique, errnoFromError(err))
			return
		}
	}
	if valid&(fattrAtime|fattrMtime) != 0 {
		var at, mt time.Time
		if valid&fattrAtimeNow != 0 {
			at = time.Now()
		} else {
			at = time.Unix(int64(atime), 0)
		}
		if valid&fattrMtimeNow != 0 {
			mt = time.Now()
		} else {
			mt = time.Unix(int64(mtime), 0)
		}
		if err := os.Chtimes(abs, at, mt); err != nil {
			s.replyError(hdr.Unique, errnoFromError(err))
			return
		}
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	out := fuseAttrOut{AttrValid: 1, Attr: buildFuseAttr(n.id, fi)}
	s.reply(hdr.Unique, 0, encodeAttrOut(out))
}

func (s *inProcServer) opOpen(hdr fuseInHeader, body []byte, dir bool) {
	n, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	abs := filepath.Join(s.root, n.path)
	var fh *fileHandle
	if dir {
		entries, err := readDirEntries(abs)
		if err != nil {
			s.replyError(hdr.Unique, errnoFromError(err))
			return
		}
		fh = &fileHandle{dir: true, entries: entries}
	} else {
		flags := uint32(0)
		if len(body) >= 4 {
			flags = binary.LittleEndian.Uint32(body[0:4])
		}
		f, err := os.OpenFile(abs, posixToGoFlags(flags), 0)
		if err != nil {
			s.replyError(hdr.Unique, errnoFromError(err))
			return
		}
		fh = &fileHandle{f: f}
	}
	s.mu.Lock()
	id := s.nextFH
	s.nextFH++
	s.handles[id] = fh
	s.mu.Unlock()

	buf := make([]byte, 16)
	binary.LittleEndian.PutUint64(buf[0:8], id)
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	s.reply(hdr.Unique, 0, buf)
}

func (s *inProcServer) opRead(hdr fuseInHeader, body []byte) {
	if len(body) < 16 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	fh := binary.LittleEndian.Uint64(body[0:8])
	offset := binary.LittleEndian.Uint64(body[8:16])
	size := binary.LittleEndian.Uint32(body[16:20])
	if size > fuseMaxRead {
		size = fuseMaxRead
	}
	s.mu.Lock()
	h, ok := s.handles[fh]
	s.mu.Unlock()
	if !ok || h.f == nil {
		s.replyError(hdr.Unique, syscall.EBADF)
		return
	}
	buf := make([]byte, size)
	nread, err := h.f.ReadAt(buf, int64(offset))
	if err != nil && err != io.EOF {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.reply(hdr.Unique, 0, buf[:nread])
}

func (s *inProcServer) opWrite(hdr fuseInHeader, body []byte) {
	if len(body) < 40 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	fh := binary.LittleEndian.Uint64(body[0:8])
	offset := binary.LittleEndian.Uint64(body[8:16])
	size := binary.LittleEndian.Uint32(body[16:20])
	data := body[40:]
	if uint32(len(data)) < size {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	s.mu.Lock()
	h, ok := s.handles[fh]
	s.mu.Unlock()
	if !ok || h.f == nil {
		s.replyError(hdr.Unique, syscall.EBADF)
		return
	}
	n, err := h.f.WriteAt(data[:size], int64(offset))
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(n))
	s.reply(hdr.Unique, 0, buf)
}

func (s *inProcServer) opRelease(hdr fuseInHeader, body []byte) {
	if len(body) < 8 {
		s.replyOK(hdr.Unique)
		return
	}
	fh := binary.LittleEndian.Uint64(body[0:8])
	s.mu.Lock()
	h, ok := s.handles[fh]
	if ok {
		delete(s.handles, fh)
	}
	s.mu.Unlock()
	if ok && h.f != nil {
		_ = h.f.Close()
	}
	s.replyOK(hdr.Unique)
}

func (s *inProcServer) opReaddir(hdr fuseInHeader, body []byte, plus bool) {
	if len(body) < 24 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	fh := binary.LittleEndian.Uint64(body[0:8])
	offset := binary.LittleEndian.Uint64(body[8:16])
	size := binary.LittleEndian.Uint32(body[16:20])
	s.mu.Lock()
	h, ok := s.handles[fh]
	s.mu.Unlock()
	if !ok || !h.dir {
		s.replyError(hdr.Unique, syscall.EBADF)
		return
	}
	buf := make([]byte, 0, size)
	for i := int(offset); i < len(h.entries); i++ {
		e := h.entries[i]
		entryHdr := fuseDirent{
			Ino:     e.ino,
			Off:     uint64(i + 1),
			NameLen: uint32(len(e.name)),
			Type:    direntType(e.mode),
		}
		entryBuf := encodeDirent(entryHdr, e.name, plus, e.ino, e.mode)
		if uint32(len(buf)+len(entryBuf)) > size {
			break
		}
		buf = append(buf, entryBuf...)
	}
	s.reply(hdr.Unique, 0, buf)
}

func (s *inProcServer) opCreate(hdr fuseInHeader, body []byte) {
	if len(body) < 16 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	parent, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	flags := binary.LittleEndian.Uint32(body[0:4])
	mode := binary.LittleEndian.Uint32(body[4:8])
	name := cstr(body[16:])
	abs, rel, err := s.resolve(parent, name)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	f, err := os.OpenFile(abs, posixToGoFlags(flags)|os.O_CREATE, os.FileMode(mode&0o7777))
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.mu.Lock()
	n := s.allocInodeLocked(rel)
	n.lookup++
	id := s.nextFH
	s.nextFH++
	s.handles[id] = &fileHandle{f: f}
	s.mu.Unlock()
	entry := buildEntryOut(n.id, fi)
	entryBuf := encodeEntryOut(entry)
	open := make([]byte, 16)
	binary.LittleEndian.PutUint64(open[0:8], id)
	s.reply(hdr.Unique, 0, append(entryBuf, open...))
}

func (s *inProcServer) opMkdir(hdr fuseInHeader, body []byte) {
	if len(body) < 8 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	parent, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	mode := binary.LittleEndian.Uint32(body[0:4])
	name := cstr(body[8:])
	abs, rel, err := s.resolve(parent, name)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	if err := os.Mkdir(abs, os.FileMode(mode&0o7777)); err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.mu.Lock()
	n := s.allocInodeLocked(rel)
	n.lookup++
	id := n.id
	s.mu.Unlock()
	entry := buildEntryOut(id, fi)
	s.reply(hdr.Unique, 0, encodeEntryOut(entry))
}

func (s *inProcServer) opUnlink(hdr fuseInHeader, body []byte, dir bool) {
	parent, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	name := cstr(body)
	abs, rel, err := s.resolve(parent, name)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	if dir {
		err = os.Remove(abs)
	} else {
		err = os.Remove(abs)
	}
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.mu.Lock()
	if existing, ok := s.pathIndex[rel]; ok {
		delete(s.pathIndex, rel)
		delete(s.inodes, existing.id)
	}
	s.mu.Unlock()
	s.replyOK(hdr.Unique)
}

func (s *inProcServer) opRename(hdr fuseInHeader, body []byte) {
	if len(body) < 8 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	parent, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	newParentID := binary.LittleEndian.Uint64(body[0:8])
	newParent, err := s.lookupByID(newParentID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	rest := body[8:]
	oldName, idx := cstrAt(rest, 0)
	newName, _ := cstrAt(rest, idx)
	oldAbs, oldRel, err := s.resolve(parent, oldName)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	newAbs, newRel, err := s.resolve(newParent, newName)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	if err := os.Rename(oldAbs, newAbs); err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.mu.Lock()
	if existing, ok := s.pathIndex[oldRel]; ok {
		delete(s.pathIndex, oldRel)
		existing.path = newRel
		s.pathIndex[newRel] = existing
	}
	s.mu.Unlock()
	s.replyOK(hdr.Unique)
}

func (s *inProcServer) opSymlink(hdr fuseInHeader, body []byte) {
	parent, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	name, idx := cstrAt(body, 0)
	target, _ := cstrAt(body, idx)
	abs, rel, err := s.resolve(parent, name)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	if err := os.Symlink(target, abs); err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.mu.Lock()
	n := s.allocInodeLocked(rel)
	n.lookup++
	id := n.id
	s.mu.Unlock()
	entry := buildEntryOut(id, fi)
	s.reply(hdr.Unique, 0, encodeEntryOut(entry))
}

func (s *inProcServer) opReadlink(hdr fuseInHeader) {
	n, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	abs := filepath.Join(s.root, n.path)
	target, err := os.Readlink(abs)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.reply(hdr.Unique, 0, []byte(target))
}

func (s *inProcServer) opLink(hdr fuseInHeader, body []byte) {
	if len(body) < 8 {
		s.replyError(hdr.Unique, syscall.EINVAL)
		return
	}
	oldNodeID := binary.LittleEndian.Uint64(body[0:8])
	oldNode, err := s.lookupByID(oldNodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	newParent, err := s.lookupByID(hdr.NodeID)
	if err != nil {
		s.replyError(hdr.Unique, syscall.ENOENT)
		return
	}
	name := cstr(body[8:])
	oldAbs := filepath.Join(s.root, oldNode.path)
	newAbs, newRel, err := s.resolve(newParent, name)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	if err := os.Link(oldAbs, newAbs); err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	fi, err := os.Lstat(newAbs)
	if err != nil {
		s.replyError(hdr.Unique, errnoFromError(err))
		return
	}
	s.mu.Lock()
	n := s.allocInodeLocked(newRel)
	n.lookup++
	id := n.id
	s.mu.Unlock()
	entry := buildEntryOut(id, fi)
	s.reply(hdr.Unique, 0, encodeEntryOut(entry))
}

func (s *inProcServer) opStatfs(hdr fuseInHeader) {
	// Best-effort: report a generous filesystem. Cross-platform statvfs is
	// noisy; the kernel only uses these for `df` output.
	out := fuseStatfsOut{Statfs: fuseKStatfs{
		Blocks:  1 << 30,
		BFree:   1 << 29,
		BAvail:  1 << 29,
		Files:   1 << 20,
		FFree:   1 << 20,
		BSize:   4096,
		NameLen: 255,
		FrSize:  4096,
	}}
	buf := make([]byte, 80)
	binary.LittleEndian.PutUint64(buf[0:8], out.Statfs.Blocks)
	binary.LittleEndian.PutUint64(buf[8:16], out.Statfs.BFree)
	binary.LittleEndian.PutUint64(buf[16:24], out.Statfs.BAvail)
	binary.LittleEndian.PutUint64(buf[24:32], out.Statfs.Files)
	binary.LittleEndian.PutUint64(buf[32:40], out.Statfs.FFree)
	binary.LittleEndian.PutUint32(buf[40:44], out.Statfs.BSize)
	binary.LittleEndian.PutUint32(buf[44:48], out.Statfs.NameLen)
	binary.LittleEndian.PutUint32(buf[48:52], out.Statfs.FrSize)
	s.reply(hdr.Unique, 0, buf)
}

// ---------------------------------------------------------------------------
// Reply helpers.
// ---------------------------------------------------------------------------

func (s *inProcServer) reply(unique uint64, errno int32, body []byte) {
	total := fuseOutHeaderSize + len(body)
	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(errno))
	binary.LittleEndian.PutUint64(buf[8:16], unique)
	copy(buf[16:], body)
	_, _ = s.conn.Write(buf)
}

func (s *inProcServer) replyOK(unique uint64) {
	s.reply(unique, 0, nil)
}

func (s *inProcServer) replyError(unique uint64, e syscall.Errno) {
	s.reply(unique, -int32(e), nil)
}

// ---------------------------------------------------------------------------
// Encoding helpers.
// ---------------------------------------------------------------------------

func buildEntryOut(id uint64, fi os.FileInfo) fuseEntryOut {
	return fuseEntryOut{
		NodeID:     id,
		Generation: 0,
		EntryValid: 1,
		AttrValid:  1,
		Attr:       buildFuseAttr(id, fi),
	}
}

func buildFuseAttr(id uint64, fi os.FileInfo) fuseAttr {
	mode := goModeToFuseMode(fi.Mode())
	size := uint64(fi.Size())
	blocks := (size + 511) / 512
	mtime := fi.ModTime()
	return fuseAttr{
		Ino:     id,
		Size:    size,
		Blocks:  blocks,
		Atime:   uint64(mtime.Unix()),
		Mtime:   uint64(mtime.Unix()),
		Ctime:   uint64(mtime.Unix()),
		Mode:    mode,
		NLink:   1,
		Blksize: 4096,
	}
}

func encodeEntryOut(e fuseEntryOut) []byte {
	buf := make([]byte, 128) // 40 hdr + 88 attr = 128
	binary.LittleEndian.PutUint64(buf[0:8], e.NodeID)
	binary.LittleEndian.PutUint64(buf[8:16], e.Generation)
	binary.LittleEndian.PutUint64(buf[16:24], e.EntryValid)
	binary.LittleEndian.PutUint64(buf[24:32], e.AttrValid)
	binary.LittleEndian.PutUint32(buf[32:36], e.EntryValidNsec)
	binary.LittleEndian.PutUint32(buf[36:40], e.AttrValidNsec)
	encodeAttr(buf[40:], e.Attr)
	return buf
}

func encodeAttrOut(o fuseAttrOut) []byte {
	buf := make([]byte, 16+88)
	binary.LittleEndian.PutUint64(buf[0:8], o.AttrValid)
	binary.LittleEndian.PutUint32(buf[8:12], o.AttrValidNsec)
	encodeAttr(buf[16:], o.Attr)
	return buf
}

func encodeAttr(buf []byte, a fuseAttr) {
	binary.LittleEndian.PutUint64(buf[0:8], a.Ino)
	binary.LittleEndian.PutUint64(buf[8:16], a.Size)
	binary.LittleEndian.PutUint64(buf[16:24], a.Blocks)
	binary.LittleEndian.PutUint64(buf[24:32], a.Atime)
	binary.LittleEndian.PutUint64(buf[32:40], a.Mtime)
	binary.LittleEndian.PutUint64(buf[40:48], a.Ctime)
	binary.LittleEndian.PutUint32(buf[48:52], a.AtimeNsec)
	binary.LittleEndian.PutUint32(buf[52:56], a.MtimeNsec)
	binary.LittleEndian.PutUint32(buf[56:60], a.CtimeNsec)
	binary.LittleEndian.PutUint32(buf[60:64], a.Mode)
	binary.LittleEndian.PutUint32(buf[64:68], a.NLink)
	binary.LittleEndian.PutUint32(buf[68:72], a.UID)
	binary.LittleEndian.PutUint32(buf[72:76], a.GID)
	binary.LittleEndian.PutUint32(buf[76:80], a.Rdev)
	binary.LittleEndian.PutUint32(buf[80:84], a.Blksize)
}

func encodeDirent(hdr fuseDirent, name string, plus bool, ino uint64, mode os.FileMode) []byte {
	nameBytes := []byte(name)
	padding := (fuseDirentAlign - (len(nameBytes) % fuseDirentAlign)) % fuseDirentAlign
	prefix := 24
	if plus {
		prefix = 24 + 128 // dirent + entry_out (without name)
		// readdirplus uses a fuse_direntplus: entry_out(128) + dirent(24) + name + pad
	}
	total := prefix + len(nameBytes) + padding
	buf := make([]byte, total)
	off := 0
	if plus {
		entry := fuseEntryOut{NodeID: ino, EntryValid: 1, AttrValid: 1, Attr: fuseAttr{Ino: ino, Mode: goModeToFuseMode(mode), NLink: 1, Blksize: 4096}}
		copy(buf[off:], encodeEntryOut(entry))
		off += 128
	}
	binary.LittleEndian.PutUint64(buf[off:off+8], hdr.Ino)
	binary.LittleEndian.PutUint64(buf[off+8:off+16], hdr.Off)
	binary.LittleEndian.PutUint32(buf[off+16:off+20], hdr.NameLen)
	binary.LittleEndian.PutUint32(buf[off+20:off+24], hdr.Type)
	copy(buf[off+24:], nameBytes)
	return buf
}

// ---------------------------------------------------------------------------
// Misc helpers.
// ---------------------------------------------------------------------------

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// cstrAt returns the C-string starting at offset and the index immediately
// after the terminating NUL.
func cstrAt(b []byte, off int) (string, int) {
	if off >= len(b) {
		return "", off
	}
	for i := off; i < len(b); i++ {
		if b[i] == 0 {
			return string(b[off:i]), i + 1
		}
	}
	return string(b[off:]), len(b)
}

func readDirEntries(path string) ([]dirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]dirEntry, 0, len(entries)+2)
	out = append(out, dirEntry{name: ".", mode: os.ModeDir, ino: 0})
	out = append(out, dirEntry{name: "..", mode: os.ModeDir, ino: 0})
	for _, e := range entries {
		out = append(out, dirEntry{name: e.Name(), mode: e.Type(), ino: 0})
	}
	return out, nil
}

func direntType(mode os.FileMode) uint32 {
	switch {
	case mode&os.ModeDir != 0:
		return 4 // DT_DIR
	case mode&os.ModeSymlink != 0:
		return 10 // DT_LNK
	case mode&os.ModeNamedPipe != 0:
		return 1 // DT_FIFO
	case mode&os.ModeSocket != 0:
		return 12 // DT_SOCK
	case mode&os.ModeDevice != 0:
		if mode&os.ModeCharDevice != 0 {
			return 2 // DT_CHR
		}
		return 6 // DT_BLK
	default:
		return 8 // DT_REG
	}
}

func goModeToFuseMode(m os.FileMode) uint32 {
	out := uint32(m.Perm())
	switch {
	case m&os.ModeDir != 0:
		out |= 0o040000
	case m&os.ModeSymlink != 0:
		out |= 0o120000
	case m&os.ModeNamedPipe != 0:
		out |= 0o010000
	case m&os.ModeSocket != 0:
		out |= 0o140000
	case m&os.ModeDevice != 0:
		if m&os.ModeCharDevice != 0 {
			out |= 0o020000
		} else {
			out |= 0o060000
		}
	default:
		out |= 0o100000
	}
	return out
}

// posixToGoFlags translates a small subset of POSIX open flags to the Go
// constants. We only honour the bits a normal kernel sends on OPEN/CREATE.
func posixToGoFlags(flags uint32) int {
	const (
		oRDONLY = 0o0
		oWRONLY = 0o1
		oRDWR   = 0o2
		oAppend = 0o2000
		oTrunc  = 0o1000
	)
	out := 0
	switch flags & 0o3 {
	case oRDONLY:
		out |= os.O_RDONLY
	case oWRONLY:
		out |= os.O_WRONLY
	case oRDWR:
		out |= os.O_RDWR
	}
	if flags&oAppend != 0 {
		out |= os.O_APPEND
	}
	if flags&oTrunc != 0 {
		out |= os.O_TRUNC
	}
	return out
}

// errnoFromError maps Go errors to the closest POSIX errno value.
func errnoFromError(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var sysErr syscall.Errno
	if errors.As(err, &sysErr) {
		return sysErr
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, os.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, os.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, os.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, io.EOF):
		return 0
	default:
		return syscall.EIO
	}
}

// ---------------------------------------------------------------------------
// Public entry point.
// ---------------------------------------------------------------------------

// StartInProcess starts an in-process FUSE server backed by the host
// directory at cfg.SharedDir. The returned Backend's Conn() gives the
// caller (the virtio-fs device) the guest-side endpoint of the FUSE
// channel. Closing the Backend tears down the server.
//
// This is the cross-platform alternative to Start; on Linux either path may
// be used, while on Windows this is the only option (no virtiofsd binary
// available).
func StartInProcess(cfg Config) (*Backend, error) {
	cfg.InProcess = true
	return startInProcServer(cfg)
}
