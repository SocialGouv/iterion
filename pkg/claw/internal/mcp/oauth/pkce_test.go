package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestPKCE_GeneratesValidVerifierAndChallenge(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if len(p.Verifier) < 43 || len(p.Verifier) > 128 {
		t.Errorf("verifier length %d outside RFC 7636 bounds [43,128]", len(p.Verifier))
	}
	if !isURLSafe(p.Verifier) {
		t.Errorf("verifier %q contains non-URL-safe chars", p.Verifier)
	}
	if p.Method != "S256" {
		t.Errorf("expected S256 method, got %q", p.Method)
	}
	// Re-derive challenge from verifier to confirm encoding.
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Errorf("challenge mismatch:\n  got  %s\n  want %s", p.Challenge, want)
	}
}

func TestPKCE_RandomStateIsUnique(t *testing.T) {
	a, err := randomState()
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomState()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("expected distinct state values, got duplicates: %s", a)
	}
	if len(a) < 32 {
		t.Errorf("state too short: %d", len(a))
	}
}

// isURLSafe checks for the unreserved character set per RFC 3986
// (which is exactly base64url alphabet sans padding).
func isURLSafe(s string) bool {
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}
