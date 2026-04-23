package guestexec

// Binary wire protocol for the guest exec channel.
//
// Every frame is length-prefixed:
//
//	[u32 BE length] [payload]
//
// The payload layout depends on whether it is a Request or a Response, but
// the first byte is always the mode (for Request) or a fixed 0x00 marker
// (for Response), so the reader/writer pair stays unambiguous.
//
// All integers are big-endian. All byte slices are length-prefixed by u32.
// Short strings (paths, commands) are length-prefixed by u32 to keep the
// decoder branch-free.
//
// Rationale: dropping JSON here removes json.Encoder / json.Decoder
// allocations and the string-escape pass in the per-exec hot path. A
// typical ExecRequest serialises to ~100 bytes of binary vs ~300+ bytes
// of JSON and decodes about 6–8x faster on a microbench.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// modeCode maps the string Mode tag into a single byte on the wire.
// Keep in sync with the ModeX constants in protocol.go.
const (
	wireModeExec                byte = 1
	wireModeStream              byte = 2
	wireModeMemoryStats         byte = 3
	wireModeMemoryHotplugGet    byte = 4
	wireModeMemoryHotplugUpdate byte = 5
	wireModeResize              byte = 6
)

// responseTag marks a frame as a Response rather than a Request. The fact
// that no Mode ever encodes to 0 lets the reader distinguish the two
// without adding a shared wrapper struct.
const responseTag byte = 0

var (
	errShortFrame    = errors.New("guestexec: binary frame too short")
	errUnknownMode   = errors.New("guestexec: unknown mode byte")
	errBadFrameKind  = errors.New("guestexec: unknown frame tag")
	errPayloadLength = errors.New("guestexec: declared payload length exceeds frame")
)

// maxFrameSize caps an incoming frame so a bogus length prefix can't
// allocate hundreds of MB. 64 MiB is huge compared to real exec payloads
// (typically under 16 KiB) but leaves room for large file/json blobs that
// still pass through this channel today.
const maxFrameSize = 64 * 1024 * 1024

// EncodeBinary writes value as a length-prefixed binary frame to w. value
// must be *Request or *Response; anything else is a programmer error.
func EncodeBinary(w io.Writer, value any) error {
	switch v := value.(type) {
	case *Request:
		return writeRequestFrame(w, v)
	case Request:
		return writeRequestFrame(w, &v)
	case *Response:
		return writeResponseFrame(w, v)
	case Response:
		return writeResponseFrame(w, &v)
	default:
		return fmt.Errorf("guestexec: unsupported binary type %T", value)
	}
}

// DecodeBinary reads one length-prefixed frame from r and fills value. It
// dispatches on the frame's tag byte so the caller does not have to know
// whether the peer sent a Request or a Response.
func DecodeBinary(r io.Reader, value any) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return err
	}
	flen := binary.BigEndian.Uint32(lenBuf[:])
	if flen == 0 {
		return errShortFrame
	}
	if flen > maxFrameSize {
		return fmt.Errorf("guestexec: frame length %d exceeds cap %d", flen, maxFrameSize)
	}
	payload := make([]byte, flen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	tag := payload[0]
	body := payload[1:]
	switch tag {
	case responseTag:
		resp, ok := value.(*Response)
		if !ok {
			return fmt.Errorf("guestexec: expected *Response, got %T", value)
		}
		return parseResponse(body, resp)
	case wireModeExec, wireModeStream, wireModeMemoryStats,
		wireModeMemoryHotplugGet, wireModeMemoryHotplugUpdate, wireModeResize:
		req, ok := value.(*Request)
		if !ok {
			return fmt.Errorf("guestexec: expected *Request, got %T", value)
		}
		return parseRequest(tag, body, req)
	default:
		return errBadFrameKind
	}
}

// ----------------------------- Request ---------------------------------

func writeRequestFrame(w io.Writer, req *Request) error {
	mode, err := modeToByte(req.Mode)
	if err != nil {
		return err
	}
	var buf []byte
	buf = append(buf, mode)
	switch mode {
	case wireModeExec, wireModeStream:
		buf = appendU16(buf, uint16(clampU16(req.Columns)))
		buf = appendU16(buf, uint16(clampU16(req.Rows)))
		buf = appendStringBytes(buf, req.Stdin)
		buf = appendStringList(buf, req.Command)
		buf = appendStringList(buf, req.Env)
		buf = appendString(buf, req.WorkDir)
	case wireModeMemoryStats:
		// no extra payload
	case wireModeMemoryHotplugGet, wireModeMemoryHotplugUpdate:
		buf = appendU64(buf, req.MemoryHotplugBaseAddr)
		buf = appendU64(buf, req.MemoryHotplugTotalBytes)
		buf = appendU64(buf, req.MemoryHotplugBlockBytes)
		buf = appendU64(buf, req.MemoryHotplugTargetBytes)
	case wireModeResize:
		buf = appendU16(buf, uint16(clampU16(req.Columns)))
		buf = appendU16(buf, uint16(clampU16(req.Rows)))
	}
	return writeLengthPrefixed(w, buf)
}

