// Package webhooks is iterion's inbound-webhook spine: long-lived,
// per-org webhook tokens that authenticate an external caller (a forge,
// CI, a script) and authorize it to launch a configured set of bots.
//
// It is the first long-lived-token concept in iterion (operator auth is
// short-lived JWT + refresh). Tokens are shown once and stored only as a
// salted hash + last4 + fingerprint, mirroring the invitation/session
// token pattern in pkg/auth.
//
// This package is provider-agnostic; the GitLab merge-request handler
// that consumes it lives in pkg/webhooks/gitlab + the server route.
package webhooks

import (
	"strings"
	"time"
)

// Provider identifies the external event source.
type Provider string

const (
	ProviderGitLab  Provider = "gitlab"
	ProviderGitHub  Provider = "github"
	ProviderForgejo Provider = "forgejo" // same wire shape as Gitea
	ProviderGeneric Provider = "generic"
)

// SignatureMode selects how an inbound delivery proves authenticity.
//
// "token" (the default — empty string) means the forge presents the
// minted iwh_ plaintext in a header; the middleware does a
// constant-time hash compare. GitLab's "secret token" model + iterion's
// own X-Iterion-Webhook-Token fall under this mode.
//
// "hmac" means the forge sends a hex HMAC-SHA256 of the raw request
// body computed with the SAME minted iwh_ plaintext as the key. The
// provider handler verifies the signature itself BEFORE acting on the
// body. The middleware MUST NOT touch the body (so we keep the bytes
// for signature recomputation) and MUST skip the header-token check
// (GitHub/Forgejo don't echo the token in any header). The plaintext
// is sealed at-rest on cfg.HMACSecretSealed so we can recompute the
// signature without storing it in cleartext.
type SignatureMode string

const (
	SignModeToken SignatureMode = ""     // header-presented bearer
	SignModeHMAC  SignatureMode = "hmac" // X-*-Signature over body
)

// Rate is a token-bucket rate limit for a webhook.
type Rate struct {
	Rate  float64 `bson:"rate" json:"rate"`   // sustained tokens/second
	Burst float64 `bson:"burst" json:"burst"` // bucket capacity
}

