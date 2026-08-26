// Package forge is iterion's OUTBOUND forge-integration layer: it
// connects a team to a GitLab / GitHub / Forgejo account (OAuth or PAT),
// lists that account's repos, and AUTO-PROVISIONS the forge-side webhook
// + the bot-secret binding when an operator enables a bot on a repo —
// replacing the manual PAT→secret→binding→webhook→forge-hook chain.
//
// It is the complement of pkg/webhooks (iterion's INBOUND receiver):
// pkg/webhooks authenticates deliveries the forge sends TO iterion;
// pkg/forge holds the admin credential that lets iterion call OUT to the
// forge to register that delivery in the first place.
//
// Separation of concerns (load-bearing):
//   - The connection's admin token (OAuth user token / GitHub-App
//     installation token / PAT) lives sealed on a forge.Connection and is
//     used only to manage iterion's footprint on the forge (create hooks,
//     list repos). It is NEVER the token a bot posts with.
//   - The bot-runtime forge token (what review-pr posts comments with)
//     stays a secrets.GenericSecret + secrets.BotSecretBinding. The
//     orchestrator DERIVES a managed generic secret from the connection,
//     so the entire downstream (ResolveGenericWithBindings → RunBundle →
//     /run/iterion/secrets/forge_token → glab/gh) is unchanged.
package forge

import (
	"errors"
	"strings"
	"time"
)

// Provider identifies the forge a connection targets. Mirrors
// webhooks.Provider (minus "generic", which has no admin API).
type Provider string

const (
	ProviderGitLab  Provider = "gitlab"
	ProviderGitHub  Provider = "github"
	ProviderForgejo Provider = "forgejo" // same wire shape as Gitea
)

// Valid reports whether p is one of the supported forges.
func (p Provider) Valid() bool {
	switch p {
	case ProviderGitLab, ProviderGitHub, ProviderForgejo:
		return true
	}
	return false
}

// Kind is how a connection authenticates to the forge.
type Kind string

const (
	// KindOAuthApp is an OAuth-app user token (GitLab / GitHub OAuth App /
	// Forgejo). Refreshable when the provider issues a refresh token.
	KindOAuthApp Kind = "oauth_app"
	// KindGitHubApp is a GitHub-App installation token (short-lived, minted
	// on demand from the App private key + installation id).
	KindGitHubApp Kind = "github_app"
	// KindPAT is an operator-pasted personal access token (the fallback for
	// self-hosted instances with no registrable OAuth app). Never refreshed.
	KindPAT Kind = "pat"
)

// ConnectionStatus is the health of a connection's admin credential.
type ConnectionStatus string

const (
	StatusActive      ConnectionStatus = "active"
	StatusNeedsReauth ConnectionStatus = "needs_reauth" // refresh failed; operator must reconnect
	StatusRevoked     ConnectionStatus = "revoked"      // token rejected by the forge (401)
	// StatusDegraded marks a connection whose token mint fails on a
	// PERMANENT configuration mismatch (a GitHub-App install approved with a
	// narrower permission set than iterion now requests → HTTP 422
	// "permissions not granted"). Unlike a transient failure this can never
	// self-heal by retrying, so the refresh worker stops re-minting it each
	// tick and records the actionable reason on StatusReason. Cleared back to
	// StatusActive by a successful reconnect/re-provision (same lifecycle as
	// StatusRevoked).
	StatusDegraded ConnectionStatus = "degraded"
)

