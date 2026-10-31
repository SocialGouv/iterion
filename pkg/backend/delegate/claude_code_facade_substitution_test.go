package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A facade on the anthropic wire (z.ai, Moonshot) answers ANY model id with
// the one model it aliases it to, so the call "succeeds" and the run's
// record names a model that never ran — the review-pr primary shipping a
// claude id to z.ai and being reviewed by GLM under the claude label
// (#2772). The delegate knows the route it just resolved (FacadeSlotEnvKey)
// and the id it was asked to serve, so it refuses BEFORE the spawn, typed
// so the node's declared `fallbacks:` route — which classifies the refusal
// as `unavailable` — fires instead of the mislabel.
//
// The refusal is scoped to the vendor's own endpoint: an operator who
// pointed the facade's base URL at their own gateway (ANTHROPIC_BASE_URL /
// MOONSHOT_BASE_URL) chose that destination — it may genuinely serve the
// id, and the engine does not second-guess an explicit routing choice.

// refuseFacadeSubstitution is the test's handle on the seam: the refusal
// setupCredsAndSession applies after the credential route is resolved.
func refuseFacadeSubstitution(t *testing.T, ctx context.Context, hint, model string) error {
	t.Helper()
	b := &ClaudeCodeBackend{}
	task := Task{NodeID: "n", ProviderHint: hint, Model: model}
	_, _, _, err := b.setupCredsAndSession(ctx, task, nil)
	return err
}

func assertFacadeSubstitution(t *testing.T, err error, wantProvider, wantModel string) {
	t.Helper()
	if err == nil {
		t.Fatalf("a %s facade asked for %q must be refused before the spawn — it would serve its own model under the requested label", wantProvider, wantModel)
	}
	var unavailable *ErrModelUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("refusal is %T (%v), want *ErrModelUnavailable so the chain classifies it `unavailable` and a declared fallback fires", err, err)
	}
	if unavailable.Provider != wantProvider {
		t.Errorf("refusal.Provider = %q, want %q", unavailable.Provider, wantProvider)
	}
	if unavailable.Model != wantModel {
		t.Errorf("refusal.Model = %q, want %q", unavailable.Model, wantModel)
	}
	if got := ClassifyFallback(err, false); got != FallbackUnavailable {
		t.Errorf("ClassifyFallback = %q, want %q — a route the facade cannot serve is a model the credential cannot reach", got, FallbackUnavailable)
	}
}

func TestFacadeSubstitutionRefusal(t *testing.T) {
	glmOnly := func(t *testing.T) context.Context {
		t.Helper()
		return ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
	}

	t.Run("default precedence, GLM-only run, claude id", func(t *testing.T) {
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, glmOnly(t), "", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")
	})

	t.Run("default precedence, GLM-only run, unset model is the claude default", func(t *testing.T) {
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, glmOnly(t), "", ""),
			"zai", defaultClaudeCodeModel)
	})

	t.Run("pinned zai, claude id", func(t *testing.T) {
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, glmOnly(t), "zai", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")
	})

	t.Run("the facade outranks the anthropic key on the default precedence", func(t *testing.T) {
		// AnthropicWireSlotOrder puts facade keys first: a run holding BOTH
		// keys still serves an unpinned claude id from z.ai — substituted.
		// The refusal reroutes honestly; the spend is z.ai's either way.
		ctx := ctxWithCreds(t, map[secrets.Provider]string{
			secrets.ProviderZAI:       "zai-run",
			secrets.ProviderAnthropic: "sk-ant-run",
		}, nil)
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, ctx, "", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")
	})

	t.Run("ZAI_API_KEY desktop shortcut, claude id", func(t *testing.T) {
		resetClaudeCredEnv(t)
		t.Setenv("ZAI_API_KEY", "zai-env")
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, context.Background(), "", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")
	})

	t.Run("pinned moonshot, claude id", func(t *testing.T) {
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-run"}, nil)
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, ctx, "moonshot", "claude-opus-5-5"),
			"moonshot", "claude-opus-5-5")
	})

	t.Run("the facade serves its own family", func(t *testing.T) {
		for _, tc := range []struct{ name, hint, model string }{
			{"glm on the default precedence", "", "glm-5.3"},
			{"glm pinned to zai", "zai", "glm-5.3"},
			{"glm spelled with the wire prefix", "", "anthropic/glm-5.3"},
			{"glm spelled with the facade prefix", "", "zai/glm-5.3"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := refuseFacadeSubstitution(t, glmOnly(t), tc.hint, tc.model); err != nil {
					t.Errorf("%s on a z.ai route must proceed — the facade serves it: %v", tc.model, err)
				}
			})
		}
	})

	t.Run("kimi on moonshot", func(t *testing.T) {
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-run"}, nil)
		if err := refuseFacadeSubstitution(t, ctx, "moonshot", "kimi-k2"); err != nil {
			t.Errorf("kimi-k2 on the moonshot facade must proceed: %v", err)
		}
	})

	t.Run("the operator's own gateway is their claim", func(t *testing.T) {
		// zaiEnv honours an explicit ANTHROPIC_BASE_URL as the destination:
		// a self-hosted z.ai-compatible endpoint, a regional facade, a proxy
		// that may genuinely serve the claude id. The vendor-default
		// substitution verdict does not reach an endpoint the operator named.
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example/anthropic")
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
		if err := refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"); err != nil {
			t.Errorf("an operator-overridden facade endpoint must not be refused: %v", err)
		}
	})

	t.Run("pinned anthropic never substitutes", func(t *testing.T) {
		ctx := ctxWithCreds(t, map[secrets.Provider]string{
			secrets.ProviderZAI:       "zai-run",
			secrets.ProviderAnthropic: "sk-ant-run",
		}, nil)
		if err := refuseFacadeSubstitution(t, ctx, "anthropic", "claude-opus-5-5"); err != nil {
			t.Errorf("the Anthropic-direct pin serves the claude id itself: %v", err)
		}
	})

	t.Run("no facade on the wire, no refusal", func(t *testing.T) {
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-run"}, nil)
		if err := refuseFacadeSubstitution(t, ctx, "", "claude-opus-5-5"); err != nil {
			t.Errorf("an anthropic-funded default route serves the claude id: %v", err)
		}
	})
}

// OperatorEndpointClaim is the claim predicate both backends judge: only an
// endpoint that differs from the vendor's own is the operator naming a
// destination — unset (or the vendor's documented endpoint, however
// cosmetically respelled) is the facade's wiring, and the substitution
// verdict applies to it.
func TestOperatorEndpointClaim(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"unset is no claim", "", false},
		{"whitespace is no claim", "   ", false},
		{"the vendor's own endpoint is no claim", secrets.ZAIDefaultBaseURL, false},
		{"a respelling of the vendor endpoint is no claim", "  " + strings.ToUpper(secrets.ZAIDefaultBaseURL) + "//  ", false},
		{"a destination that differs is the claim", "https://gateway.example/anthropic", true},
		{"another vendor's endpoint is a claim", secrets.MoonshotDefaultBaseURL, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OperatorEndpointClaim(tc.endpoint, secrets.ZAIDefaultBaseURL); got != tc.want {
				t.Errorf("OperatorEndpointClaim(%q) = %v, want %v", tc.endpoint, got, tc.want)
			}
		})
	}
}
