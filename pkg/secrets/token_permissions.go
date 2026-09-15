package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"time"
)

// TokenPermissionProof records the provider's response for one minted token.
// Installation grants and requested mint options are not evidence of what the
// returned token carries. A replacement token cannot reuse this proof.
type TokenPermissionProof struct {
	SHA256      string            `json:"sha256" bson:"sha256"`
	Permissions map[string]string `json:"permissions" bson:"permissions"`
	ExpiresAt   time.Time         `json:"expires_at" bson:"expires_at"`
}

// TokenSHA256 is a full digest, unlike the short audit fingerprint.
func TokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func NewTokenPermissionProof(token string, permissions map[string]string, expires time.Time) *TokenPermissionProof {
	if token == "" || permissions == nil || expires.IsZero() {
		return nil
	}
	return &TokenPermissionProof{SHA256: TokenSHA256(token), Permissions: maps.Clone(permissions), ExpiresAt: expires}
}

// Allows requires both the token identity and a live, explicit write grant.
func (p *TokenPermissionProof) Allows(token, permission string, now time.Time) bool {
	return p != nil && token != "" && p.SHA256 == TokenSHA256(token) && now.Before(p.ExpiresAt) && p.Permissions[permission] == "write"
}
