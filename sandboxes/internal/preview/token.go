// Package preview implements signed-URL previews for in-sandbox HTTP
// servers. Each token grants time-bounded access to ONE sandbox + ONE
// guest port via two equivalent paths:
//
//   - GET /previews/{token}                   — direct relay
//   - GET <sandbox-id>--<port>.sbx.localhost — subdomain relay
//                                                (requires the host
//                                                 to wildcard-match
//                                                 *.sbx.localhost to
//                                                 sandboxd's port)
//
// Tokens are HMAC-SHA256 over (sandbox_id|port|expires_unix). The
// payload is base64url-encoded; the signature follows. No JSON
// parsing in the verify path — the format is byte-stable so a
// truncated / mutated token fails fast at decode rather than at
// signature compare.
//
// This file (slice 1) defines the token format + Sign/Verify. The
// proxy handler that USES the token lands in slice 2.
package preview

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TokenPayload is the unsigned part of a preview token. Sign hashes
// the canonical string form; Verify recomputes from the same fields.
// Adding/removing a field is a breaking change — bump the magic
// prefix below to invalidate prior tokens cleanly.
type TokenPayload struct {
	SandboxID string
	Port      uint16
	ExpiresAt time.Time
}

// tokenMagic prefixes every signed payload so we can recognize a
// preview token at a glance and so a future format bump can roll
// over without ambiguity. The "v1" tag is part of the signed bytes,
// so changing it invalidates every prior token deterministically.
const tokenMagic = "gcsbxprev:v1:"

// ErrInvalidToken is returned by Verify for ANY parse / signature /
// expiry failure. Callers should NOT distinguish — leaking "expired
// vs invalid" lets attackers probe for valid sandbox IDs.
var ErrInvalidToken = errors.New("preview: invalid token")

// Signer wraps a 32-byte HMAC secret. Construct once per sandboxd
// process via NewSigner and reuse.
type Signer struct {
	secret []byte
}

// NewSigner builds a Signer from a key. Returns an error if the key
// is shorter than 32 bytes — too short means brute-forcable HMAC.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("preview: signer key must be ≥32 bytes, got %d", len(key))
	}
	// Defensive copy so callers can scrub their buffer.
	cp := make([]byte, len(key))
	copy(cp, key)
	return &Signer{secret: cp}, nil
}

// Sign returns a base64url-encoded token for the given payload. The
// token is single-use-friendly (Verify only checks expiry, not
// reuse) — callers that need replay protection layer their own
// nonce store on top.
func (s *Signer) Sign(p TokenPayload) string {
	canonical := canonicalString(p)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(canonical))
	sig := mac.Sum(nil)
	// Token = base64url(canonical_bytes) + "." + base64url(sig)
	// Two segments separated by "." — same shape as JWT for
	// familiarity, NOT JWT (no JSON, no algorithm field).
	return enc.EncodeToString([]byte(canonical)) + "." + enc.EncodeToString(sig)
}

// Verify decodes + signature-checks the token. Returns the payload
// on success or ErrInvalidToken on ANY failure (decode, signature,
// expiry, magic-prefix mismatch). The error is intentionally
// indistinguishable for all failure modes.
func (s *Signer) Verify(token string) (TokenPayload, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return TokenPayload{}, ErrInvalidToken
	}
	payloadBytes, err := enc.DecodeString(parts[0])
	if err != nil {
		return TokenPayload{}, ErrInvalidToken
	}
	sigBytes, err := enc.DecodeString(parts[1])
	if err != nil {
		return TokenPayload{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write(payloadBytes)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expectedSig) {
		return TokenPayload{}, ErrInvalidToken
	}
	// Signature ok — parse the canonical body.
	p, perr := parseCanonical(string(payloadBytes))
	if perr != nil {
		return TokenPayload{}, ErrInvalidToken
	}
	if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt) {
		return TokenPayload{}, ErrInvalidToken
	}
	return p, nil
}

// canonicalString is the byte-stable representation used for HMAC.
// Format: "<magic>:<sandbox_id>:<port>:<exp_unix>". Fields are
// pipe-separated to disambiguate from sandbox IDs that contain
// colons (they shouldn't, but defense in depth).
func canonicalString(p TokenPayload) string {
	exp := int64(0)
	if !p.ExpiresAt.IsZero() {
		exp = p.ExpiresAt.Unix()
	}
	return tokenMagic + p.SandboxID + "|" + strconv.FormatUint(uint64(p.Port), 10) + "|" + strconv.FormatInt(exp, 10)
}

func parseCanonical(s string) (TokenPayload, error) {
	if !strings.HasPrefix(s, tokenMagic) {
		return TokenPayload{}, ErrInvalidToken
	}
	body := strings.TrimPrefix(s, tokenMagic)
	parts := strings.Split(body, "|")
	if len(parts) != 3 {
		return TokenPayload{}, ErrInvalidToken
	}
	port, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil {
		return TokenPayload{}, ErrInvalidToken
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return TokenPayload{}, ErrInvalidToken
	}
	var expTime time.Time
	if exp != 0 {
		expTime = time.Unix(exp, 0)
	}
	return TokenPayload{SandboxID: parts[0], Port: uint16(port), ExpiresAt: expTime}, nil
}

// enc is the URL-safe base64 variant without padding — matches the
// shape of typical signed tokens (JWT, Fernet) so previews don't
// trip URL parsers that mishandle '+' / '/' / '='.
var enc = base64.RawURLEncoding