func parseRequest(tag byte, body []byte, req *Request) error {
	*req = Request{Mode: byteToMode(tag)}
	r := &reader{b: body}
	switch tag {
	case wireModeExec, wireModeStream:
		cols, err := r.u16()
		if err != nil {
			return err
		}
		rows, err := r.u16()
		if err != nil {
			return err
		}
		req.Columns = int(cols)
		req.Rows = int(rows)
		stdinB, err := r.bytes()
		if err != nil {
			return err
		}
		req.Stdin = string(stdinB)
		cmd, err := r.strings()
		if err != nil {
			return err
		}
		req.Command = cmd
		env, err := r.strings()
		if err != nil {
			return err
		}
		req.Env = env
		wd, err := r.stringField()
		if err != nil {
			return err
		}
		req.WorkDir = wd
	case wireModeMemoryStats:
		// no fields
	case wireModeMemoryHotplugGet, wireModeMemoryHotplugUpdate:
		a, err := r.u64()
		if err != nil {
			return err
		}
		req.MemoryHotplugBaseAddr = a
		a, err = r.u64()
		if err != nil {
			return err
		}
		req.MemoryHotplugTotalBytes = a
		a, err = r.u64()
		if err != nil {
			return err
		}
		req.MemoryHotplugBlockBytes = a
		a, err = r.u64()
		if err != nil {
			return err
		}
		req.MemoryHotplugTargetBytes = a
	case wireModeResize:
		cols, err := r.u16()
		if err != nil {
			return err
		}
		rows, err := r.u16()
		if err != nil {
			return err
		}
		req.Columns = int(cols)
		req.Rows = int(rows)
	}
	return nil
}

// ----------------------------- Response --------------------------------

func writeResponseFrame(w io.Writer, resp *Response) error {
	var buf []byte
	buf = append(buf, responseTag)
	var okByte byte
	if resp.OK {
		okByte = 1
	}
	buf = append(buf, okByte)
	buf = appendI32(buf, int32(resp.ExitCode))
	buf = appendString(buf, resp.Stdout)
	buf = appendString(buf, resp.Stderr)
	buf = appendString(buf, resp.Error)
	if resp.MemoryStats != nil {
		buf = append(buf, 1)
		ms := resp.MemoryStats
		for _, v := range [...]uint64{
			ms.SwapIn, ms.SwapOut, ms.MajorFaults, ms.MinorFaults,
			ms.FreeMemory, ms.TotalMemory, ms.AvailableMemory, ms.DiskCaches,
			ms.OOMKill, ms.AllocStall, ms.AsyncScan, ms.DirectScan,
			ms.AsyncReclaim, ms.DirectReclaim,
		} {
			buf = appendU64(buf, v)
		}
	} else {
		buf = append(buf, 0)
	}
	if resp.MemoryHotplug != nil {
		buf = append(buf, 1)
		mh := resp.MemoryHotplug
		buf = appendU64(buf, mh.BlockSizeBytes)
		buf = appendU64(buf, mh.RequestedBytes)
		buf = appendU64(buf, mh.PluggedBytes)
		buf = appendU64(buf, mh.OnlineBlocks)
		buf = appendU64(buf, mh.PresentBlocks)
	} else {
		buf = append(buf, 0)
	}
	return writeLengthPrefixed(w, buf)
}

