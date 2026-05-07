// Package secrets seals and unseals sensitive values (BYOK API keys,
// OAuth credentials, OIDC client secrets) at rest. The Sealer
// interface lets us swap the AES-GCM master-key implementation for a
// KMS-backed one later without touching call sites.
//
// Wire format of an AES-GCM sealed blob (single byte version prefix
// for forward compatibility):
//
//	v1: 0x01 | nonce(12) | ciphertext+tag
//
// Authenticated additional data (AAD) binds the ciphertext to a
// caller-supplied context string (e.g. "api_key:<id>") so a sealed
// value cannot be silently moved between records.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Sealer seals and opens secret payloads. Implementations MUST provide
// authenticated encryption: tampering with sealed bytes must surface
// as an Open error, never as silently-corrupted plaintext.
type Sealer interface {
	// Seal returns a sealed blob. AAD is optional context binding;
	// pass the same value to Open.
	Seal(plaintext, aad []byte) ([]byte, error)

	// Open returns the plaintext or an error. AAD must match the
	// value supplied at Seal.
	Open(sealed, aad []byte) ([]byte, error)
}

// Errors returned by Open when the input is malformed, the master
// key is wrong, or the ciphertext was tampered with. All three look
// the same on the wire to avoid oracle attacks; we still tag them
// internally for logging.
var (
	ErrSealedFormat       = errors.New("secrets: invalid sealed format")
	ErrSealedVersion      = errors.New("secrets: unsupported sealed version")
	ErrSealedAuthenticate = errors.New("secrets: authentication failed")
)

// sealedVersionV1 is the current wire-format version. New versions
// must come with a separate code path so we can migrate transparently
// (Open accepts any known version; Seal always emits the latest).
const sealedVersionV1 byte = 0x01

const aesGCMNonceSize = 12

// AESGCMSealer is an AES-256-GCM implementation of Sealer driven by
// a single master key. The master key MUST be 32 bytes; supply it
// via NewAESGCMSealer or NewAESGCMSealerFromBase64.
type AESGCMSealer struct {
	aead cipher.AEAD
}

// NewAESGCMSealer constructs a sealer from a 32-byte master key.
// The key is consumed immediately by aes.NewCipher; callers should
// avoid retaining the slice elsewhere.
func NewAESGCMSealer(masterKey []byte) (*AESGCMSealer, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("secrets: master key must be 32 bytes, got %d", len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher.NewGCM: %w", err)
	}
	if aead.NonceSize() != aesGCMNonceSize {
		// Defensive: the stdlib uses 12 bytes for GCM, but pin it
		// so a future change cannot silently break the wire format.
		return nil, fmt.Errorf("secrets: GCM nonce size %d != %d", aead.NonceSize(), aesGCMNonceSize)
	}
	return &AESGCMSealer{aead: aead}, nil
}

// NewAESGCMSealerFromBase64 decodes a standard or URL-safe base64
// master key (with or without padding) and constructs a sealer.
// Suitable for reading directly from ITERION_SECRETS_KEY.
func NewAESGCMSealerFromBase64(b64 string) (*AESGCMSealer, error) {
	key, err := DecodeBase64Lenient(b64)
	if err != nil {
		return nil, fmt.Errorf("secrets: decode master key: %w", err)
	}
	return NewAESGCMSealer(key)
}

// DecodeBase64Lenient accepts std/URL/raw variants of base64. Exposed
// for callers that need to decode an operator-supplied key (notably
// pkg/auth's JWT secret loader).
func DecodeBase64Lenient(b64 string) ([]byte, error) {
	return decodeBase64Lenient(b64)
}

// Seal returns version|nonce|ciphertext (with appended GCM tag).
func (s *AESGCMSealer) Seal(plaintext, aad []byte) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errors.New("secrets: nil sealer")
	}
	nonce := make([]byte, aesGCMNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: rand: %w", err)
	}
	out := make([]byte, 0, 1+aesGCMNonceSize+len(plaintext)+s.aead.Overhead())
	out = append(out, sealedVersionV1)
	out = append(out, nonce...)
	out = s.aead.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// Open verifies and decrypts a sealed blob.
func (s *AESGCMSealer) Open(sealed, aad []byte) ([]byte, error) {
	if s == nil || s.aead == nil {
		return nil, errors.New("secrets: nil sealer")
	}
	if len(sealed) < 1+aesGCMNonceSize+s.aead.Overhead() {
		return nil, ErrSealedFormat
	}
	if sealed[0] != sealedVersionV1 {
		return nil, ErrSealedVersion
	}
	nonce := sealed[1 : 1+aesGCMNonceSize]
	ct := sealed[1+aesGCMNonceSize:]
	pt, err := s.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrSealedAuthenticate
	}
	return pt, nil
}

// decodeBase64Lenient accepts std/URL/raw variants of base64. We allow
// all of them so an operator pasting a key from any source works.
func decodeBase64Lenient(b64 string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if v, err := enc.DecodeString(b64); err == nil {
			return v, nil
		}
	}
	return nil, errors.New("not valid base64")
}

// Last4 returns the last four characters of a secret for display. If
// the input is shorter than 8, it returns "****" — never reveal more
// than half of a short value.
func Last4(secret string) string {
	if len(secret) < 8 {
		return "****"
	}
	return secret[len(secret)-4:]
}

// FingerprintSHA256 returns a stable 16-char hex fingerprint of a
// secret value. Useful for logs that need to correlate two records
// using the same key without ever revealing the secret.
func FingerprintSHA256(secret string) string {
	return fingerprintHex(secret)
}
