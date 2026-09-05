package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Credentials carries the resolved per-run BYOK plaintext keyed by
// provider. Stamped into context by the runner right before the
// engine starts, consumed by pkg/backend/model/registry.go and the
// claude_code/codex delegate backends.
//
// Plaintexts here are sensitive: never log them, never include them
// in events. The runner zeroes the slice after the run completes
// (best effort — Go does not give us secure-erase guarantees, but
// the bundle's TTL bounds exposure on the wire and at rest).
type Credentials struct {
	APIKeys map[Provider]string
	// Generic maps workflow/user secret names to plaintext values. It is
	// populated by the cloud runner from sealed per-run bundles and used
	// by declared workflow secrets whose value is intentionally empty
	// (meaning "resolve by name from the user's/team's stored secrets").
	Generic map[string]string
	// GenericHosts maps a generic secret name to the egress host
	// allowlist a bot-secret binding imposes on it (empty/absent = no
	// binding restriction). The secret guard intersects it with the
	// workflow's declared `secrets.<name>.hosts` so a binding can only
	// narrow, never broaden, where the credential may egress.
	GenericHosts map[string][]string
	// GenericRefs maps a generic secret name to its generic-secret store
	// record ID (IDs only, never values). It lets the runner re-read the
	// server-refreshed record mid-run and rewrite the secret's
	// materialised file before a short-TTL credential (e.g. a 1h GitHub
	// App installation token) expires under a long run.
	GenericRefs map[string]string
	// OAuthCredentialFiles maps "claude_code" / "codex" → the
	// absolute path of a temp directory holding the materialised
	// credentials.json or auth.json. The delegate backends pass
	// this directory via CLAUDE_CONFIG_DIR / CODEX_HOME to the
	// CLI subprocess. Empty when no OAuth-forfait is in play.
	OAuthCredentialFiles map[string]string
	// ForgeAppBotLogin, when set, is the GitHub-App bot login whose
	// installation token pushes this run's commits (see RunBundle). The
	// runner uses it to seed the App-bot git committer identity, which a
	// bare installation token can't self-resolve. Empty for PAT/OAuth runs.
	ForgeAppBotLogin string
	// PlatformSourced marks the slots (provider names / OAuth kinds) the
	// platform tier filled — see RunBundle.PlatformSourced. Consumers that
	// scope metering or policy per tenant must treat these as the
	// deployment's own credential, not the tenant's.
	PlatformSourced map[string]bool
	// PoolSourced marks the slots the credential pool filled with a lent
	// credential — see RunBundle.PoolSourced. Metering that scopes a bump
	// per tenant must treat these as the donor's, not the run's tenant's.
	PoolSourced map[string]bool
	// Fingerprints maps a credential slot (a Provider name or an OAuth
	// kind) to the audit identity of what filled it. Not sensitive (8
	// hash bytes) and deliberately NOT zeroed by cleanup: it says WHICH
	// credential a run drew on. The usage-cap meter key composes it, so
	// a rotated credential opens a fresh ledger instead of inheriting
	// the readings of the account it replaced.
	//
	// The two slot kinds derive it differently, and the difference IS
	// the contract: an API key is static, so its own hash identifies it,
	// while an OAuth payload is NOT — the refresh worker rewrites its
	// tokens every few hours for the same subscription — so an OAuth
	// slot carries the record's stamped identity
	// (SubscriptionFingerprint, fixed at connect) rather than a hash of
	// the blob sitting in the slot. Hashing an OAuth payload here would
	// rotate the meter with every token and no reading would ever
	// accumulate. Absent for credentials that predate stamping: those
	// keep the fingerprint-less meter they always had.
	Fingerprints map[string]string
}

// IsPlatformSourced reports whether the named credential slot (a provider
// name like "anthropic" or an OAuth kind like "claude_code") was filled by
// the platform tier rather than the tenant's own stores.
func (c Credentials) IsPlatformSourced(slot string) bool {
	return c.PlatformSourced[slot]
}

