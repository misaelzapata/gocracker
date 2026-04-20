package agent

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrame_RoundTrip_AllChannels(t *testing.T) {
	cases := []struct {
		name    string
		channel byte
		payload []byte
	}{
		{"stdin small", ChannelStdin, []byte("hello\n")},
		{"stdout binary", ChannelStdout, []byte{0x00, 0xff, 0x7f, 0x80, '\n', '\r', 0x01}},
		{"stderr empty", ChannelStderr, nil},
		{"signal SIGTERM", ChannelSignal, []byte{15}},
		{"exit 0", ChannelExit, []byte{0, 0, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := WriteFrame(&buf, tc.channel, tc.payload)
			if err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			if want := 5 + len(tc.payload); n != want {
				t.Fatalf("WriteFrame returned %d, want %d", n, want)
			}
			ch, p, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if ch != tc.channel {
				t.Fatalf("channel: got %d, want %d", ch, tc.channel)
			}
			if !bytes.Equal(p, tc.payload) {
				t.Fatalf("payload: got %x, want %x", p, tc.payload)
			}
		})
	}
}

func TestFrame_StreamMultiple(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, ChannelStdout, []byte("first"))
	WriteFrame(&buf, ChannelStderr, []byte("second"))
	WriteExitFrame(&buf, 42)

	for _, want := range []struct {
		channel byte
		payload []byte
	}{
		{ChannelStdout, []byte("first")},
		{ChannelStderr, []byte("second")},
		{ChannelExit, []byte{0, 0, 0, 42}},
	} {
		ch, p, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if ch != want.channel || !bytes.Equal(p, want.payload) {
			t.Fatalf("got ch=%d payload=%x; want ch=%d payload=%x", ch, p, want.channel, want.payload)
		}
	}
	if _, _, err := ReadFrame(&buf); err != io.EOF {
		t.Fatalf("expected EOF after exit frame, got %v", err)
	}
}

func TestFrame_AtCap(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, MaxFrameLen)
	var buf bytes.Buffer
	if _, err := WriteFrame(&buf, ChannelStdout, payload); err != nil {
		t.Fatalf("write at cap: %v", err)
	}
	ch, got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read at cap: %v", err)
	}
	if ch != ChannelStdout || !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch at cap (len=%d)", len(got))
	}
}

func TestFrame_OverCapWriteRejects(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, MaxFrameLen+1)
	var buf bytes.Buffer
	if _, err := WriteFrame(&buf, ChannelStdout, payload); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("write rejected but %d bytes still emitted", buf.Len())
	}
}

func TestFrame_OverCapReadRejects(t *testing.T) {
	// Hand-crafted header claiming MaxFrameLen+1 — we never actually
	// allocate the payload because the size check rejects first. This
	// is the defense against a hostile peer pinning memory.
	var buf bytes.Buffer
	buf.WriteByte(ChannelStdout)
	// length = MaxFrameLen+1 in big-endian
	buf.Write([]byte{0x00, 0x10, 0x00, 0x01})
	_, _, err := ReadFrame(&buf)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestFrame_ShortHeader(t *testing.T) {
	// 3 bytes — header is truncated mid-stream (peer crashed).
	r := bytes.NewReader([]byte{ChannelStdout, 0x00, 0x00})
	if _, _, err := ReadFrame(r); !errors.Is(err, ErrShortHeader) {
		t.Fatalf("expected ErrShortHeader, got %v", err)
	}
}

func TestFrame_ShortPayload(t *testing.T) {
	// Header says 10 bytes of payload, only 3 follow.
	var buf bytes.Buffer
	buf.WriteByte(ChannelStdin)
	buf.Write([]byte{0x00, 0x00, 0x00, 0x0A}) // length=10
	buf.WriteString("abc")                    // only 3 bytes
	if _, _, err := ReadFrame(&buf); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("expected ErrShortPayload, got %v", err)
	}
}

func TestFrame_CleanEOFOnEmpty(t *testing.T) {
	// Empty reader: caller should see io.EOF, NOT ErrShortHeader.
	// This is how the read loop knows the peer closed cleanly.
	if _, _, err := ReadFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("expected io.EOF on empty reader, got %v", err)
	}
}

func TestExit_RoundTrip(t *testing.T) {
	cases := []int32{0, 1, 42, 127, 137, -1}
	for _, code := range cases {
		var buf bytes.Buffer
		if err := WriteExitFrame(&buf, code); err != nil {
			t.Fatalf("WriteExitFrame(%d): %v", code, err)
		}
		ch, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if ch != ChannelExit {
			t.Fatalf("channel: got %d, want ChannelExit", ch)
		}
		got, err := ParseExitPayload(payload)
		if err != nil {
			t.Fatalf("ParseExitPayload: %v", err)
		}
		if got != code {
			t.Fatalf("exit code: got %d, want %d", got, code)
		}
	}
}

func TestExit_ParsePayload_RejectsBadLength(t *testing.T) {
	for _, p := range [][]byte{nil, {0x00}, {0x00, 0x00, 0x00, 0x00, 0x00}} {
		if _, err := ParseExitPayload(p); err == nil {
			t.Fatalf("expected error for %d-byte payload", len(p))
		} else if !strings.Contains(err.Error(), "expected 4-byte payload") {
			t.Fatalf("unexpected error message: %v", err)
		}
	}
}
