// Package pat implements personal access tokens — long-lived bearer
// credentials for programmatic API access (CI jobs, SDKs, curl)
// where the 15-minute JWT + refresh-cookie dance is impractical.
//
// Security model (v1): a PAT authenticates AS its owning user — it
// inherits the user's role (including super-admin) within the
// resolved team. There are no granular scopes yet (v2); mitigations
// are the optional team pin, the optional expiry, ITERION_PAT_MAX_TTL,
// instant revocation — of one token, or of every token of the account
// by "sign out everywhere" and an admin disable — and audit rows on
// create/revoke/use. A PAT mints no credential of its own account but a
// WebSocket ticket: it never switches team or org, never mints another PAT,
// links no SSO identity and resets no password of its own account
// (auth.Service.CheckDerivation, which also reads its row at every route it
// gates), so the pin and the expiry bound every credential it derives. A
// super-admin's token keeps the platform-admin routes, whose acts on other
// accounts — a temporary password included — outlive it.
package pat

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TokenPrefix marks an iterion personal access token. The middleware
// branches on it to pick PAT verification over JWT parsing.
const TokenPrefix = "iap_"

// Token is one personal access token at rest. The plaintext is shown
// exactly once at mint; only the hash is persisted.
type Token struct {
	ID          string `bson:"_id" json:"id"`
	UserID      string `bson:"user_id" json:"user_id"`
	Name        string `bson:"name" json:"name"`
	TokenHash   string `bson:"token_hash" json:"-"`
	TokenLast4  string `bson:"token_last4" json:"token_last4"`
	Fingerprint string `bson:"fingerprint,omitempty" json:"fingerprint,omitempty"`
	// TeamID optionally pins the token to one team: requests
	// authenticate with that team active (membership re-checked at
	// every use). Empty → the user's default team.
	TeamID     string     `bson:"team_id,omitempty" json:"team_id,omitempty"`
	CreatedAt  time.Time  `bson:"created_at" json:"created_at"`
	ExpiresAt  *time.Time `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
	LastUsedAt *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
}

// Usable reports whether the token can authenticate right now.
func (t Token) Usable(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
		return false
	}
	return true
}

// Sentinel errors.
var (
	ErrNotFound = errors.New("pat: not found")
)

// Store persists tokens. Implementations: MongoStore (production)
// and MemoryStore (tests/local). Keep semantics in lock-step.
type Store interface {
	Create(ctx context.Context, t Token) error
	// GetByTokenHash is the auth-path lookup (no user scoping — the
	// hash IS the credential).
	GetByTokenHash(ctx context.Context, hash string) (Token, error)
	Get(ctx context.Context, id string) (Token, error)
	ListByUser(ctx context.Context, userID string) ([]Token, error)
	// Revoke marks the token unusable; rows are kept for audit.
	Revoke(ctx context.Context, id string, at time.Time) error
	// RevokeAllByUser revokes every token of userID not revoked yet — "sign
	// out everywhere" and an admin disable — and returns how many it
	// revoked. A token already revoked keeps its first revocation instant.
	RevokeAllByUser(ctx context.Context, userID string, at time.Time) (int64, error)
	MarkUsed(ctx context.Context, id string, at time.Time) error
}

// MintToken returns a fresh PAT plaintext (shown once) plus the
// at-rest fields. Same primitives as webhook tokens.
func MintToken() (plaintext, hash, last4, fingerprint string, err error) {
	tok, _, err := auth.GenerateRandomToken(32)
	if err != nil {
		return "", "", "", "", err
	}
	plaintext = TokenPrefix + tok
	return plaintext, auth.HashRefreshToken(plaintext), secrets.Last4(plaintext), secrets.FingerprintSHA256(plaintext), nil
}

// HashToken maps a presented plaintext to its storage hash.
func HashToken(presented string) string { return auth.HashRefreshToken(presented) }

// VerifyToken constant-time compares a presented token against a
// stored hash.
func VerifyToken(presented, storedHash string) bool {
	if presented == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(auth.HashRefreshToken(presented)), []byte(storedHash)) == 1
}