// IsPoolSourced reports whether the named credential slot was filled by the
// mutualised credential pool with a contributor's lent credential.
func (c Credentials) IsPoolSourced(slot string) bool {
	return c.PoolSourced[slot]
}

// WireFamily groups credential slots (Provider names and OAuthKinds) that
// authenticate the same wire. Two slots in one family are alternative
// shapes of the same access — an API key and an OAuth blob the delegates
// rank against each other — so a resolver filling gaps must treat the
// family, not the slot, as the unit ("don't add a second credential to a
// wire the run already holds"): the claude_code delegate ranks a ctx API
// key (and a z.ai facade key) above a ctx OAuth dir, so pairing a fallback
// key with a run's own forfait would silently switch every call onto the
// fallback. Slots outside the two shared wires are their own family.
func WireFamily(slot string) string {
	switch slot {
	case string(ProviderAnthropic), string(ProviderZAI), string(OAuthKindClaudeCode):
		return "anthropic-wire"
	case string(ProviderOpenAI), string(OAuthKindCodex):
		return "openai-wire"
	default:
		return slot
	}
}

// Fingerprint returns the short audit fingerprint of the credential that
// filled slot (a Provider name or an OAuth kind), or "" when the slot is
// empty or predates fingerprinting.
func (c Credentials) Fingerprint(slot string) string {
	if c.Fingerprints == nil {
		return ""
	}
	return c.Fingerprints[slot]
}

// APIKey returns the plaintext API key for the requested provider
// (or "" when none is configured for the run).
func (c Credentials) APIKey(p Provider) string {
	if c.APIKeys == nil {
		return ""
	}
	return c.APIKeys[p]
}

func (c Credentials) GenericSecret(name string) string {
	if c.Generic == nil {
		return ""
	}
	return c.Generic[name]
}

// OAuthDir returns the temp dir holding sealed credentials for kind
// (claude_code / codex), or "" when no OAuth bundle was injected.
func (c Credentials) OAuthDir(kind string) string {
	if c.OAuthCredentialFiles == nil {
		return ""
	}
	return c.OAuthCredentialFiles[kind]
}

// SubscriptionOAuthOnly reports whether the only credential available for
// provider in this ctx is the subscription OAuth token of kind (a Claude
// Pro/Max or ChatGPT plan bearer) — i.e. there is no metered API key to fall
// back on.
//
// This used to gate a refusal, on the reading that consuming a subscription
// outside the vendor's own CLI was out of policy. Anthropic's API settled it:
// it ACCEPTS the token from a third-party app and bills it against a separate
// *extra-usage* balance instead of the plan's limits, answering
//
//	400 invalid_request_error — "Third-party apps now draw from your extra
//	usage, not your plan limits. Add more at claude.ai/settings/usage"
//
// when that balance runs out. The line the vendor draws is about billing, not
// about which client. So the condition is now a NOTICE, not a bar: it still
// matters (the operator is spending a different pot than they may expect), so
// callers log SubscriptionOAuthNotice — but they proceed. See ADR-085.
//
// Returns false when the ctx carries no credentials, when an API key is
// available, or when no OAuth credential of that kind is present.
func SubscriptionOAuthOnly(ctx context.Context, provider Provider, kind OAuthKind) bool {
	creds, ok := CredentialsFromContext(ctx)
	if !ok {
		return false
	}
	if creds.APIKey(provider) != "" {
		return false
	}
	return creds.OAuthDir(string(kind)) != ""
}

