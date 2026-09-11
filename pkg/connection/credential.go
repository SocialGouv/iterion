package connection

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The credential half of a connection: sealed at rest, opened on ONE path.
//
// "Execution-only" (ADR-098 decision 4) is a property of what this file does
// NOT export. A connector credential must never become a workflow secret, an
// environment variable, a mounted file or a prompt materialisation — which is
// exactly what `pkg/secrets`' managed-secret path does for forge tokens, and
// why reusing that path "as is" was refused.
//
// Enforcing it as a rule would mean a guard listing the spellings of a leak,
// which does not converge. Enforcing it structurally is one line of design:
// `openCredential` is unexported, and the only exported caller is the resolver,
// which hands the value straight to the HTTP executor inside a call. Making the
// token reachable anywhere else requires adding an exported function to this
// file — a visible, reviewable act, not an accident at a call site.

// errNilSealer is returned when a seal/open is attempted without one. A nil
// sealer is a wiring mistake, and treating it as "no encryption" would store
// tokens in cleartext at exactly the moment nobody is looking.
var errNilSealer = errors.New("connection: nil sealer")

// credentialBlob is what sits in Connection.SealedPayload. Unexported: its
// shape is this package's business, and a caller that could construct one
// could also hold a token.
type credentialBlob struct {
	// Token is the api key / bearer / personal access token.
	Token string `json:"token,omitempty"`
	// Username and Password serve HTTP basic.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// RefreshToken renews Token where the provider issues one.
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt mirrors Connection.ExpiresAt so an opened blob is
	// self-describing — a blob that travelled without its record still knows
	// when it dies.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// connectionAAD binds a sealed blob to its record id, so a payload cannot be
// transplanted onto another connection — the same convention as
// `forge_conn:<id>` and `generic_secret:<id>`.
//
// Without it, an attacker (or a bad migration) able to write a connection row
// could point a low-privilege connection's record at a high-privilege
// connection's sealed bytes, and the seal would open happily.
func connectionAAD(connID string) []byte {
	return []byte("connection:" + connID)
}

// SealToken seals a bearer/api-key/PAT credential onto a connection.
//
// Exported because CONNECTING is a legitimate operation from outside this
// package (a CLI command, a studio handler). Note the asymmetry with opening,
// which is not exported: writing a credential in is a user action; reading one
// out is only ever iterion making a call.
func SealToken(sealer secrets.Sealer, connID, token string, expiresAt time.Time) ([]byte, error) {
	return sealCredential(sealer, connID, credentialBlob{Token: token, ExpiresAt: expiresAt})
}

// SealBasic seals a username/password credential onto a connection.
func SealBasic(sealer secrets.Sealer, connID, username, password string) ([]byte, error) {
	return sealCredential(sealer, connID, credentialBlob{Username: username, Password: password})
}

func sealCredential(sealer secrets.Sealer, connID string, blob credentialBlob) ([]byte, error) {
	if sealer == nil {
		return nil, errNilSealer
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return nil, fmt.Errorf("connection: marshal credential: %w", err)
	}
	sealed, err := sealer.Seal(raw, connectionAAD(connID))
	if err != nil {
		return nil, fmt.Errorf("connection: seal credential: %w", err)
	}
	return sealed, nil
}

// openCredential is the ONE path from a stored connection to a usable secret.
// Unexported on purpose — see the file comment.
func openCredential(sealer secrets.Sealer, c Connection) (credentialBlob, error) {
	if sealer == nil {
		return credentialBlob{}, errNilSealer
	}
	if len(c.SealedPayload) == 0 {
		return credentialBlob{}, fmt.Errorf("connection %q (%s/%s) carries no credential", c.ID, c.Connector, c.Alias)
	}
	raw, err := sealer.Open(c.SealedPayload, connectionAAD(c.ID))
	if err != nil {
		return credentialBlob{}, fmt.Errorf("connection %q: open credential: %w", c.ID, err)
	}
	var blob credentialBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return credentialBlob{}, fmt.Errorf("connection %q: decode credential: %w", c.ID, err)
	}
	return blob, nil
}