// Connection is a team's authenticated link to one forge account. The
// admin token material lives sealed on SealedPayload (AAD bound to ID);
// the JSON encoder drops it so it never reaches the studio.
type Connection struct {
	ID          string   `bson:"_id" json:"id"`
	TenantID    string   `bson:"tenant_id" json:"tenant_id"`
	Provider    Provider `bson:"provider" json:"provider"`
	Kind        Kind     `bson:"kind" json:"kind"`
	DisplayName string   `bson:"display_name,omitempty" json:"display_name,omitempty"`

	// ForgeBaseURL pins the forge host this connection authenticates to,
	// e.g. "https://gitlab.example.com" for self-hosted. Empty = the
	// provider's canonical SaaS host (DefaultBaseURL). Canonicalised
	// (scheme+host, no trailing slash) at connect time and threaded onto
	// the auto-created webhooks.Config.ForgeBaseURL so the existing inbound
	// SSRF host-pin keeps applying.
	ForgeBaseURL string `bson:"forge_base_url,omitempty" json:"forge_base_url,omitempty"`

	// Connected identity / namespace, populated from WhoAmI at connect time.
	AccountLogin string `bson:"account_login,omitempty" json:"account_login,omitempty"`
	AccountID    string `bson:"account_id,omitempty" json:"account_id,omitempty"`
	Namespace    string `bson:"namespace,omitempty" json:"namespace,omitempty"`

	// GitHub-App specific (Kind == KindGitHubApp).
	InstallationID int64  `bson:"installation_id,omitempty" json:"installation_id,omitempty"`
	AppSlug        string `bson:"app_slug,omitempty" json:"app_slug,omitempty"`
	// InstallationAccount is the login of the account the App is INSTALLED
	// ON — the org. It is NOT AccountLogin, which for a github_app
	// connection is the App's own bot handle ("iterion-forge-xxxx[bot]"):
	// the connection authenticates AS the App, but operates ON the org.
	// Anything keyed by org (the security-read token map, which a bot's
	// config looks up by org name) MUST read this one.
	InstallationAccount string `bson:"installation_account,omitempty" json:"installation_account,omitempty"`
	// OAuthAppID points at the ForgeOAuthApp whose private key mints this
	// installation's tokens. It exists because a tenant may hold SEVERAL GitHub
	// Apps on one host (one per owning org — a private App is only installable
	// on its owner), so the app can no longer be re-derived from
	// (tenant, provider, host). Signing an installation token with the wrong
	// app's key does not degrade gracefully: it fails, or worse, addresses a
	// different installation entirely.
	//
	// Empty on connections created before this field existed; resolution falls
	// back to the legacy per-instance lookup, which is unambiguous for them
	// precisely because the old unique index allowed only one app per host.
	OAuthAppID string `bson:"oauth_app_id,omitempty" json:"oauth_app_id,omitempty"`
	// GrantedPermissions is the permission set the installation's owner
	// actually APPROVED, captured at install and refreshed by the health
	// probe. A minted token must ask only for permissions that exist, or the
	// whole mint 422s — so this is what makes one code path work for an App
	// created before delivery permissions existed and one created with them.
	// Empty = unknown (pre-dates this field); those keep the historical
	// baseline.
	GrantedPermissions map[string]string `bson:"granted_permissions,omitempty" json:"granted_permissions,omitempty"`

	// SecurityReadEnabled opts this github_app connection into the
	// security-read token flow: the refresh worker (and the enable endpoint)
	// mint an org-wide vulnerability_alerts:read installation token and
	// merge it into the tenant's `dependabot_tokens` generic secret
	// (SecurityReadSecretName) under the installation's org login — the
	// secret the vuln-watch bot reads. Off by default: alert data is
	// sensitive, so a team turns it on per connection, deliberately.
	SecurityReadEnabled bool `bson:"security_read_enabled,omitempty" json:"security_read_enabled,omitempty"`

	Status ConnectionStatus `bson:"status" json:"status"`

	// StatusReason is a human-readable explanation for a non-active Status
	// (e.g. which permission a degraded GitHub-App install is missing and
	// that an org admin must re-approve or remove the connection). Surfaced
	// to the operator so the fix is actionable; cleared on reconnect.
	StatusReason string `bson:"status_reason,omitempty" json:"status_reason,omitempty"`

	// SealedPayload holds the token blob (access/refresh/PAT + expiry),
	// sealed via secrets.Sealer with AAD "forge_conn:<ID>". Never serialised.
	SealedPayload []byte `bson:"sealed_payload" json:"-"`

	// AccessTokenExpiresAt is stored in the CLEAR (not in the sealed blob)
	// so the refresh worker can query expiring connections without
	// decrypting. Nil for non-expiring credentials (PAT, long-lived OAuth).
	AccessTokenExpiresAt *time.Time `bson:"access_token_expires_at,omitempty" json:"access_token_expires_at,omitempty"`
	LastRefreshedAt      *time.Time `bson:"last_refreshed_at,omitempty" json:"last_refreshed_at,omitempty"`

	// Scopes are the granted scopes (observability only — the forge is the
	// authority on what the token can actually do).
	Scopes []string `bson:"scopes,omitempty" json:"scopes,omitempty"`

	// ManagedSecretID is the generic_secrets row the orchestrator created
	// to hold the bot-runtime forge token. The refresh worker rewrites that
	// secret's plaintext on rotation, so downstream resolver code needs no
	// changes. Empty until the first repo is enabled on this connection.
	// Internal store reference — never exposed via the API.
	ManagedSecretID string `bson:"managed_secret_id,omitempty" json:"-"`

	CreatedBy string    `bson:"created_by" json:"created_by"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// Host returns the connection's forge host without scheme, used to set
// the bot-secret binding's AllowedHosts (egress narrowing). Falls back to
// the provider's canonical host when ForgeBaseURL is empty.
func (c *Connection) Host() string {
	base := c.ForgeBaseURL
	if base == "" {
		base = DefaultBaseURL(c.Provider)
	}
	return hostOf(base)
}

// BaseURL returns the connection's forge base URL, defaulting to the
// provider's canonical SaaS host when unset.
func (c *Connection) BaseURL() string {
	if c.ForgeBaseURL != "" {
		return c.ForgeBaseURL
	}
	return DefaultBaseURL(c.Provider)
}

// DefaultBaseURL is the canonical SaaS base URL for a provider, used when
// a connection pins no self-hosted ForgeBaseURL.
func DefaultBaseURL(p Provider) string {
	switch p {
	case ProviderGitLab:
		return "https://gitlab.com"
	case ProviderGitHub:
		return "https://github.com"
	case ProviderForgejo:
		return "https://codeberg.org"
	}
	return ""
}

// CanonicalBaseURL normalises an operator-supplied forge base URL to
// scheme+host with no trailing slash (https assumed when no scheme), or the
// provider's canonical SaaS host when empty. The OAuth-app store's (tenant,
// provider, base URL) key and the connect resolver both run through this so a
// pasted "gitlab.example.com", "https://gitlab.example.com/" and "" (→ SaaS)
// all resolve to the same instance.
func CanonicalBaseURL(p Provider, raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultBaseURL(p)
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return strings.TrimRight(s, "/")
}

// hostOf strips scheme + path from a base URL, returning bare host[:port].
// Tolerant of a missing scheme so a pasted "gitlab.example.com" still works.
func hostOf(base string) string {
	s := strings.TrimSpace(base)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// Sentinel errors. Callers compare with errors.Is.
var (
	ErrConnectionNotFound  = errors.New("forge: connection not found")
	ErrIntegrationNotFound = errors.New("forge: repo integration not found")
	ErrOAuthAppNotFound    = errors.New("forge: oauth app not found")
	ErrOAuthAppExists      = errors.New("forge: oauth app already exists")
	ErrHookNotFound        = errors.New("forge: hook not found")
	// ErrForbidden is returned by an admin client when the credential lacks
	// the scope to perform an operation (e.g. create a webhook). The
	// orchestrator surfaces it as a structured "insufficient_scope" error so
	// the studio can prompt for re-auth with broader scope or a PAT.
	ErrForbidden = errors.New("forge: insufficient scope")
	// ErrUnauthorized is returned when the credential is rejected outright
	// (revoked / expired). The refresh worker flips the connection to
	// StatusRevoked on this.
	ErrUnauthorized = errors.New("forge: credential rejected")
	// ErrPermissionsNotGranted is returned when minting a GitHub-App
	// installation token fails with a PERMANENT permission mismatch — the
	// installation was approved with a narrower permission set than iterion
	// now requests (HTTP 422 "The permissions requested are not granted to
	// this installation"). It is terminal: retrying every refresh cycle can
	// never self-heal, so the worker flips the connection to StatusDegraded
	// (an org admin must re-approve the install with the updated permissions,
	// or the connection should be removed) instead of re-minting each tick.
	ErrPermissionsNotGranted = errors.New("forge: installation permissions not granted")
)