// Config is a per-org inbound webhook. The token plaintext is returned
// exactly once at create/rotate; only TokenHash/TokenLast4/Fingerprint
// persist.
type Config struct {
	ID          string        `bson:"_id" json:"id"`
	TenantID    string        `bson:"tenant_id" json:"tenant_id"`
	Name        string        `bson:"name" json:"name"`
	Provider    Provider      `bson:"provider" json:"provider"`
	SignMode    SignatureMode `bson:"sign_mode,omitempty" json:"sign_mode,omitempty"`
	Enabled     bool          `bson:"enabled" json:"enabled"`
	TokenHash   string        `bson:"token_hash" json:"-"`
	TokenLast4  string        `bson:"token_last4" json:"token_last4"`
	Fingerprint string        `bson:"fingerprint,omitempty" json:"fingerprint,omitempty"`

	// HMACSecretSealed holds the sealed plaintext used to recompute the
	// body HMAC for hmac-mode providers (GitHub, Forgejo). Same plaintext
	// as the minted iwh_ token — the operator pastes it once into the
	// forge's "secret" field. Empty for token-mode webhooks. Sealed via
	// secrets.Sealer with AAD bound to the webhook ID so a sealed blob
	// cannot be silently transplanted across configs.
	HMACSecretSealed []byte `bson:"hmac_secret_sealed,omitempty" json:"-"`

	// Bot scoping. BotIDs lists the allowed bot names; WildcardBots
	// (BotIDs == ["*"]) permits any bot and must be set explicitly so
	// the UI + audit can flag it.
	BotIDs       []string `bson:"bot_ids" json:"bot_ids"`
	WildcardBots bool     `bson:"wildcard_bots,omitempty" json:"wildcard_bots,omitempty"`
	DefaultBotID string   `bson:"default_bot_id,omitempty" json:"default_bot_id,omitempty"`

	// CommandMap routes a /slash-command (lowercase key, no leading slash) to
	// the bot(s) that claim it. Computed by the forge orchestrator from the
	// co-enabled bots' manifest invocations (kind=command), so a comment
	// handler resolves a command in O(1) without loading bundles on the hot
	// path. Aliases are flattened into the map (each alias is its own key).
	// The value is a slice because two bots may share a command via
	// args-based disambiguation (the review-pr vs revi-converse pattern);
	// ResolveCommand picks by whether args are present. Empty for
	// hand-created webhooks — those fall back to a live registry resolve
	// (ResolveCommandRoute) only when WildcardBots is set.
	CommandMap map[string][]CommandRoute `bson:"command_map,omitempty" json:"command_map,omitempty"`

	// Source allowlists (empty = allow-all within the tenant).
	ProjectAllowlist []string `bson:"project_allowlist,omitempty" json:"project_allowlist,omitempty"`
	EventAllowlist   []string `bson:"event_allowlist,omitempty" json:"event_allowlist,omitempty"`
	// AuthorAllowlist restricts which PR/MR author logins trigger a launch
	// (empty = any author). Case-insensitive; entries may be bot logins like
	// "dependabot[bot]" / "renovate[bot]". Lets a webhook react ONLY to a
	// dependency-bot's PRs while ignoring human PRs on the same repo.
	AuthorAllowlist []string `bson:"author_allowlist,omitempty" json:"author_allowlist,omitempty"`

	// ForgeBaseURL, when set, pins the forge instance this webhook's bot
	// token may call back to (e.g. "https://gitlab.example.com"). The
	// inbound payload's MR-URL host must match it or the delivery is
	// refused, so a hostile (but secret-authenticated) payload can't
	// redirect the bot's forge_token to an arbitrary host. Empty = derive
	// the host from the payload, still gated by the optional global
	// ITERION_WEBHOOK_FORGE_HOSTS allowlist. GitLab note/MR flows only.
	ForgeBaseURL string `bson:"forge_base_url,omitempty" json:"forge_base_url,omitempty"`

	// Limits.
	RateLimit        Rate `bson:"rate_limit" json:"rate_limit"`
	MonthlyCallLimit int  `bson:"monthly_call_limit,omitempty" json:"monthly_call_limit,omitempty"` // 0 = inherit org

	// LaunchVars are stamped onto every run launched through this webhook
	// (e.g. severity_threshold), overriding the handler-derived vars.
	LaunchVars map[string]string `bson:"launch_vars,omitempty" json:"launch_vars,omitempty"`

	// KeyOverrides pins a BYOK key per LLM provider for runs launched
	// through this webhook (provider name → api_key id), overriding the
	// org/user default in secrets.Resolve. Lets several webhooks for the
	// same bot bill against different keys. See docs/byok.md.
	KeyOverrides map[string]string `bson:"key_overrides,omitempty" json:"key_overrides,omitempty"`

	// SecretOverrides pins a specific stored secret per workflow-secret name
	// (name -> secret id) for runs launched through this webhook, overriding
	// the org bot-secret binding in secrets.ResolveGenericWithBindings. Lets
	// several webhooks for the same bot post under different forge tokens /
	// bot identities. See docs/byok.md.
	SecretOverrides map[string]string `bson:"secret_overrides,omitempty" json:"secret_overrides,omitempty"`

	// AuthorizedRepliers + MinReplierRole gate who may "talk back" to the bot
	// via a note (a /revi command or a reply): a note author is authorized
	// when they are in AuthorizedRepliers (usernames with/without @, or numeric
	// ids) OR a project member at >= MinReplierRole (guest|reporter|developer|
	// maintainer|owner; empty → developer). See docs/forge-conversations.md.
	AuthorizedRepliers []string `bson:"authorized_repliers,omitempty" json:"authorized_repliers,omitempty"`
	MinReplierRole     string   `bson:"min_replier_role,omitempty" json:"min_replier_role,omitempty"`

	// ProvisionedBy marks a config the forge Integrations orchestrator
	// created + owns (value "forge:<connection_id>"), as opposed to one an
	// operator hand-created. Non-empty configs are managed: the CRUD layer
	// blocks direct delete (the operator disables the integration instead)
	// and the studio Webhooks tab renders them read-only with a "Managed via
	// Integrations" pill. Empty = a normal operator-created webhook (the
	// default; every pre-existing row decodes to "" and behaves as before).
	ProvisionedBy string `bson:"provisioned_by,omitempty" json:"provisioned_by,omitempty"`

	CreatedBy  string     `bson:"created_by" json:"created_by"`
	CreatedAt  time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt  time.Time  `bson:"updated_at" json:"updated_at"`
	LastUsedAt *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
	RotatedAt  *time.Time `bson:"rotated_at,omitempty" json:"rotated_at,omitempty"`
}

// AllowsBot reports whether this webhook may launch botID.
func (c *Config) AllowsBot(botID string) bool {
	if c == nil || botID == "" {
		return false
	}
	if c.WildcardBots {
		return true
	}
	for _, b := range c.BotIDs {
		if b == "*" || b == botID {
			return true
		}
	}
	return false
}

// SelectBot resolves which bot a delivery should launch: the explicit
// default, else the sole allowed bot. Returns "" when ambiguous (the
// caller decides per-provider, e.g. GitLab V1 pins Revi).
func (c *Config) SelectBot() string {
	if c == nil {
		return ""
	}
	if c.DefaultBotID != "" {
		return c.DefaultBotID
	}
	if len(c.BotIDs) == 1 && c.BotIDs[0] != "*" {
		return c.BotIDs[0]
	}
	return ""
}

