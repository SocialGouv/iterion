package configshare

import (
	"crypto/subtle"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TokenPrefix marks a config-share editor token in URLs / logs. The 32 random
// bytes follow the prefix; only the salted hash + last4 + fingerprint persist.
const TokenPrefix = "iws_"

// MintToken returns a fresh share token and the values persisted on the record
// (never the plaintext). Mirrors webhooks.MintToken so both self-authenticating
// surfaces share the same at-rest discipline.
func MintToken() (plaintext, hash, last4, fingerprint string, err error) {
	tok, _, err := auth.GenerateRandomToken(32)
	if err != nil {
		return "", "", "", "", fmt.Errorf("configshare: mint token: %w", err)
	}
	plaintext = TokenPrefix + tok
	return plaintext, auth.HashRefreshToken(plaintext), secrets.Last4(plaintext), secrets.FingerprintSHA256(plaintext), nil
}

// VerifyToken constant-time compares a presented token against a stored hash.
func VerifyToken(presented, storedHash string) bool {
	if presented == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(auth.HashRefreshToken(presented)), []byte(storedHash)) == 1
}
