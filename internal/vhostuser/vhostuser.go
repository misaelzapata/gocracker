package vhostuser

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	headerSize  = 12
	maxMsgSize  = 0x1000
	maxOOBSize  = 256
	protoV1Flag = 0x1

	headerFlagReply     = 0x4
	headerFlagNeedReply = 0x8

	FrontendReqGetFeatures         = 1
	FrontendReqSetFeatures         = 2
	FrontendReqSetOwner            = 3
	FrontendReqSetMemTable         = 5
	FrontendReqSetVringNum         = 8
	FrontendReqSetVringAddr        = 9
	FrontendReqSetVringBase        = 10
	FrontendReqSetVringKick        = 12
	FrontendReqSetVringCall        = 13
	FrontendReqGetProtocolFeatures = 15
	FrontendReqSetProtocolFeatures = 16
	FrontendReqGetQueueNum         = 17
	FrontendReqSetVringEnable      = 18

	VhostUserVirtioFeatureProtocolFeatures = 0x4000_0000

	ProtocolFeatureMQ       = 0x0000_0001
	ProtocolFeatureReplyAck = 0x0000_0008
	ProtocolFeatureConfig   = 0x0000_0200
)

type Header struct {
	Request uint32
	Flags   uint32
	Size    uint32
}

type U64 struct {
	Value uint64
}

type Memory struct {
	NumRegions uint32
	Padding    uint32
}

type MemoryRegion struct {
	GuestPhysAddr uint64
	MemorySize    uint64
	UserAddr      uint64
	MmapOffset    uint64
}

type VringState struct {
	Index uint32
	Num   uint32
}

type VringAddr struct {
	Index      uint32
	Flags      uint32
	Descriptor uint64
	Used       uint64
	Available  uint64
	Log        uint64
}

type Client struct {
	conn      *net.UnixConn
	needReply bool
}