func parseResponse(body []byte, resp *Response) error {
	r := &reader{b: body}
	okByte, err := r.u8()
	if err != nil {
		return err
	}
	resp.OK = okByte != 0
	ec, err := r.i32()
	if err != nil {
		return err
	}
	resp.ExitCode = int(ec)
	stdout, err := r.stringField()
	if err != nil {
		return err
	}
	resp.Stdout = stdout
	stderr, err := r.stringField()
	if err != nil {
		return err
	}
	resp.Stderr = stderr
	errStr, err := r.stringField()
	if err != nil {
		return err
	}
	resp.Error = errStr
	hasMS, err := r.u8()
	if err != nil {
		return err
	}
	if hasMS != 0 {
		ms := &MemoryStats{}
		fields := [...]*uint64{
			&ms.SwapIn, &ms.SwapOut, &ms.MajorFaults, &ms.MinorFaults,
			&ms.FreeMemory, &ms.TotalMemory, &ms.AvailableMemory, &ms.DiskCaches,
			&ms.OOMKill, &ms.AllocStall, &ms.AsyncScan, &ms.DirectScan,
			&ms.AsyncReclaim, &ms.DirectReclaim,
		}
		for _, p := range fields {
			v, err := r.u64()
			if err != nil {
				return err
			}
			*p = v
		}
		resp.MemoryStats = ms
	}
	hasMH, err := r.u8()
	if err != nil {
		return err
	}
	if hasMH != 0 {
		mh := &MemoryHotplug{}
		for _, p := range []*uint64{&mh.BlockSizeBytes, &mh.RequestedBytes, &mh.PluggedBytes, &mh.OnlineBlocks, &mh.PresentBlocks} {
			v, err := r.u64()
			if err != nil {
				return err
			}
			*p = v
		}
		resp.MemoryHotplug = mh
	}
	return nil
}

// ----------------------------- helpers ---------------------------------

func modeToByte(mode string) (byte, error) {
	switch mode {
	case ModeExec:
		return wireModeExec, nil
	case ModeStream:
		return wireModeStream, nil
	case ModeMemoryStats:
		return wireModeMemoryStats, nil
	case ModeMemoryHotplugGet:
		return wireModeMemoryHotplugGet, nil
	case ModeMemoryHotplugUpdate:
		return wireModeMemoryHotplugUpdate, nil
	case ModeResize:
		return wireModeResize, nil
	default:
		return 0, fmt.Errorf("%w: %q", errUnknownMode, mode)
	}
}

func byteToMode(b byte) string {
	switch b {
	case wireModeExec:
		return ModeExec
	case wireModeStream:
		return ModeStream
	case wireModeMemoryStats:
		return ModeMemoryStats
	case wireModeMemoryHotplugGet:
		return ModeMemoryHotplugGet
	case wireModeMemoryHotplugUpdate:
		return ModeMemoryHotplugUpdate
	case wireModeResize:
		return ModeResize
	default:
		return ""
	}
}

func writeLengthPrefixed(w io.Writer, payload []byte) error {
	if len(payload) > maxFrameSize {
		return fmt.Errorf("guestexec: payload %d exceeds frame cap %d", len(payload), maxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func clampU16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 0xFFFF {
		return 0xFFFF
	}
	return uint16(v)
}

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendU64(b []byte, v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return append(b, buf[:]...)
}

func appendI32(b []byte, v int32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(v))
	return append(b, buf[:]...)
}

func appendStringBytes(b []byte, s string) []byte {
	return appendU32Len(b, []byte(s))
}

func appendString(b []byte, s string) []byte {
	return appendU32Len(b, []byte(s))
}

func appendU32Len(b []byte, data []byte) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	b = append(b, hdr[:]...)
	return append(b, data...)
}

func appendStringList(b []byte, items []string) []byte {
	b = appendU16(b, uint16(len(items)))
	for _, s := range items {
		b = appendString(b, s)
	}
	return b
}

type reader struct {
	b   []byte
	off int
}

func (r *reader) remaining() int { return len(r.b) - r.off }

func (r *reader) u8() (byte, error) {
	if r.remaining() < 1 {
		return 0, errShortFrame
	}
	v := r.b[r.off]
	r.off++
	return v, nil
}

func (r *reader) u16() (uint16, error) {
	if r.remaining() < 2 {
		return 0, errShortFrame
	}
	v := binary.BigEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v, nil
}

func (r *reader) u32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, errShortFrame
	}
	v := binary.BigEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v, nil
}

func (r *reader) u64() (uint64, error) {
	if r.remaining() < 8 {
		return 0, errShortFrame
	}
	v := binary.BigEndian.Uint64(r.b[r.off:])
	r.off += 8
	return v, nil
}

func (r *reader) i32() (int32, error) {
	v, err := r.u32()
	return int32(v), err
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if uint32(r.remaining()) < n {
		return nil, errPayloadLength
	}
	b := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return b, nil
}

func (r *reader) stringField() (string, error) {
	b, err := r.bytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *reader) strings() ([]string, error) {
	n, err := r.u16()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]string, n)
	for i := range out {
		s, err := r.stringField()
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}
