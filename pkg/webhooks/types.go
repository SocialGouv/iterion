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

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/schedgate"
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

	// BotRules is the per-bot routing table for the event-driven lanes. When
	// non-empty it REPLACES the single-bot SelectBot path: one delivery fans
	// out to every rule that claims the event and admits the author, so a
	// dependency-guard and a reviewer can share one repo webhook and each
	// react to its own PRs. Nil on every config written before this field
	// existed → the legacy single-bot path, byte-identical. Written only by
	// the forge orchestrator (never by the webhook CRUD layer): the rules are
	// derived from the co-enabled bots' manifests, so hand-editing them would
	// re-open the drift the CommandMap design already closed.
	BotRules []BotRule `bson:"bot_rules,omitempty" json:"bot_rules,omitempty"`

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
	// LabelAllowlist restricts which freshly-applied issue label triggers a
	// launch on the GitHub/Forgejo `issues` (labeled) path (e.g.
	// ["implement"] so only that label dispatches the bot). Empty = any
	// label triggers. Case-insensitive; see MatchLabel. Has no effect on the
	// pull_request / issue_comment paths.
	LabelAllowlist []string `bson:"label_allowlist,omitempty" json:"label_allowlist,omitempty"`
	// HoldLabels is a bot-agnostic suppression set: when the triggering PR or
	// issue carries any of these labels, the auto-launch lanes (PR-open review,
	// auto-implement-on-open) suppress the launch — whatever bot would have
	// run. It is the operator's escape hatch to pause automation on one
	// PR/issue without disabling the webhook. Case-insensitive; empty = off.
	// Distinct from LabelAllowlist (which selects a bot); this vetoes ALL bots.
	HoldLabels []string `bson:"hold_labels,omitempty" json:"hold_labels,omitempty"`

	// BranchImproveAsPR changes how the branch-improvement bot (Billy) lands
	// its hardening on a PR it reviews. Default (false): it commits + pushes
	// directly onto the PR's own source branch (in-place — the author merges
	// its PR and gets the improvements with it). True: Billy instead opens a
	// SEPARATE PR targeting that source branch, so the author reviews the bot's
	// changes as an isolated diff before integrating them — the right posture
	// for a third-party contributor's work (they stay in control of their
	// branch). Routes Billy through open_mr=true + mr_base=<source branch>
	// instead of the direct push-back.
	BranchImproveAsPR bool `bson:"branch_improve_as_pr,omitempty" json:"branch_improve_as_pr,omitempty"`

	// AutoImplementOnOpen, when true, dispatches the implementer bot on a
	// freshly-OPENED issue (not only a labeled one) — the zero-touch lane where
	// iterion turns every new issue into a PR without a manual label. OFF by
	// default: labeling an issue (LabelAllowlist) stays the deliberate opt-in,
	// so enabling this is a per-webhook decision to auto-act on ALL new issues.
	// The labeled path keeps working alongside it. The opened lane is
	// author-gated (see MinAuthorRole): an untrusted author's issue is
	// filtered here and parks on the board for approval instead.
	AutoImplementOnOpen bool `bson:"auto_implement_on_open,omitempty" json:"auto_implement_on_open,omitempty"`

	// MinAuthorRole is the minimum repo role (gitlab vocabulary: guest|
	// reporter|developer|maintainer|owner; "" → developer ≡ write) the ISSUE
	// AUTHOR must hold for the AutoImplementOnOpen zero-touch lane to launch
	// — the budget boundary against drive-by issues. Trust resolves as:
	// AuthorAllowlist ∪ GitHub author_association fast path ∪ live
	// CollaboratorPermission (needs a forge_token binding). The labeled lane
	// is NOT author-gated: applying the trigger label already requires
	// triage+ rights on the forge, which IS the approval gesture.
	MinAuthorRole string `bson:"min_author_role,omitempty" json:"min_author_role,omitempty"`

	// ReviewRequestLogins names the review identities whose (re-)request the
	// on-demand re-review lane answers, IN ADDITION to the one derived from the
	// webhook's forge connection. It is what makes that lane reachable on
	// GitHub at all: a GitHub App cannot be a requested reviewer, so the button
	// only exists for a User account — and the review must be POSTED by that
	// same account for the forge to clear the pending request and re-arm it,
	// which is what a `pat` connection to a dedicated bot user gives.
	//
	// EXPLICIT ONLY, never derived from the connection's account: the PAT
	// connect path stamps whatever token was pasted, typically a maintainer's
	// own, and deriving would turn every ordinary reviewer ping addressed to
	// that human into a bot run — the reasoning isIterionForgeBotAuthor already
	// applies when it refuses to trust AccountLogin on GitHub.
	//
	// The logins join iterionBotLogins, so BOTH halves of the identity read the
	// same set: the lane answers their request, and the actor guard recognises
	// their own PRs and their own reviewer-writes. An identity only one half
	// knew would launch on the bot's own echo.
	ReviewRequestLogins []string `bson:"review_request_logins,omitempty" json:"review_request_logins,omitempty"`

	// ReviewOnSync, when true, re-runs the review bot on a PR "synchronize"
	// (a push to the PR head), not only on opened/reopened. OFF by default
	// (a push is normally on-demand re-review — see prforge.IsReviewable, kept
	// budget-frugal). Turn it ON to power the MERGE GATE: the reviewer posts
	// its revi/review commit status per head SHA, so as the author pushes
	// fixes the required check re-evaluates on the new revision instead of
	// deadlocking on a status the old SHA carried. Pairs with the bot's
	// gate_enabled var + a required-check ruleset listing revi/review.
	ReviewOnSync bool `bson:"review_on_sync,omitempty" json:"review_on_sync,omitempty"`

	// ReviewOnSyncPinned records that an operator set ReviewOnSync
	// EXPLICITLY through the webhook API (either value). Provisioning's
	// gating derivation then leaves ReviewOnSync alone in BOTH directions —
	// it neither forces it on for a statuses-scope bot nor releases it on a
	// gate_enabled=false pin. An explicit operator choice is never silently
	// replaced (CLAUDE.md principle 1); without the pin, ReviewOnSync is
	// presumed derivation-owned. Clearable via the same PATCH
	// (review_on_sync_pinned: false) to hand the field back.
	ReviewOnSyncPinned bool `bson:"review_on_sync_pinned,omitempty" json:"review_on_sync_pinned,omitempty"`

	// BlockForkPRs, when true, filters (never auto-launches ANY bot on) a PR
	// whose head branch lives in a DIFFERENT repo than its base — a fork PR.
	// The anti budget-exhaustion boundary: a fork PR is untrusted (an adversary
	// can open many to trigger costly bot runs), so an operator must validate it
	// before a bot runs. Off by default (fork PRs still auto-review via Revi;
	// the mutating branch-improve bot never runs on a PR-open regardless — the
	// PR-open lane is review-only, see handlePRForgeReview). Recommended ON for
	// a public repo.
	BlockForkPRs bool `bson:"block_fork_prs,omitempty" json:"block_fork_prs,omitempty"`

	// ForgeBaseURL, when set, pins the forge instance this webhook's bot
	// token may call back to (e.g. "https://gitlab.example.com"). The
	// inbound payload's MR-URL host must match it or the delivery is
	// refused, so a hostile (but secret-authenticated) payload can't
	// redirect the bot's forge_token to an arbitrary host. Empty = derive
	// the host from the payload, still gated by the optional global
	// ITERION_WEBHOOK_FORGE_HOSTS allowlist. GitLab note/MR flows only.
	ForgeBaseURL string `bson:"forge_base_url,omitempty" json:"forge_base_url,omitempty"`

	// Limits.
	RateLimit Rate `bson:"rate_limit" json:"rate_limit"`
	// RateLimitPinned records that an operator set RateLimit EXPLICITLY
	// through the webhook API (create or PATCH). The re-provision carry
	// preserves a pinned value — losing an operator's raise means
	// deliveries silently 429. An UNPINNED value is presumed
	// provisioner-owned: a re-provision moves it to the current
	// provisioning default, so a default bump actually reaches existing
	// webhooks instead of freezing each on the burst it was born with.
	RateLimitPinned  bool `bson:"rate_limit_pinned,omitempty" json:"rate_limit_pinned,omitempty"`
	MonthlyCallLimit int  `bson:"monthly_call_limit,omitempty" json:"monthly_call_limit,omitempty"` // 0 = inherit org

	// OperatorLaunchVars are the per-repo overrides an operator pinned on the
	// integration, kept SEPARATE from LaunchVars (which is the union of every
	// co-enabled bot's manifest vars). Layering that union last would undo the
	// per-bot isolation BotRule.LaunchVars exists for: two bots declaring the
	// same key with different values would both run with whichever won the
	// union. Precedence is base < the bot's own rule vars < these.
	OperatorLaunchVars map[string]string `bson:"operator_launch_vars,omitempty" json:"operator_launch_vars,omitempty"`

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

	// Overlap is the concurrency policy for runs this webhook launches,
	// keyed on (webhook, subject, bot) — one PR's reviews, not the whole
	// repo's. Vocabulary shared with every other launch surface
	// (pkg/schedgate): "allow" | "skip" | "supersede".
	//
	// EMPTY means allow, NOT schedgate's "skip" default. A webhook is
	// event-driven: every delivery has always launched, and silently
	// promoting that to at-most-one-live would drop deliveries operators
	// currently rely on. The gate applies only when explicitly set.
	//
	// "supersede" is the one worth setting on a review webhook: with
	// re-review on push, three pushes in two minutes launch three runs, two
	// of which analyse stale code and post their verdict on dead commits.
	// Superseding keeps the run that matches what is actually on the branch.
	Overlap string `bson:"overlap,omitempty" json:"overlap,omitempty"`

	// Retry policy (pkg/retrypolicy) for a run launched through this
	// webhook that dies on an exhausted provider usage window. The
	// launch-surface layer, same role the equivalent fields play on a
	// schedule row or a trigger subscription: only what is set here
	// overrides the bot's manifest and the machine default.
	//
	// A webhook-launched run is often the one an author is WAITING on (a
	// PR review), so a shorter max_wait than a nightly's is usually the
	// right call — a review that lands three days late is not a review.
	RetryUsageWindow string `bson:"retry_usage_window,omitempty" json:"retry_usage_window,omitempty"`
	RetryMaxAttempts int    `bson:"retry_max_attempts,omitempty" json:"retry_max_attempts,omitempty"`
	RetryMaxWait     string `bson:"retry_max_wait,omitempty" json:"retry_max_wait,omitempty"`
	RetryJitter      string `bson:"retry_jitter,omitempty" json:"retry_jitter,omitempty"`

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

