package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TokenPrefix marks an iterion webhook token so it's recognisable in
// configs/logs (the secret material follows the prefix).
const TokenPrefix = "iwh_"

// MintToken returns a fresh webhook token plaintext (shown to the
// operator exactly once) plus the at-rest fields persisted on a Config:
// a salted hash, the last 4 chars, and a fingerprint. Reuses the same
// random-token + hash primitives as operator session/invitation tokens.
//
// A webhook token is a team token (auth.MintTeamToken): it outlives the
// credential it is minted from, so the context must carry the derivation
// chokepoint's verdict for it — the gate stamped it after its ask granted —
// and a call whose context carries none is refused, whatever calls this.
func MintToken(ctx context.Context) (plaintext, hash, last4, fingerprint string, err error) {
	if _, err := auth.RequireMintVerdict(ctx, auth.MintTeamToken, "webhooks.MintToken"); err != nil {
		return "", "", "", "", err
	}
	tok, _, err := auth.GenerateRandomToken(32)
	if err != nil {
		return "", "", "", "", err
	}
	plaintext = TokenPrefix + tok
	return plaintext, auth.HashRefreshToken(plaintext), secrets.Last4(plaintext), secrets.FingerprintSHA256(plaintext), nil
}

// VerifyToken constant-time compares a presented token against a stored
// hash. Constant-time avoids leaking a near-miss via timing.
func VerifyToken(presented, storedHash string) bool {
	if presented == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(auth.HashRefreshToken(presented)), []byte(storedHash)) == 1
}

// hmacSecretAAD binds an HMAC-sealed blob to the webhook record id so a
// sealed plaintext cannot be silently transplanted to another config
// (same convention as generic secrets — see pkg/secrets/generic.go).
func hmacSecretAAD(webhookID string) []byte {
	return []byte("webhook_hmac_secret:" + webhookID)
}

// SealHMACSecret seals the iwh_ plaintext for an hmac-mode webhook, so
// the same value can later be used to recompute the body HMAC without
// keeping cleartext at rest. The plaintext is the same minted token the
// operator pastes into the forge's "secret" field.
func SealHMACSecret(sealer secrets.Sealer, webhookID, plaintext string) ([]byte, error) {
	if sealer == nil {
		return nil, secrets.ErrSealedFormat // surfaces as a server error; never reached in tests because the test sealer is always wired.
	}
	return sealer.Seal([]byte(plaintext), hmacSecretAAD(webhookID))
}

// VerifyHMACSignature recomputes HMAC-SHA256(body, plaintext) and
// constant-time compares the hex digest against the presented value.
// The presented value MAY carry a `sha256=` prefix (GitHub convention);
// we strip it before decoding. Malformed or empty inputs (no sealed
// secret, no signature, non-hex digest, length mismatch) return false
// so the caller can use this as a single boolean gate; it never panics.
func VerifyHMACSignature(sealer secrets.Sealer, webhookID string, sealed, body []byte, signatureHex string) bool {
	if sealer == nil || len(sealed) == 0 || signatureHex == "" {
		return false
	}
	plaintext, err := sealer.Open(sealed, hmacSecretAAD(webhookID))
	if err != nil || len(plaintext) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, plaintext)
	mac.Write(body)
	expected := mac.Sum(nil)

	// Strip the GitHub-style "sha256=" prefix so callers can pass the
	// header value verbatim.
	presented := strings.TrimPrefix(signatureHex, "sha256=")
	decoded, err := hex.DecodeString(presented)
	if err != nil || len(decoded) != len(expected) {
		return false
	}
	return hmac.Equal(decoded, expected)
}
