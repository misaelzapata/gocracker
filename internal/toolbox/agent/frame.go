package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Wire format for the toolbox /exec data plane:
//
//	[1 byte channel] [4 bytes length BE] [length bytes payload]
//
// Designed to NOT inherit the limitations that buried the older
// internal/guestexec JSON protocol — binary-clean (payloads are bytes,
// not JSON strings), streaming (one frame at a time, no whole-response
// buffering), backpressure (kernel socket buffer is the only buffer),
// and channel-multiplexed (stdin/stdout/stderr/exit/signal share one
// socket without ambiguity).

// Channel identifiers. Matches the Docker-exec convention loosely; the
// numbers are stable wire constants and must never be reordered.
const (
	ChannelStdin  byte = 0 // host → guest
	ChannelStdout byte = 1 // guest → host
	ChannelStderr byte = 2 // guest → host
	ChannelExit   byte = 3 // guest → host (4-byte BE exit code, then EOF)
	ChannelSignal byte = 4 // host → guest (SIGTERM/SIGKILL/TIOCSWINSZ)
)

// MaxFrameLen caps any single payload at 1 MiB. The 4-byte length header
// permits 4 GiB but accepting that would let a malformed peer pin
// arbitrary memory. Stdout/stderr larger than this gets chunked into
// multiple frames by the writer side.
const MaxFrameLen = 1 << 20

// Errors returned by ReadFrame / WriteFrame. ErrFrameTooLarge is the
// only one a server should respond to with a protocol close — the rest
// indicate the peer has already gone away.
var (
	ErrFrameTooLarge = errors.New("toolbox frame: payload exceeds 1 MiB cap")
	ErrShortHeader   = errors.New("toolbox frame: short read on 5-byte header")
	ErrShortPayload  = errors.New("toolbox frame: short read on payload")
)

// WriteFrame emits one frame to w. Returns the bytes written (header +
// payload) so callers can reason about backpressure if they need to.
func WriteFrame(w io.Writer, channel byte, payload []byte) (int, error) {
	if len(payload) > MaxFrameLen {
		return 0, ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = channel
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return 0, err
	}
	if len(payload) == 0 {
		return 5, nil
	}
	n, err := w.Write(payload)
	return 5 + n, err
}

// ReadFrame reads one frame from r and returns its channel byte and
// payload bytes. The payload slice is freshly allocated per call —
// callers are free to retain it. On clean EOF (zero bytes consumed)
// returns io.EOF; on partial-header EOF returns ErrShortHeader.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	n, err := io.ReadFull(r, hdr[:])
	if err != nil {
		if err == io.EOF && n == 0 {
			return 0, nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return 0, nil, ErrShortHeader
		}
		return 0, nil, err
	}
	channel := hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > MaxFrameLen {
		return 0, nil, fmt.Errorf("%w: peer sent %d bytes", ErrFrameTooLarge, length)
	}
	if length == 0 {
		return channel, nil, nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.ErrUnexpectedEOF {
			return 0, nil, ErrShortPayload
		}
		return 0, nil, err
	}
	return channel, payload, nil
}

// WriteExitFrame is a convenience wrapper for the agent's exit emission.
// Always 4 bytes BE on channel ChannelExit. After sending this the
// agent should close the connection — there are no frames after EXIT.
func WriteExitFrame(w io.Writer, code int32) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(code))
	_, err := WriteFrame(w, ChannelExit, payload[:])
	return err
}

// ParseExitPayload decodes the 4-byte payload of a ChannelExit frame.
// Returns an error if len(payload) != 4 — useful for clients that want
// to assert wire-format compliance from the agent.
func ParseExitPayload(payload []byte) (int32, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("toolbox exit frame: expected 4-byte payload, got %d", len(payload))
	}
	return int32(binary.BigEndian.Uint32(payload)), nil
}
