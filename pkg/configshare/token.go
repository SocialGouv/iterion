package configshare

import (
	"context"
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
//
// A share token is a team token (auth.MintTeamToken): it outlives the
// credential it is minted from, so the context must carry the derivation
// chokepoint's verdict for it — the gate stamped it after its ask granted —
// and a call whose context carries none is refused, whatever calls this.
func MintToken(ctx context.Context) (plaintext, hash, last4, fingerprint string, err error) {
	if _, err := auth.RequireMintVerdict(ctx, auth.MintTeamToken, "configshare.MintToken"); err != nil {
		return "", "", "", "", err
	}
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
