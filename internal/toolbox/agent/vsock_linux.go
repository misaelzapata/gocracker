//go:build linux

package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type vsockListener struct {
	// fd=-1 means "closed / never opened". Using 0 was wrong because fd
	// 0 is a legitimate file descriptor (stdin if nothing else has been
	// opened); if we ever received fd=0 for the vsock socket, Close()
	// would silently leak it.
	fd int
}

func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}

	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	var bindErr error
	for i := 0; i < 50; i++ {
		bindErr = unix.Bind(fd, sa)
		if bindErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if bindErr != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, bindErr)
	}
	if err := unix.Listen(fd, 16); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	l := &vsockListener{fd: fd}
	return l, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	nfd, sa, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}
	return newVsockConn(nfd, sa)
}

func (l *vsockListener) Close() error {
	if l.fd < 0 {
		return nil
	}
	err := unix.Close(l.fd)
	l.fd = -1
	return err
}

func (l *vsockListener) Addr() net.Addr { return vsockAddr{} }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

type vsockConn struct {
	file  *os.File
	local net.Addr
	peer  net.Addr
}

func newVsockConn(fd int, peer unix.Sockaddr) (net.Conn, error) {
	file := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-conn:%d", fd))
	local, err := unix.Getsockname(fd)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &vsockConn{
		file:  file,
		local: sockaddrToAddr(local),
		peer:  sockaddrToAddr(peer),
	}, nil
}

func (c *vsockConn) Read(p []byte) (int, error)  { return c.file.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error) { return c.file.Write(p) }
func (c *vsockConn) Close() error                { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr         { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr        { return c.peer }
func (c *vsockConn) SetDeadline(t time.Time) error {
	return c.file.SetDeadline(t)
}
func (c *vsockConn) SetReadDeadline(t time.Time) error {
	return c.file.SetReadDeadline(t)
}
func (c *vsockConn) SetWriteDeadline(t time.Time) error {
	return c.file.SetWriteDeadline(t)
}

func sockaddrToAddr(sa unix.Sockaddr) net.Addr {
	vm, ok := sa.(*unix.SockaddrVM)
	if !ok {
		return vsockAddr{}
	}
	return guestVsockAddr{cid: vm.CID, port: vm.Port}
}

type guestVsockAddr struct {
	cid  uint32
	port uint32
}

func (a guestVsockAddr) Network() string { return "vsock" }
func (a guestVsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

var _ net.Listener = (*vsockListener)(nil)
var _ net.Addr = vsockAddr{}
var _ net.Conn = (*vsockConn)(nil)

var errVsockUnsupported = errors.New("vsock unsupported")

type deadlineConn interface {
	SetDeadline(time.Time) error
}
