package connection

import (
	"strings"
	"time"
)

// The FORGE connection model, contracted into this package from pkg/forge
// (#2760, ADR-098 decision 3: "pkg/forge's connection model becomes
// pkg/connection … and the forges become connectors #1-3").
//
// What moved is the MODEL — the record, its store (forge_store.go) and its
// credential sealer (forge_credential.go). What stays in pkg/forge is the
// BEHAVIOUR built on top of it: the OAuth/App connect flows, the refresh
// worker, the provisioning orchestrator and the three forge clients. Those
// read this contract through aliases, so the move changes no call site's
// vocabulary — only who owns the type.
//
// Three things are PRESERVED by construction, which is what "existing
// credentials still open" means concretely:
//   - record ids and the whole BSON shape — the collection
//     (`forge_connections`) and its indexes are untouched, so no data
//     migration exists to run;
//   - the `forge_conn:<id>` sealing AAD — a payload sealed by any previous
//     build opens under this one, and new payloads seal under the same AAD
//     (ADR-098: the expand-and-contract preserves it);
//   - the OAuth callback URLs, which name routes, not packages.

// Provider identifies the forge a connection targets. Mirrors
// webhooks.Provider (minus "generic", which has no admin API).
//
// The value doubles as the CONNECTOR ID this forge answers to in the
// catalog ("github", "gitlab", "forgejo" are connectors #1-3): the provider
// enum is the forge-era spelling of what a connection's Connector field
// carries for every other package.
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

// BotIdentity values — what identity a connection's bots POST as. Empty keeps
// the historical behaviour: the connected account itself. See
// ForgeConnection.BotIdentity.
const (
	// BotIdentityConnection is the zero value: bots post as the account the
	// connection authenticated as (the pasted token's bot user, the OAuth
	// user, the App).
	BotIdentityConnection = ""
	// BotIdentityProjectToken makes iterion mint one project access token per
	// enabled repository and hand bots THAT token: they post as the
	// repository's own bot user, never as the person whose OAuth grant
	// bootstrapped the connection. GitLab OAuth connections only — the one
	// forge with a per-project bot API and no App equivalent.
	BotIdentityProjectToken = "project_token"
)

// AccountKind values — what the account behind a credential IS on the forge.
// Identity.Kind carries one at connect time and ForgeConnection.AccountKind keeps it.
const (
	// AccountKindUser is a person's account: an OAuth authorization or a PAT.
	AccountKindUser = "user"
	// AccountKindBot is a machine account the forge itself flags as one —
	// GitLab's group/project-access-token bot user or service account. The
	// only kind iterion rebrands with the iterion-bot avatar unasked.
	AccountKindBot = "bot"
	// AccountKindInstallation is a GitHub App acting through an installation.
	AccountKindInstallation = "installation"
)

// StatusNeedsReauth extends the shared status vocabulary with the
// forge-lifecycle state the generic trio cannot express: the credential
// exists but its refresh failed, and only an operator reconnecting brings it
// back. Not usable, and distinct from StatusRevoked (the credential was
// rejected outright) because the remedy differs — re-authorise versus
// re-create.
//
// One vocabulary, per ADR-098: the forge record carries the SAME Status type
// as every other connection rather than a parallel enum that would drift.
const StatusNeedsReauth Status = "needs_reauth"

// Purpose is the role a connection plays. It is deliberately NOT derived
// from the granted permissions: a runtime App that an owner narrowed by
// mistake must read as broken (degraded, with a reason), not silently
// re-label itself a security-read connection and stop doing its job.
type Purpose string

const (
	// PurposeRuntime is the zero value, so every connection that existed
	// before this field keeps its behaviour with no migration.
	PurposeRuntime Purpose = ""
	// PurposeSecurityRead is a read-only alerts source. See ForgeConnection.Purpose.
	PurposeSecurityRead Purpose = "security_read"
)

// IsSecurityReadOnly reports whether this connection exists only to mint the
// org-wide Dependabot-alerts token and must never be used for runtime work.
func (c ForgeConnection) IsSecurityReadOnly() bool { return c.Purpose == PurposeSecurityRead }

// MintsProjectBots reports whether enabling a repository on this connection
// mints a per-repository bot credential instead of handing bots the
// connection's own token. GitLab-only: the per-project access-token API has
// no twin on the other forges.
func (c ForgeConnection) MintsProjectBots() bool {
	return c.Provider == ProviderGitLab && c.BotIdentity == BotIdentityProjectToken
}

