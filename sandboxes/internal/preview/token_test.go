package preview

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

func newSignerForTest(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner([]byte("test-key-must-be-at-least-32-bytes-long-yes"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestNewSigner_RequiresMinKeyLen(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Error("NewSigner with short key should error")
	}
	if _, err := NewSigner(make([]byte, 32)); err != nil {
		t.Errorf("NewSigner with 32-byte key should succeed, got %v", err)
	}
}

func TestSignVerify_RoundTrip(t *testing.T) {
	s := newSignerForTest(t)
	payload := TokenPayload{
		SandboxID: "sb-abc123",
		Port:      3000,
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	tok := s.Sign(payload)
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.SandboxID != payload.SandboxID || got.Port != payload.Port {
		t.Errorf("got %+v, want %+v", got, payload)
	}
	// ExpiresAt round-trip via Unix() loses sub-second precision.
	if got.ExpiresAt.Unix() != payload.ExpiresAt.Unix() {
		t.Errorf("exp mismatch: got %v, want %v", got.ExpiresAt, payload.ExpiresAt)
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	s := newSignerForTest(t)
	tok := s.Sign(TokenPayload{
		SandboxID: "sb-x",
		Port:      80,
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	})
	if _, err := s.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expired token Verify err=%v, want ErrInvalidToken", err)
	}
}

func TestVerify_NoExpiryAllowed(t *testing.T) {
	// Zero ExpiresAt = no expiry. Useful for tests / dev tokens.
	s := newSignerForTest(t)
	tok := s.Sign(TokenPayload{SandboxID: "sb-x", Port: 80})
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("zero-exp token should be valid forever, got err=%v", err)
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("got non-zero ExpiresAt for zero-exp token: %v", got.ExpiresAt)
	}
}

func TestVerify_RejectsWrongSignature(t *testing.T) {
	s1 := newSignerForTest(t)
	s2, _ := NewSigner([]byte("a-different-32-byte-secret-key-here-yes"))
	tok := s1.Sign(TokenPayload{SandboxID: "sb-x", Port: 80, ExpiresAt: time.Now().Add(1 * time.Hour)})
	if _, err := s2.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("cross-signer Verify err=%v, want ErrInvalidToken", err)
	}
}

func TestVerify_RejectsTamperedPayload(t *testing.T) {
	s := newSignerForTest(t)
	tok := s.Sign(TokenPayload{SandboxID: "sb-x", Port: 80, ExpiresAt: time.Now().Add(1 * time.Hour)})
	// Flip a single base64 char in the payload segment.
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		t.Fatal("token didn't split as expected")
	}
	tampered := flipFirstChar(parts[0]) + "." + parts[1]
	if _, err := s.Verify(tampered); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("tampered Verify err=%v, want ErrInvalidToken", err)
	}
}

func TestVerify_RejectsTamperedSignature(t *testing.T) {
	s := newSignerForTest(t)
	tok := s.Sign(TokenPayload{SandboxID: "sb-x", Port: 80, ExpiresAt: time.Now().Add(1 * time.Hour)})
	parts := strings.SplitN(tok, ".", 2)
	tampered := parts[0] + "." + flipFirstChar(parts[1])
	if _, err := s.Verify(tampered); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("tampered-sig Verify err=%v, want ErrInvalidToken", err)
	}
}

func TestVerify_RejectsMalformedToken(t *testing.T) {
	s := newSignerForTest(t)
	cases := []string{
		"",
		"no-dot-separator",
		"....",
		".onlysig",
		"onlypayload.",
		"!!!!.@@@@",  // invalid base64
		"abcd.~~~~",  // invalid base64 in sig
	}
	for _, tok := range cases {
		t.Run(tok, func(t *testing.T) {
			if _, err := s.Verify(tok); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Verify(%q) err=%v, want ErrInvalidToken", tok, err)
			}
		})
	}
}

func TestVerify_RejectsMagicMismatch(t *testing.T) {
	// A token signed with a DIFFERENT magic (e.g. a future v2 token
	// being verified by a v1 signer) should fail at the parseCanonical
	// step, not at signature compare.
	s := newSignerForTest(t)
	// Hand-craft a token with bogus magic.
	body := "wrong-magic:sb-x|80|0"
	encBody := enc.EncodeToString([]byte(body))
	// Sign it with the real signer so the signature DOES verify.
	mac := hmacSum(s.secret, []byte(body))
	tok := encBody + "." + enc.EncodeToString(mac)
	if _, err := s.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("magic-mismatch Verify err=%v, want ErrInvalidToken", err)
	}
}

// TestSign_DeterministicForSamePayload: HMAC is deterministic, so
// signing the same payload twice must produce the same token. Tests
// rely on this for cache-key shapes etc.
func TestSign_DeterministicForSamePayload(t *testing.T) {
	s := newSignerForTest(t)
	p := TokenPayload{SandboxID: "x", Port: 1, ExpiresAt: time.Unix(1234567890, 0)}
	if s.Sign(p) != s.Sign(p) {
		t.Error("Sign produced non-deterministic output")
	}
}

// helpers
func flipFirstChar(s string) string {
	if len(s) == 0 {
		return s
	}
	c := s[0]
	if c == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

// hmacSum is the same HMAC-SHA256 token.go uses internally. Defined
// here so the magic-mismatch test can hand-craft a token with a
// valid signature but a wrong-magic payload.
func hmacSum(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(msg)
	return mac.Sum(nil)
}