// BotRule is one bot's OWN routing contract on a shared repo webhook: which
// forge events it claims, whose PRs it reacts to, and the launch vars its
// manifest declares. The forge orchestrator materialises one rule per
// co-enabled bot, so an inbound delivery fans out to EVERY bot whose own rule
// matches — each with its own author filter and its own vars, instead of the
// single winner a flattened config could express.
//
// Events use the NORMALIZED vocabulary (bundle.ForgeEvent*, e.g.
// "pull_request"), never a provider-native name, so one rule set serves
// github / gitlab / forgejo identically.
type BotRule struct {
	BotID string `bson:"bot_id" json:"bot_id"`

	// Events this bot claims, normalized. Empty = claims no event lane: a
	// command-only bot is routed by CommandMap and never auto-launched.
	Events []string `bson:"events,omitempty" json:"events,omitempty"`

	// Actions narrows the trigger inside an event ("opened", "reopened").
	// RECORDED BUT NOT ENFORCED: the forge also sends actions a manifest never
	// lists but the lanes depend on ("ready_for_review" on a draft flip,
	// "synchronize" for the merge gate), so the handler's own reviewability
	// gate stays authoritative and this is documentation until the canonical
	// action mapping lands.
	Actions []string `bson:"actions,omitempty" json:"actions,omitempty"`

	// AuthorAllowlist restricts which PR/MR author logins fire THIS bot.
	// Empty = open. Same matcher as Config.AuthorAllowlist (MatchAuthor).
	AuthorAllowlist []string `bson:"author_allowlist,omitempty" json:"author_allowlist,omitempty"`
	// AuthorDenylist suppresses this bot for these logins; deny beats allow.
	// Materialised by the orchestrator from another co-enabled bot's exclusive
	// author claim (manifest forge.webhook.author_scope: exclusive) — stored
	// rather than computed per request so the suppression is auditable.
	AuthorDenylist []string `bson:"author_denylist,omitempty" json:"author_denylist,omitempty"`

	// LabelAllowlist narrows the issue-labeled lane for this bot. Empty = any.
	LabelAllowlist []string `bson:"label_allowlist,omitempty" json:"label_allowlist,omitempty"`

	// LaunchVars are THIS bot's manifest forge.webhook.launch_vars, kept per
	// bot so two bots' vars cannot collide in one flat map.
	LaunchVars map[string]string `bson:"launch_vars,omitempty" json:"launch_vars,omitempty"`

	// Mode mirrors the invocation's execution mode ("direct" | "board").
	Mode string `bson:"mode,omitempty" json:"mode,omitempty"`
}