// CommandRoute records how a webhook routes one /slash-command to a bot and
// its execution mode. Mirrors the bot's manifest command invocation
// (bundle.InvocationCommand + the invocation's mode/args_var/context_vars),
// flattened by the orchestrator so a comment handler dispatches without
// touching the bot bundle.
type CommandRoute struct {
	BotID          string            `bson:"bot_id" json:"bot_id"`
	Mode           string            `bson:"mode,omitempty" json:"mode,omitempty"` // "direct" | "board" (empty = direct)
	ArgsVar        string            `bson:"args_var,omitempty" json:"args_var,omitempty"`
	ContextVars    map[string]string `bson:"context_vars,omitempty" json:"context_vars,omitempty"`
	Scope          string            `bson:"scope,omitempty" json:"scope,omitempty"` // "pr" | "issue" | "any" (empty = pr)
	MinReplierRole string            `bson:"min_replier_role,omitempty" json:"min_replier_role,omitempty"`
	Disambiguator  string            `bson:"disambiguator,omitempty" json:"disambiguator,omitempty"`

	// OpensMR mirrors the bot manifest command's opens_mr flag: when set, a
	// board-mode dispatch of this command stamps open_mr="true" +
	// source_issue_ref=<subject URL/ref> into the materialised card's bot_args
	// so the routed bot opens an MR and back-links the issue the human
	// commented on. Off for read-only commands (e.g. /revi).
	OpensMR bool `bson:"opens_mr,omitempty" json:"opens_mr,omitempty"`
}

// AllowsScope reports whether this route may fire for a comment on the given
// surface ("pr" or "issue"). An empty route scope defaults to "pr" (matching
// today's /revi-on-MR behaviour); "any" matches both.
func (r CommandRoute) AllowsScope(surface string) bool {
	sc := r.Scope
	if sc == "" {
		sc = "pr"
	}
	return sc == "any" || sc == surface
}

// ResolveCommand resolves a /slash-command on this webhook to a single route,
// picking by args-presence when two bots share the command via
// disambiguation (when_args_present claims "/cmd <args>", when_args_empty
// claims a bare "/cmd"). ok=false means no route is configured for cmd (the
// caller may fall back to a live registry resolve for a wildcard webhook).
func (c *Config) ResolveCommand(cmd, args string) (CommandRoute, bool) {
	if c == nil || cmd == "" || len(c.CommandMap) == 0 {
		return CommandRoute{}, false
	}
	routes := c.CommandMap[strings.ToLower(cmd)]
	if len(routes) == 0 {
		return CommandRoute{}, false
	}
	hasArgs := strings.TrimSpace(args) != ""
	// Prefer a disambiguator that matches the args state.
	for _, r := range routes {
		if (r.Disambiguator == "when_args_present" && hasArgs) ||
			(r.Disambiguator == "when_args_empty" && !hasArgs) {
			return r, true
		}
	}
	// Else the first unconditional (no-disambiguator) claim.
	for _, r := range routes {
		if r.Disambiguator == "" {
			return r, true
		}
	}
	// All routes are disambiguated but none matched the args state — fall
	// back to the first so a configured command never silently no-ops.
	return routes[0], true
}

// Delivery records an inbound webhook delivery for audit + idempotency.
// It NEVER stores the raw payload — only a hash and the selected fields.
type Delivery struct {
	ID             string   `bson:"_id" json:"id"`
	TenantID       string   `bson:"tenant_id" json:"tenant_id"`
	WebhookID      string   `bson:"webhook_id" json:"webhook_id"`
	Provider       Provider `bson:"provider" json:"provider"`
	IdempotencyKey string   `bson:"idempotency_key" json:"idempotency_key"`

	EventKind   string `bson:"event_kind,omitempty" json:"event_kind,omitempty"`
	EventAction string `bson:"event_action,omitempty" json:"event_action,omitempty"`
	ProjectPath string `bson:"project_path,omitempty" json:"project_path,omitempty"`
	SubjectID   string `bson:"subject_id,omitempty" json:"subject_id,omitempty"`
	SubjectSHA  string `bson:"subject_sha,omitempty" json:"subject_sha,omitempty"`
	PayloadHash string `bson:"payload_hash,omitempty" json:"payload_hash,omitempty"`

	Status     string     `bson:"status" json:"status"`
	BotID      string     `bson:"bot_id,omitempty" json:"bot_id,omitempty"`
	RunID      string     `bson:"run_id,omitempty" json:"run_id,omitempty"`
	Error      string     `bson:"error,omitempty" json:"error,omitempty"`
	SourceIP   string     `bson:"source_ip,omitempty" json:"source_ip,omitempty"`
	ReceivedAt time.Time  `bson:"received_at" json:"received_at"`
	LaunchedAt *time.Time `bson:"launched_at,omitempty" json:"launched_at,omitempty"`
}

// Delivery status values.
const (
	StatusAccepted      = "accepted"
	StatusDuplicate     = "duplicate"
	StatusRateLimited   = "rate_limited"
	StatusQuotaExceeded = "quota_exceeded"
	StatusInvalid       = "invalid"
	StatusFiltered      = "filtered"
	StatusLaunched      = "launched"
	StatusLaunchError   = "launch_error"
)
