package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE encapsulates a Proof Key for Code Exchange pair (RFC 7636). The
// authorization request sends Challenge + Method; the token-exchange
// request sends Verifier so the AS can verify it hashes to Challenge.
type PKCE struct {
	Verifier  string // 43-128 unreserved-char string per RFC 7636
	Challenge string // base64url(SHA-256(Verifier)) without padding
	Method    string // always "S256" — plain is rejected by modern providers
}

// pkceVerifierLen is the byte length we feed to base64url. 32 bytes
// → 43 base64url chars, the lower bound RFC 7636 §4.1 mandates. Any
// less and most providers reject the request.
const pkceVerifierLen = 32

// NewPKCE generates a fresh code_verifier / code_challenge pair using
// SHA-256. Returns an error only if the OS RNG fails — every other
// failure mode in PKCE is a programming bug, not a runtime condition.
func NewPKCE() (*PKCE, error) {
	raw := make([]byte, pkceVerifierLen)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("pkce: read random: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		Method:    "S256",
	}, nil
}

// randomState returns a URL-safe random string suitable for the OAuth
// state parameter. We use 32 bytes → 43 base64url chars to match the
// PKCE verifier length. State is single-use and validated on callback
// to defend against CSRF.
func randomState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("pkce: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
