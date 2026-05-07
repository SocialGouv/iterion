package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func newTestSealer(t *testing.T) *AESGCMSealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	s, err := NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	return s
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := newTestSealer(t)
	plaintext := []byte("sk-ant-api03-redacted-very-secret")
	aad := []byte("api_key:abc-123")

	sealed, err := s.Seal(plaintext, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(sealed) <= len(plaintext) {
		t.Fatalf("sealed length %d <= plaintext %d (no auth tag?)", len(sealed), len(plaintext))
	}
	if sealed[0] != sealedVersionV1 {
		t.Fatalf("expected version %x, got %x", sealedVersionV1, sealed[0])
	}

	recovered, err := s.Open(sealed, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(recovered, plaintext) {
		t.Fatalf("plaintext mismatch: got %q want %q", recovered, plaintext)
	}
}

func TestSealNonceUniqueness(t *testing.T) {
	s := newTestSealer(t)
	pt := []byte("same-plaintext")
	a, _ := s.Seal(pt, nil)
	b, _ := s.Seal(pt, nil)
	if bytes.Equal(a, b) {
		t.Fatalf("identical sealed outputs for same plaintext (nonce reused?)")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	s := newTestSealer(t)
	sealed, err := s.Seal([]byte("hello"), []byte("ctx"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip a byte in the ciphertext.
	tampered := make([]byte, len(sealed))
	copy(tampered, sealed)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := s.Open(tampered, []byte("ctx")); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}
}

func TestOpenRejectsWrongAAD(t *testing.T) {
	s := newTestSealer(t)
	sealed, _ := s.Seal([]byte("payload"), []byte("ctx-A"))
	if _, err := s.Open(sealed, []byte("ctx-B")); err == nil {
		t.Fatal("Open accepted wrong AAD")
	}
}

func TestOpenRejectsShortInput(t *testing.T) {
	s := newTestSealer(t)
	if _, err := s.Open([]byte{0x01, 0x00}, nil); err != ErrSealedFormat {
		t.Fatalf("expected ErrSealedFormat, got %v", err)
	}
}

func TestOpenRejectsUnknownVersion(t *testing.T) {
	s := newTestSealer(t)
	sealed, _ := s.Seal([]byte("payload"), nil)
	sealed[0] = 0xff
	if _, err := s.Open(sealed, nil); err != ErrSealedVersion {
		t.Fatalf("expected ErrSealedVersion, got %v", err)
	}
}

func TestNewAESGCMSealer_KeyLength(t *testing.T) {
	if _, err := NewAESGCMSealer(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
	if _, err := NewAESGCMSealer(make([]byte, 32)); err != nil {
		t.Fatalf("32-byte key rejected: %v", err)
	}
}

func TestNewAESGCMSealerFromBase64(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	for name, encoded := range map[string]string{
		"std":     base64.StdEncoding.EncodeToString(key),
		"raw":     base64.RawStdEncoding.EncodeToString(key),
		"url":     base64.URLEncoding.EncodeToString(key),
		"raw_url": base64.RawURLEncoding.EncodeToString(key),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := NewAESGCMSealerFromBase64(encoded)
			if err != nil {
				t.Fatalf("NewAESGCMSealerFromBase64(%q): %v", name, err)
			}
			pt := []byte("hello")
			sealed, _ := s.Seal(pt, nil)
			got, err := s.Open(sealed, nil)
			if err != nil || !bytes.Equal(got, pt) {
				t.Fatalf("roundtrip failed: %v", err)
			}
		})
	}

	if _, err := NewAESGCMSealerFromBase64("not_base64$$$"); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestLast4(t *testing.T) {
	cases := map[string]string{
		"":                  "****",
		"short":             "****",
		"abcdefgh":          "efgh",
		"sk-ant-api03-XYZW": "XYZW",
	}
	for input, want := range cases {
		if got := Last4(input); got != want {
			t.Errorf("Last4(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFingerprintIsStableAndShort(t *testing.T) {
	a := FingerprintSHA256("hello")
	b := FingerprintSHA256("hello")
	if a != b {
		t.Fatalf("fingerprint not stable: %s vs %s", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("fingerprint length %d, want 16", len(a))
	}
	if FingerprintSHA256("hello") == FingerprintSHA256("world") {
		t.Fatalf("fingerprint collision on different inputs")
	}
	// Make sure it's hex (no underscore / slash).
	if strings.ContainsAny(a, "_/+= ") {
		t.Fatalf("fingerprint not hex: %s", a)
	}
}