// ForgeConnection is a team's authenticated link to one forge account. The
// admin token material lives sealed on SealedPayload (AAD bound to ID);
// the JSON encoder drops it so it never reaches the studio.
//
// It is the forge flavour of the connection model — richer than the generic
// Connection because it also tracks credential ACQUISITION (OAuth app
// linkage, GitHub-App installation identity, refresh bookkeeping), which
// ADR-098's F21 names as the lifecycle seam a later lot generalises. Until
// then this record is what the forge store persists.
type ForgeConnection struct {
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

	// WebhookBaseURL overrides, for this connection only, the base the
	// INBOUND hook URL is built from — the reverse direction of
	// ForgeBaseURL, which names the host we call OUT to. Empty = the
	// deployment's public URL, which is what every connection wants until
	// one of them cannot reach it.
	//
	// It exists because a forge may refuse the deployment's canonical host
	// outright: GitLab's outbound allowlist rejects any unlisted webhook
	// URL with "Invalid url given" (HTTP 422), and getting a host listed is
	// an administrative act on the forge's side, not ours. Without a
	// per-connection escape hatch, one such forge pins the whole
	// deployment's public URL — a re-provision would rewrite its hook to an
	// address it is not allowed to call, and the failure would only surface
	// as a provisioning error much later.
	//
	// Declaring it is what makes the exception survive: a hook URL that
	// merely predates a public-URL change is one re-provision away from
	// being silently replaced.
	WebhookBaseURL string `bson:"webhook_base_url,omitempty" json:"webhook_base_url,omitempty"`

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
	// BotIdentity is what this connection's bots post as (the BotIdentity*
	// constants). BotIdentityProjectToken is stamped at connect on a GitLab
	// OAuth connection: the OAuth grant stays the ADMIN credential (list
	// repos, create hooks, mint), and every enabled repository gets its own
	// minted project access token as the runtime forge token
	// (RepoIntegration.ProjectBot). Empty on every connection that predates
	// the field, which keeps the connected account as the posting identity.
	BotIdentity string `bson:"bot_identity,omitempty" json:"bot_identity,omitempty"`
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

	// Purpose is what this connection is FOR. Empty (PurposeRuntime) is the
	// ordinary forge connection: it clones, pushes, opens PRs, carries
	// webhooks. PurposeSecurityRead marks a connection whose App holds only
	// metadata:read + vulnerability_alerts:read — it exists solely to mint the
	// org-wide Dependabot-alerts token, and CANNOT do runtime work.
	//
	// The distinction is load-bearing in three places, and skipping any one of
	// them breaks the connection rather than merely limiting it:
	//   - the refresh worker must not mint a RUNTIME token for it (the mint
	//     would 422 on permissions it deliberately lacks, marking it degraded
	//     and withdrawing the very token it exists to supply);
	//   - the auto-resolvers that pick "a connection on this host" must skip
	//     it, or a review lands on a connection that cannot post;
	//   - the health view must not report its absent delivery grants as a
	//     defect — they are the point.
	Purpose Purpose `bson:"purpose,omitempty" json:"purpose,omitempty"`

	// SecurityReadOrgKey records the key this connection's token was actually
	// FILED UNDER in the dependabot_tokens map. Withdrawal reads it instead of
	// re-deriving the key from InstallationAccount, which the health probe
	// rewrites from live truth: after a GitHub org rename the derived key no
	// longer matches the deposited one, and the old entry becomes a dead token
	// no withdrawal path can reach — the map never empties, so the secret is
	// never deleted, and a disconnect leaves it readable by every bot.
	SecurityReadOrgKey string `bson:"security_read_org_key,omitempty" json:"-"`

	Status Status `bson:"status" json:"status"`

	// StatusReason is a human-readable explanation for a non-active Status
	// (e.g. which permission a degraded GitHub-App install is missing and
	// that an org admin must re-approve or remove the connection). Surfaced
	// to the operator so the fix is actionable; cleared on reconnect.
	StatusReason string `bson:"status_reason,omitempty" json:"status_reason,omitempty"`

	// AccountKind is what the connected account IS on the forge (the
	// AccountKind* constants), copied from WhoAmI at connect time. It gates
	// the iterion-bot avatar upload: only AccountKindBot is rebranded without
	// the operator asking. Empty on connections older than the field, which
	// reads as a user — nothing is ever rebranded on a mere absence.
	AccountKind string `bson:"account_kind,omitempty" json:"account_kind,omitempty"`
	// AvatarAppliedAt is when iterion last uploaded the iterion-bot avatar
	// onto this connection's account (at connect time for a bot identity, or
	// through the explicit apply action). Nil = never.
	AvatarAppliedAt *time.Time `bson:"avatar_applied_at,omitempty" json:"avatar_applied_at,omitempty"`
	// AvatarError keeps the forge's refusal of the last avatar upload, so the
	// studio can name it and offer a retry instead of showing a default
	// avatar with no explanation. Cleared by the next successful upload.
	AvatarError string `bson:"avatar_error,omitempty" json:"avatar_error,omitempty"`

	// SealedPayload holds the token blob (access/refresh/PAT + expiry),
	// sealed via secrets.Sealer with AAD "forge_conn:<ID>" (preserved by the
	// #2760 contract — see the file doc). Never serialised.
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
func (c *ForgeConnection) Host() string {
	base := c.ForgeBaseURL
	if base == "" {
		base = DefaultBaseURL(c.Provider)
	}
	return hostOf(base)
}

// BaseURL returns the connection's forge base URL, defaulting to the
// provider's canonical SaaS host when unset.
func (c *ForgeConnection) BaseURL() string {
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