func Dial(path string) (*Client, error) {
	var lastErr error
	for attempts := 0; attempts < 6; attempts++ {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err == nil {
			return &Client{conn: conn}, nil
		}
		lastErr = err
		if !retryableDialErr(err) || attempts == 5 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, lastErr
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) SetOwner() error {
	return c.sendRequest(FrontendReqSetOwner, nil, nil, false)
}

func (c *Client) GetFeatures() (uint64, error) {
	data, err := c.requestReply(FrontendReqGetFeatures, nil, nil)
	if err != nil {
		return 0, err
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("GET_FEATURES reply size %d", len(data))
	}
	return binary.LittleEndian.Uint64(data), nil
}

func (c *Client) SetFeatures(features uint64) error {
	var body [8]byte
	binary.LittleEndian.PutUint64(body[:], features)
	return c.sendRequest(FrontendReqSetFeatures, body[:], nil, true)
}

func (c *Client) GetProtocolFeatures() (uint64, error) {
	data, err := c.requestReply(FrontendReqGetProtocolFeatures, nil, nil)
	if err != nil {
		return 0, err
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("GET_PROTOCOL_FEATURES reply size %d", len(data))
	}
	return binary.LittleEndian.Uint64(data), nil
}

func (c *Client) SetProtocolFeatures(features uint64) error {
	var body [8]byte
	binary.LittleEndian.PutUint64(body[:], features)
	if err := c.sendRequest(FrontendReqSetProtocolFeatures, body[:], nil, false); err != nil {
		return err
	}
	c.needReply = features&ProtocolFeatureReplyAck != 0
	return nil
}

func (c *Client) GetQueueNum() (uint64, error) {
	data, err := c.requestReply(FrontendReqGetQueueNum, nil, nil)
	if err != nil {
		return 0, err
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("GET_QUEUE_NUM reply size %d", len(data))
	}
	return binary.LittleEndian.Uint64(data), nil
}

func (c *Client) SetMemTable(region MemoryRegion, fd int) error {
	body := make([]byte, 8+32)
	binary.LittleEndian.PutUint32(body[0:], 1)
	binary.LittleEndian.PutUint64(body[8:], region.GuestPhysAddr)
	binary.LittleEndian.PutUint64(body[16:], region.MemorySize)
	binary.LittleEndian.PutUint64(body[24:], region.UserAddr)
	binary.LittleEndian.PutUint64(body[32:], region.MmapOffset)
	return c.sendRequest(FrontendReqSetMemTable, body, []int{fd}, true)
}

func (c *Client) SetVringNum(queueIndex int, num uint16) error {
	var body [8]byte
	binary.LittleEndian.PutUint32(body[0:], uint32(queueIndex))
	binary.LittleEndian.PutUint32(body[4:], uint32(num))
	return c.sendRequest(FrontendReqSetVringNum, body[:], nil, true)
}

func (c *Client) SetVringAddr(queueIndex int, addr VringAddr) error {
	var body [40]byte
	binary.LittleEndian.PutUint32(body[0:], uint32(queueIndex))
	binary.LittleEndian.PutUint32(body[4:], addr.Flags)
	binary.LittleEndian.PutUint64(body[8:], addr.Descriptor)
	binary.LittleEndian.PutUint64(body[16:], addr.Used)
	binary.LittleEndian.PutUint64(body[24:], addr.Available)
	binary.LittleEndian.PutUint64(body[32:], addr.Log)
	return c.sendRequest(FrontendReqSetVringAddr, body[:], nil, true)
}

func (c *Client) SetVringBase(queueIndex int, base uint16) error {
	var body [8]byte
	binary.LittleEndian.PutUint32(body[0:], uint32(queueIndex))
	binary.LittleEndian.PutUint32(body[4:], uint32(base))
	return c.sendRequest(FrontendReqSetVringBase, body[:], nil, true)
}

func (c *Client) SetVringKick(queueIndex int, fd int) error {
	return c.sendFDRequest(FrontendReqSetVringKick, queueIndex, fd)
}

func (c *Client) SetVringCall(queueIndex int, fd int) error {
	return c.sendFDRequest(FrontendReqSetVringCall, queueIndex, fd)
}

func (c *Client) SetVringEnable(queueIndex int, enable bool) error {
	var body [8]byte
	binary.LittleEndian.PutUint32(body[0:], uint32(queueIndex))
	if enable {
		binary.LittleEndian.PutUint32(body[4:], 1)
	}
	return c.sendRequest(FrontendReqSetVringEnable, body[:], nil, true)
}

func (c *Client) sendFDRequest(request uint32, queueIndex int, fd int) error {
	var body [8]byte
	binary.LittleEndian.PutUint64(body[:], uint64(queueIndex))
	return c.sendRequest(request, body[:], []int{fd}, true)
}

func (c *Client) requestReply(request uint32, body []byte, fds []int) ([]byte, error) {
	if err := c.writeMessage(request, body, fds, false); err != nil {
		return nil, err
	}
	reply, err := c.readMessage()
	if err != nil {
		return nil, err
	}
	if reply.header.Request != request || reply.header.Flags&headerFlagReply == 0 {
		return nil, fmt.Errorf("unexpected reply req=%d flags=%#x", reply.header.Request, reply.header.Flags)
	}
	if len(reply.fds) > 0 {
		return nil, fmt.Errorf("unexpected reply file descriptors")
	}
	return reply.body, nil
}

func (c *Client) sendRequest(request uint32, body []byte, fds []int, allowAck bool) error {
	if err := c.writeMessage(request, body, fds, allowAck && c.needReply); err != nil {
		return err
	}
	if !allowAck || !c.needReply {
		return nil
	}
	reply, err := c.readMessage()
	if err != nil {
		return err
	}
	if reply.header.Request != request || reply.header.Flags&headerFlagReply == 0 {
		return fmt.Errorf("unexpected ack req=%d flags=%#x", reply.header.Request, reply.header.Flags)
	}
	if len(reply.fds) > 0 {
		return fmt.Errorf("unexpected ack file descriptors")
	}
	if len(reply.body) != 8 {
		return fmt.Errorf("unexpected ack size %d", len(reply.body))
	}
	if binary.LittleEndian.Uint64(reply.body) != 0 {
		return fmt.Errorf("backend rejected request %d", request)
	}
	return nil
}

func (c *Client) writeMessage(request uint32, body []byte, fds []int, needReply bool) error {
	if len(body) > maxMsgSize {
		return fmt.Errorf("oversized vhost-user message: %d", len(body))
	}
	var header [headerSize]byte
	flags := uint32(protoV1Flag)
	if needReply {
		flags |= headerFlagNeedReply
	}
	binary.LittleEndian.PutUint32(header[0:], request)
	binary.LittleEndian.PutUint32(header[4:], flags)
	binary.LittleEndian.PutUint32(header[8:], uint32(len(body)))

	packet := append(header[:], body...)
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	n, _, err := c.conn.WriteMsgUnix(packet, oob, nil)
	if err != nil {
		return err
	}
	if n != len(packet) {
		return fmt.Errorf("short vhost-user write: %d/%d", n, len(packet))
	}
	return nil
}

type message struct {
	header Header
	body   []byte
	fds    []int
}

func (c *Client) readMessage() (*message, error) {
	buf := make([]byte, maxMsgSize+headerSize)
	oob := make([]byte, maxOOBSize)

	n, oobn, _, _, err := c.conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	if n < headerSize {
		return nil, fmt.Errorf("short vhost-user header: %d", n)
	}

	header := Header{
		Request: binary.LittleEndian.Uint32(buf[0:4]),
		Flags:   binary.LittleEndian.Uint32(buf[4:8]),
		Size:    binary.LittleEndian.Uint32(buf[8:12]),
	}
	if header.Flags&0x3 != protoV1Flag {
		return nil, fmt.Errorf("unsupported vhost-user version flags=%#x", header.Flags)
	}
	total := headerSize + int(header.Size)
	if total > len(buf) {
		return nil, fmt.Errorf("oversized vhost-user reply: %d", total)
	}
	for n < total {
		read, err := c.conn.Read(buf[n:total])
		if err != nil {
			return nil, err
		}
		if read == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		n += read
	}

	var fds []int
	if oobn > 0 {
		msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			rights, err := unix.ParseUnixRights(&msg)
			if err != nil {
				return nil, err
			}
			fds = append(fds, rights...)
		}
	}

	return &message{
		header: header,
		body:   append([]byte(nil), buf[headerSize:total]...),
		fds:    fds,
	}, nil
}

func CloseFDs(fds []int) error {
	var errs []error
	for _, fd := range fds {
		if fd < 0 {
			continue
		}
		if err := unix.Close(fd); err != nil && !errors.Is(err, unix.EBADF) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func retryableDialErr(err error) bool {
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if netErr.Err == nil {
			return false
		}
		if errors.Is(netErr.Err, os.ErrNotExist) {
			return true
		}
		var sysErr *os.SyscallError
		if errors.As(netErr.Err, &sysErr) {
			return errors.Is(sysErr.Err, unix.ECONNREFUSED) || errors.Is(sysErr.Err, unix.ENOENT)
		}
		return errors.Is(netErr.Err, unix.ECONNREFUSED) || errors.Is(netErr.Err, unix.ENOENT)
	}
	return errors.Is(err, unix.ECONNREFUSED) || errors.Is(err, unix.ENOENT)
}
