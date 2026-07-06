package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// errNilSealer is returned by every seal/open helper in this package when
// called without a configured secrets.Sealer.
var errNilSealer = errors.New("forge: nil sealer")

// SealPAT seals a personal access token into a connection payload. Used by
// the server's PAT-connect handler (which holds the sealer but not the
// internal payload shape).
func SealPAT(sealer secrets.Sealer, connID, pat string) ([]byte, error) {
	return sealConnectionSecret(sealer, connID, connectionSecret{PATToken: pat})
}

// SealOAuthTokens seals an OAuth access/refresh token pair into a
// connection payload. expiresAt may be zero for non-expiring tokens.
func SealOAuthTokens(sealer secrets.Sealer, connID, accessToken, refreshToken string, expiresAt time.Time) ([]byte, error) {
	return sealConnectionSecret(sealer, connID, connectionSecret{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
	})
}

// AdminTokenFor opens a connection's sealed payload and returns the token
// its admin client should authenticate with (PAT or access token). Used by
// the server's admin-client factory.
func AdminTokenFor(sealer secrets.Sealer, conn Connection) (string, error) {
	sec, err := openConnectionSecret(sealer, conn.ID, conn.SealedPayload)
	if err != nil {
		return "", err
	}
	return sec.AdminToken(), nil
}

// connectionSecret is the token blob sealed on Connection.SealedPayload.
// It is never exposed outside this package — the orchestrator and refresh
// worker open it, use the token in memory, and re-seal.
type connectionSecret struct {
	// AccessToken is the OAuth-app user token or the GitHub-App installation
	// token. Empty for KindPAT.
	AccessToken string `json:"access_token,omitempty"`
	// RefreshToken renews AccessToken (OAuth apps that issue one). Empty for
	// PAT and GitHub-App (the App re-mints from its private key instead).
	RefreshToken string `json:"refresh_token,omitempty"`
	// PATToken is the operator-pasted personal access token. KindPAT only.
	PATToken string `json:"pat_token,omitempty"`
	// TokenType is the OAuth token type (usually "bearer"); informational.
	TokenType string `json:"token_type,omitempty"`
	// ExpiresAt mirrors Connection.AccessTokenExpiresAt; kept here too so an
	// opened blob is self-describing. Zero = non-expiring.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// AdminToken returns the token the admin client authenticates with — the
// PAT for KindPAT, else the access token.
func (s connectionSecret) AdminToken() string {
	if s.PATToken != "" {
		return s.PATToken
	}
	return s.AccessToken
}

// forgeConnAAD binds a sealed connection blob to its record id so a sealed
// payload cannot be silently transplanted to another connection (same
// convention as secrets.genericSecretAAD / webhooks.hmacSecretAAD).
func forgeConnAAD(connID string) []byte {
	return []byte("forge_conn:" + connID)
}

func sealConnectionSecret(sealer secrets.Sealer, connID string, sec connectionSecret) ([]byte, error) {
	if sealer == nil {
		return nil, errNilSealer
	}
	raw, err := json.Marshal(sec)
	if err != nil {
		return nil, fmt.Errorf("forge: marshal connection secret: %w", err)
	}
	return sealer.Seal(raw, forgeConnAAD(connID))
}

func openConnectionSecret(sealer secrets.Sealer, connID string, sealed []byte) (connectionSecret, error) {
	if sealer == nil {
		return connectionSecret{}, errNilSealer
	}
	raw, err := sealer.Open(sealed, forgeConnAAD(connID))
	if err != nil {
		return connectionSecret{}, fmt.Errorf("forge: open connection secret: %w", err)
	}
	var sec connectionSecret
	if err := json.Unmarshal(raw, &sec); err != nil {
		return connectionSecret{}, fmt.Errorf("forge: unmarshal connection secret: %w", err)
	}
	return sec, nil
}
