package oidc

import (
	"crypto/sha256"
	"encoding/base64"
)

// deriveS256 returns the PKCE S256 challenge derived from a verifier.
// Shared by all connectors that support PKCE.
func deriveS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
