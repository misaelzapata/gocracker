package main

import (
	"testing"
	"time"
)

func TestTranscriptStateTracksDiskPathAndPrompts(t *testing.T) {
	var state transcriptState
	state.feed([]byte("disk: /tmp/disk.ext4\n/ # "))
	diskPath, prompts, size := state.snapshot()
	if diskPath != "/tmp/disk.ext4" {
		t.Fatalf("diskPath = %q", diskPath)
	}
	if prompts == 0 || size == 0 {
		t.Fatalf("snapshot = (%q, %d, %d)", diskPath, prompts, size)
	}
}

func TestWaitForDiskPathAndReady(t *testing.T) {
	var state transcriptState
	state.feed([]byte("disk: /tmp/disk.ext4\n/ # "))
	if _, err := waitForDiskPath(&state, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForDiskPath() error = %v", err)
	}
	if _, err := waitForReady(&state, 400*time.Millisecond); err != nil {
		t.Fatalf("waitForReady() error = %v", err)
	}
}

func TestNormalizeInputAndFilter(t *testing.T) {
	if got := string(normalizeInput([]byte("a\r\nb\rc"))); got != "a\nb\nc" {
		t.Fatalf("normalizeInput() = %q", got)
	}

	var filter terminalQueryFilter
	payload, reply := filter.Filter([]byte("hello\x1b[6nworld"))
	if string(payload) != "helloworld" {
		t.Fatalf("payload = %q", payload)
	}
	if string(reply) != "\x1b[1;1R" {
		t.Fatalf("reply = %q", reply)
	}
}

func TestFilterCarriesPartialSequencesAndDropsReplies(t *testing.T) {
	var filter terminalQueryFilter
	payload, reply := filter.Filter([]byte("x\x1b["))
	if string(payload) != "x" || len(reply) != 0 {
		t.Fatalf("payload=%q reply=%q", payload, reply)
	}
	if tail := string(filter.Flush()); tail != "\x1b[" {
		t.Fatalf("Flush() = %q", tail)
	}
	if !shouldDropTerminalReply([]byte("\x1b[?2004h")) {
		t.Fatal("shouldDropTerminalReply() = false")
	}
	if shouldDropTerminalReply([]byte("plain")) {
		t.Fatal("shouldDropTerminalReply(plain) = true")
	}
}
