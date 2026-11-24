package connection

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The forge credential half of the #2760 contract: the token blob sealed on
// ForgeConnection.SealedPayload, and the only paths that seal or open it.
//
// The AAD is `forge_conn:<id>` and STAYS `forge_conn:<id>` — ADR-098's
// expand-and-contract preserves it, so a payload sealed by any build before
// the contract opens under this one and no record needs re-sealing. The blob
// shape is likewise unchanged. There is deliberately no second AAD to fall
// back to: an open either matches the one AAD every record was written under
// or it fails, and a failure names the connection rather than guessing.

// errNilForgeSealer is returned by every seal/open helper in this file when
// called without a configured secrets.Sealer.
var errNilForgeSealer = errors.New("forge: nil sealer")

// ForgeCredential is the token blob sealed on ForgeConnection.SealedPayload.
// It is opened on the paths that spend or renew the credential — the
// orchestrator, the refresh worker, the admin-client factory — which use the
// token in memory and re-seal.
//
// Exported because the forge BEHAVIOUR lives in pkg/forge while the model
// lives here: the refresh worker rotates these tokens, which a sealed blob
// it cannot open would make impossible. This is the forge legacy delivery
// ADR-098 keeps explicitly separate from the execution-only connector
// credential (credential.go) — a ForgeCredential DOES become a managed
// workflow secret, and nothing here may be reused for a generic connector
// connection.
type ForgeCredential struct {
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
	// ExpiresAt mirrors ForgeConnection.AccessTokenExpiresAt; kept here too so
	// an opened blob is self-describing. Zero = non-expiring.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// AdminToken returns the token the admin client authenticates with — the
// PAT for KindPAT, else the access token.
func (c ForgeCredential) AdminToken() string {
	if c.PATToken != "" {
		return c.PATToken
	}
	return c.AccessToken
}

// SealForgePAT seals a personal access token into a connection payload. Used
// by the server's PAT-connect handler (which holds the sealer but not the
// internal payload shape).
func SealForgePAT(sealer secrets.Sealer, connID, pat string) ([]byte, error) {
	return SealForgeCredential(sealer, connID, ForgeCredential{PATToken: pat})
}

// SealForgeOAuthTokens seals an OAuth access/refresh token pair into a
// connection payload. expiresAt may be zero for non-expiring tokens.
func SealForgeOAuthTokens(sealer secrets.Sealer, connID, accessToken, refreshToken string, expiresAt time.Time) ([]byte, error) {
	return SealForgeCredential(sealer, connID, ForgeCredential{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
	})
}

// ForgeAdminToken opens a connection's sealed payload and returns the token
// its admin client should authenticate with (PAT or access token). Used by
// the server's admin-client factory.
func ForgeAdminToken(sealer secrets.Sealer, conn ForgeConnection) (string, error) {
	sec, err := OpenForgeCredential(sealer, conn.ID, conn.SealedPayload)
	if err != nil {
		return "", err
	}
	return sec.AdminToken(), nil
}

// forgeConnAAD binds a sealed connection blob to its record id so a sealed
// payload cannot be silently transplanted to another connection (same
// convention as secrets.genericSecretAAD / webhooks.hmacSecretAAD).
//
// The value is part of the persisted format — changing it would strand every
// existing credential, which is exactly what the #2760 contract is forbidden
// from doing.
func forgeConnAAD(connID string) []byte {
	return []byte("forge_conn:" + connID)
}

// SealForgeCredential seals a credential blob under the connection's AAD.
// The refresh worker uses it to write back a renewed token pair.
func SealForgeCredential(sealer secrets.Sealer, connID string, cred ForgeCredential) ([]byte, error) {
	if sealer == nil {
		return nil, errNilForgeSealer
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return nil, fmt.Errorf("forge: marshal connection secret: %w", err)
	}
	return sealer.Seal(raw, forgeConnAAD(connID))
}

// OpenForgeCredential opens a sealed connection blob. A payload sealed under
// a different id fails here — the AAD is what makes a transplant visible.
func OpenForgeCredential(sealer secrets.Sealer, connID string, sealed []byte) (ForgeCredential, error) {
	if sealer == nil {
		return ForgeCredential{}, errNilForgeSealer
	}
	raw, err := sealer.Open(sealed, forgeConnAAD(connID))
	if err != nil {
		return ForgeCredential{}, fmt.Errorf("forge: open connection secret: %w", err)
	}
	var cred ForgeCredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return ForgeCredential{}, fmt.Errorf("forge: unmarshal connection secret: %w", err)
	}
	return cred, nil
}