// SubscriptionOAuthNotice is the warning a backend logs when it is about to
// spend a subscription OAuth token outside the vendor's own CLI. Shared so
// every backend words it identically.
//
// Only Anthropic's arrangement is stated as fact, because only it was measured
// (a third-party call billed to the extra-usage balance rather than the plan).
// Asserting the same of another vendor would be inventing a billing model: the
// operator would read a confident sentence nobody verified, and act on it.
func SubscriptionOAuthNotice(provider Provider) string {
	detail := "consumption follows that vendor's policy for third-party clients"
	if provider == ProviderAnthropic {
		detail = "third-party apps bill against your EXTRA USAGE balance, not your plan " +
			"limits (top up at the provider's usage settings)"
	}
	return fmt.Sprintf(
		"using the %s subscription OAuth token: %s. Set ITERION_FORBID_SUBSCRIPTION_OAUTH=1 "+
			"to refuse this instead, or supply a metered API key.", provider, detail)
}

// ErrSubscriptionOAuthForbidden is returned when the operator has opted into
// refusing subscription-OAuth consumption outside the vendor's own CLI.
var ErrSubscriptionOAuthForbidden = errors.New(
	"secrets: refusing to spend a subscription OAuth token outside the vendor's own CLI " +
		"(ITERION_FORBID_SUBSCRIPTION_OAUTH=1); supply a metered API key, or use the " +
		"backend that spawns the vendor CLI (claude_code)")

// ForbidSubscriptionOAuth reports whether the operator has opted out of
// spending subscription tokens on third-party surfaces.
//
// The opt-out exists for shared and cloud deployments: there, consuming an
// operator's extra-usage balance is a cost decision taken on behalf of
// everyone using that instance, and it should be possible to close.
func ForbidSubscriptionOAuth() bool {
	return strings.TrimSpace(os.Getenv("ITERION_FORBID_SUBSCRIPTION_OAUTH")) == "1"
}

// anthropicOAuthTokenPrefix is the shape Anthropic mints subscription OAuth
// access tokens in, and the only reliable way to tell one apart from the other
// things ANTHROPIC_AUTH_TOKEN legitimately carries.
const anthropicOAuthTokenPrefix = "sk-ant-oat"

// IsAnthropicSubscriptionToken reports whether a bearer value is an Anthropic
// subscription OAuth token.
//
// It exists because ANTHROPIC_AUTH_TOKEN is overloaded: it is how a Claude
// subscription reaches claw/pi, but it is ALSO how the z.ai Anthropic-compatible
// facade is wired (ANTHROPIC_BASE_URL=z.ai + ANTHROPIC_AUTH_TOKEN=$ZAI_API_KEY)
// and how a gateway bearer is supplied. So the subscription opt-out must key on
// the token's shape, not on the variable's name — blanket-clearing the variable
// would break those setups, which the opt-out has no business touching.
func IsAnthropicSubscriptionToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), anthropicOAuthTokenPrefix)
}

// GuardSubscriptionOAuth returns ErrSubscriptionOAuthForbidden only when the
// operator has opted out AND the subscription token is the sole credential.
// It is the one-call form of the SubscriptionOAuthOnly + ForbidSubscriptionOAuth
// pair for callers that have no logger to warn through.
func GuardSubscriptionOAuth(ctx context.Context, provider Provider, kind OAuthKind) error {
	if !ForbidSubscriptionOAuth() {
		return nil
	}
	if !SubscriptionOAuthOnly(ctx, provider, kind) {
		return nil
	}
	return ErrSubscriptionOAuthForbidden
}

type credentialsCtxKey struct{}

// WithCredentials returns a child ctx carrying the resolved
// credentials. Empty / zero-value Credentials are still stored so
// callers can detect "we are inside a per-run scope with no keys"
// vs "no credentials ctx at all" (env fallback).
func WithCredentials(parent context.Context, c Credentials) context.Context {
	return context.WithValue(parent, credentialsCtxKey{}, c)
}

// CredentialsFromContext returns the resolved credentials and a flag
// indicating whether a per-run scope was active at all.
func CredentialsFromContext(ctx context.Context) (Credentials, bool) {
	if ctx == nil {
		return Credentials{}, false
	}
	c, ok := ctx.Value(credentialsCtxKey{}).(Credentials)
	return c, ok
}
