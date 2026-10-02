package model

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	anthropicprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/anthropic"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// providerHintCtxKey carries the node's credential-routing hint (the DSL
// `provider:` field, delegate.Task.ProviderHint) from ClawBackend.Execute
// down to the registry's resolution chokepoint. The hint is operator text
// that decides WHICH VENDOR a run's spend lands on; the registry resolves
// credentials from env and per-run credentials and would otherwise never
// see it — which is how a `provider: "anthropic"` node reached z.ai
// whenever ZAI_API_KEY was set (#1718).
type providerHintCtxKey struct{}

// WithProviderHint stamps the hint onto ctx. An empty hint clears nothing —
// the default env-driven precedence applies. Exported for the claw-adjacent
// resolvers that share the registry's chokepoint outside a backend Execute
// (the supervisor's evaluator, pkg/supervise): their ResolveWithContext must
// read the same hint the supervised nodes were routed with, or the eval
// spends a credential the nodes were forbidden from.
func WithProviderHint(ctx context.Context, hint string) context.Context {
	if strings.TrimSpace(hint) == "" {
		return ctx
	}
	return context.WithValue(ctx, providerHintCtxKey{}, hint)
}

// providerHintFromContext returns the folded hint, "" when none was stamped.
func providerHintFromContext(ctx context.Context) string {
	hint, _ := ctx.Value(providerHintCtxKey{}).(string)
	return strings.ToLower(strings.TrimSpace(hint))
}

// ErrProviderHintUnfunded is the refusal a hint-pinned node gets when the
// credential its hint mandates is not reachable. The alternative the
// pre-#1718 code produced was worse in both directions: silently spending a
// credential the hint excluded (the z.ai fallback under `provider:
// "anthropic"`), or — with the facade now skipped — building an
// unauthenticated client whose every call answers 401 (#687's shape).
type ErrProviderHintUnfunded struct {
	Provider string // the hint as the operator wrote it, folded
	Model    string
}

func (e *ErrProviderHintUnfunded) Error() string {
	return fmt.Sprintf("model: node pinned to provider %q (model %s) but no credential for that route is reachable "+
		"(no run key, no resolved forfait, no environment credential the hint admits) — "+
		"refusing rather than spending a credential the hint excluded",
		e.Provider, e.Model)
}

// ErrGLMHintUnfunded is the refusal a hint-pinned GLM node gets when no z.ai
// credential is reachable. The GLM exemption from the hint branch (GLM is
// served by z.ai whatever the hint says) skips resolveAnthropicDirect — the
// only producer of ErrProviderHintUnfunded — so without its own refusal the
// node fell through to the env factory, which builds an UNAUTHENTICATED
// client: #687's opaque-401 shape, with nothing naming z.ai as the missing
// credential.
type ErrGLMHintUnfunded struct {
	Model string
}

func (e *ErrGLMHintUnfunded) Error() string {
	return fmt.Sprintf("model: glm id %q is served by z.ai even under provider: \"anthropic\" (Anthropic does not serve it), "+
		"but no z.ai credential is reachable (no run key for the route, no ZAI_API_KEY, no explicit "+
		"ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN gateway) — set ZAI_API_KEY, or route the node to a model Anthropic serves",
		e.Model)
}

// glmKeyReachableUnderHint reports whether a GLM route has anything to spend
// under the hint: the run's z.ai key (the channel ResolveWithContext's GLM
// branch reads), the ZAI_API_KEY env-fallback the env factory synthesises
// from, or an explicit anthropic-wire gateway (ANTHROPIC_BASE_URL /
// ANTHROPIC_AUTH_TOKEN) — the operator's own destination, which a hint never
// overrules (philosophy: an explicit choice is honoured, not refused).
func glmKeyReachableUnderHint(ctx context.Context) bool {
	if creds, ok := credentialsLookup(ctx); ok && creds(string(secrets.ProviderZAI)) != "" {
		return true
	}
	return os.Getenv("ZAI_API_KEY") != "" ||
		os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" ||
		os.Getenv("ANTHROPIC_BASE_URL") != ""
}

// resolveAnthropicDirect builds an UNCACHED Anthropic-direct client for a
// node pinned `provider: "anthropic"` — the in-process twin of the
// claude_code delegate's hint branch
// (selectedAnthropicCredEnvForCLI): the z.ai synthesis (ZAI_API_KEY →
// z.ai base URL), ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN are all
// skipped, so the node's spend cannot land on a vendor the hint excluded.
// Uncached on purpose: the registry cache is keyed by model spec alone,
// and two nodes in one process may hold different hints for the same spec.
//
// Precedence mirrors ResolveWithContext minus the facades: the run's own
// key, then the resolved Claude forfait, then the ambient ANTHROPIC_API_KEY,
// then the desktop forfait on disk — and a typed refusal when none answers.
func (r *Registry) resolveAnthropicDirect(ctx context.Context, modelID string) (api.APIClient, error) {
	p := anthropicprovider.New()
	if creds, ok := credentialsLookup(ctx); ok {
		if k := creds(string(secrets.ProviderAnthropic)); k != "" {
			return p.NewClient(withClientIdentity(api.ProviderConfig{APIKey: k, Model: modelID}))
		}
	}
	// The forfait on the DIRECT wire: the empty base URL stands in for the
	// ANTHROPIC_BASE_URL the hint skips, so AnthropicForfaitWireOK cannot
	// refuse a forfait because of a z.ai value the hint already overruled.
	if client, ok, err := r.anthropicFromCtxForfaitOnWire(ctx, modelID, ""); ok {
		return client, err
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return p.NewClient(withClientIdentity(api.ProviderConfig{APIKey: k, Model: modelID}))
	}
	if tok := secrets.AnthropicForfaitAccessTokenFromDisk(); tok != "" {
		// Same guard as the env factory: the opt-out is enforced at the
		// point the subscription token becomes the client's credential.
		if secrets.ForbidSubscriptionOAuth() {
			return nil, fmt.Errorf("claw: %w", secrets.ErrSubscriptionOAuthForbidden)
		}
		claudeForfaitWarnOnce.Do(func() {
			iterlog.NewFromEnv(os.Stderr).Warn("claw: %s",
				secrets.SubscriptionOAuthNotice(secrets.ProviderAnthropic))
		})
		return p.NewClient(withClientIdentity(api.ProviderConfig{Model: modelID, OAuthToken: tok}))
	}
	return nil, &ErrProviderHintUnfunded{Provider: "anthropic", Model: modelID}
}