// MatchesEvent reports whether this rule claims a normalized event kind.
func (r BotRule) MatchesEvent(event string) bool {
	if event == "" {
		return false
	}
	for _, e := range r.Events {
		if e == "*" || strings.EqualFold(strings.TrimSpace(e), event) {
			return true
		}
	}
	return false
}

// MatchesAuthor applies this rule's own author filter (deny beats allow,
// empty allowlist = open).
func (r BotRule) MatchesAuthor(login string) bool {
	return MatchAuthorRule(r.AuthorAllowlist, r.AuthorDenylist, login)
}

// HasBotRules reports whether this config carries a per-bot routing table. A
// config without one predates the field and must keep the legacy single-bot
// behaviour, idempotency keys included.
func (c *Config) HasBotRules() bool { return c != nil && len(c.BotRules) > 0 }

// RulesForEvent returns, in provision order, every rule claiming event whose
// author filter admits author and whose bot the webhook still permits
// (AllowsBot — belt to the provisioning braces, since bot scope and rules are
// written together but read long after).
func (c *Config) RulesForEvent(event, author string) []BotRule {
	if !c.HasBotRules() {
		return nil
	}
	var out []BotRule
	for _, r := range c.BotRules {
		if !r.MatchesEvent(event) || !r.MatchesAuthor(author) || !c.AllowsBot(r.BotID) {
			continue
		}
		out = append(out, r)
	}
	return out
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
	// ParentSubjectID is the subject this one HANGS OFF, when the two
	// differ: a `/command` comment ("comment:99") and a review-thread reply
	// ("rc:88") both live on a pull request ("pr:7"). SubjectID stays
	// per-comment because it is the idempotency key — one launch per comment
	// — so without this second handle a run launched from a comment is
	// unreachable from its own pull request. Empty when the subject has no
	// parent (the pull_request lane, a plain-issue comment, generic).
	ParentSubjectID string `bson:"parent_subject_id,omitempty" json:"parent_subject_id,omitempty"`
	SubjectSHA      string `bson:"subject_sha,omitempty" json:"subject_sha,omitempty"`
	PayloadHash     string `bson:"payload_hash,omitempty" json:"payload_hash,omitempty"`

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

// OverlapPolicy projects the webhook's overlap field onto the shared
// launch-surface policy. Not normalized: schedgate's zero value means "skip"
// and a webhook's means "allow", so the caller checks Overlap != "" before
// evaluating rather than letting Normalize impose the wrong default here.
func (c Config) OverlapPolicy() schedgate.Policy {
	return schedgate.Policy{Overlap: c.Overlap}
}

// RetryPolicy projects the webhook's retry fields. Not normalized — this
// is one layer of a precedence chain, and defaults filled here would
// masquerade as an explicit per-webhook choice.
func (c Config) RetryPolicy() retrypolicy.Policy {
	return retrypolicy.Policy{
		UsageWindow: c.RetryUsageWindow,
		MaxAttempts: c.RetryMaxAttempts,
		MaxWait:     c.RetryMaxWait,
		Jitter:      c.RetryJitter,
	}
}
