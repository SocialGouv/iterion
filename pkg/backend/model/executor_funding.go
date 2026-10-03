package model

import (
	"context"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// anthropicFunding names how the ANTHROPIC wire is funded right now, in ""
// = nothing the wire would accept. It is the PROBE the D6-R2 disposition
// extracted from the anthropic factory: the same inputs, read the same way,
// so a default the wire cannot serve is refused BEFORE dispatch instead of
// surfacing as a 401 loop with the knob invisible. The host env it reads is
// the same one sandboxed dispatch forwards for these names, so host-side
// funding and container-side funding agree by construction.
//
// Presence, not precedence: any ONE of these serves the wire; the factory
// keeps its own pick order.
func anthropicFunding(ctx context.Context) string {
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		// APIKeyForRoute, not APIKey: a shared-tier key PINNED for the route
		// is exactly what serves an un-routed default on claw
		// (ResolveWithContext spends it), and a probe that cannot see pins
		// refuses funded deployments.
		if creds.APIKeyForRoute(secrets.ProviderAnthropic) != "" {
			return "the run's anthropic key"
		}
		if dir := creds.OAuthDir(string(secrets.OAuthKindClaudeCode)); dir != "" {
			// Non-blank token, exactly like the resolver's funded predicate
			// (anthropicFromCtxForfaitOnWire): AnthropicForfaitToken returns
			// ("", nil) for a dir whose blob merely parses — counting it here
			// would bless a dispatch the resolver serves with an
			// unauthenticated client.
			if tok, err := secrets.AnthropicForfaitToken(dir); err == nil && tok != "" && secrets.AnthropicForfaitWireOK(os.Getenv("ANTHROPIC_BASE_URL")) {
				return "the run's Claude forfait"
			}
		}
	}
	// The env pair, with the factory's own shape: AUTH_TOKEN is spent via
	// the OAuth branch, which a z.ai/bigmodel base URL gates OFF — counting
	// it there would bless a dispatch the factory sends out credential-less.
	base := strings.ToLower(os.Getenv("ANTHROPIC_BASE_URL"))
	isZAI := strings.Contains(base, "z.ai") || strings.Contains(base, "bigmodel")
	if os.Getenv("ANTHROPIC_API_KEY") != "" || (os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" && !isZAI) {
		return "the environment's anthropic credential"
	}
	// The factory's desktop fallback: this host's own Claude Code
	// subscription, exactly as the env path would spend it — the operator's
	// kill switch AND the redirected-wire gate both apply, and the probe
	// honours both (a forfait the factory would skip funds nothing).
	if !secrets.ForbidSubscriptionOAuth() &&
		secrets.AnthropicForfaitWireOK(os.Getenv("ANTHROPIC_BASE_URL")) &&
		secrets.AnthropicForfaitAccessTokenFromDisk() != "" {
		return "this host's Claude forfait"
	}
	return ""
}
