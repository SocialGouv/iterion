// Package auth implements the multitenant authentication core:
// password hashing, JWT issuance/verification, refresh token
// rotation, and the per-request Identity carried through ctx.
//
// Subpackage pkg/auth/oidc owns the SSO connectors (Google, GitHub,
// generic OIDC).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Sized so a single hash takes ~50ms on modern
// server hardware. Tunable later via config without changing the
// stored hash format (parameters are encoded inline).
const (
	argonTimeCost    uint32 = 2
	argonMemoryCost  uint32 = 64 * 1024 // 64 MiB
	argonParallelism uint8  = 1
	argonSaltLen     int    = 16
	argonKeyLen      uint32 = 32
)

// ErrInvalidPasswordHash is returned by VerifyPassword when the
// stored hash is not in the encoded format produced by HashPassword.
var ErrInvalidPasswordHash = errors.New("auth: invalid password hash")

// HashPassword returns an argon2id PHC-style encoded hash of pw. The
// returned string is safe to store as-is (contains the salt and all
// parameters needed to verify later).
//
// Format: $argon2id$v=19$m=65536,t=2,p=1$<salt-b64>$<key-b64>
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: rand: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTimeCost, argonMemoryCost, argonParallelism, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemoryCost,
		argonTimeCost,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether pw matches the encoded hash. Uses
// constant-time compare on the derived key to avoid timing leaks.
func VerifyPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// Expected: ["", "argon2id", "v=19", "m=65536,t=2,p=1", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidPasswordHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrInvalidPasswordHash
	}
	var memory, time uint32
	var parallelism uint8
	if err := parseArgonParams(parts[3], &memory, &time, &parallelism); err != nil {
		return false, ErrInvalidPasswordHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidPasswordHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidPasswordHash
	}
	got := argon2.IDKey([]byte(pw), salt, time, memory, parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return true, nil
	}
	return false, nil
}

// parseArgonParams parses the "m=N,t=N,p=N" fragment inline.
func parseArgonParams(s string, m, t *uint32, p *uint8) error {
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("invalid param %q", kv)
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid number for %s: %w", k, err)
		}
		switch k {
		case "m":
			*m = uint32(n)
		case "t":
			*t = uint32(n)
		case "p":
			*p = uint8(n)
		default:
			return fmt.Errorf("unknown param %q", k)
		}
	}
	return nil
}

// GenerateRandomToken returns a base64-url-encoded n-byte secret
// suitable for invitation tokens, OAuth state, etc. Returns the raw
// bytes too so callers can hash or fingerprint without re-decoding.
func GenerateRandomToken(n int) (token string, raw []byte, err error) {
	raw = make([]byte, n)
	if _, err = rand.Read(raw); err != nil {
		return "", nil, err
	}
	return base64.RawURLEncoding.EncodeToString(raw), raw, nil
}
