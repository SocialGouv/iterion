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
// The refusal is scoped to the vendor's own published endpoints: an
// operator who pointed the facade's base URL at any OTHER destination
// (ANTHROPIC_BASE_URL / MOONSHOT_BASE_URL) chose that endpoint — it may
// genuinely serve the id, and the engine does not second-guess an explicit
// routing choice.

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

	t.Run("the vendor's second published host is no claim", func(t *testing.T) {
		// The facades publish the same wire on TWO hosts apiece — Moonshot's
		// .cn gateway, Zhipu's open.bigmodel.cn for the z.ai family — and a
		// claude id routed to either is substituted exactly as on the
		// default: the ensemble (secrets.FacadeVendorEndpoints), not one
		// spelling of the vendor, is what the claim is judged against.
		resetClaudeCredEnv(t)
		t.Setenv("MOONSHOT_BASE_URL", secrets.MoonshotChinaBaseURL)
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-run"}, nil)
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, ctx, "moonshot", "claude-opus-5-5"),
			"moonshot", "claude-opus-5-5")

		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", secrets.ZAIChinaBaseURL)
		ctx = ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")
	})

	t.Run("an explicit default port is a respelling, not a claim", func(t *testing.T) {
		// :443 on https is the port the scheme implies — the same endpoint,
		// so the same verdict; a different port stays the operator's.
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai:443/api/anthropic")
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")

		// A padded spelling of that port is the same route: net.LookupPort
		// reads "0443" as 443, and the CLI's own URL parser drops it
		// altogether, so both backends dial api.z.ai:443 and GLM answers the
		// claude id. A port compared as a string reads it as the operator's
		// own destination and the refusal never fires.
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai:0443/api/anthropic")
		ctx = ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")

		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example:8443/anthropic")
		ctx = ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
		if err := refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"); err != nil {
			t.Errorf("an operator gateway on a non-default port must not be refused: %v", err)
		}
	})

	t.Run("a respelling of the vendor's endpoint is no claim", func(t *testing.T) {
		// The components a URL can carry without changing where it dials —
		// userinfo, the absolute-FQDN host, a fragment — are not an operator
		// naming a destination: the call still reaches the vendor, which
		// still answers a claude id with GLM. Driven through the real seam,
		// because the predicate is only half the path: the refusal reads the
		// base URL the resolver stamped.
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://token@api.z.ai./api/anthropic#frag")
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
		assertFacadeSubstitution(t,
			refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"),
			"zai", "claude-opus-5-5")

		// The same shape over ANOTHER host is the operator's claim: there the
		// vendor's name is the userinfo and gateway.example is what is dialled.
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai@gateway.example/api/anthropic")
		ctx = ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
		if err := refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"); err != nil {
			t.Errorf("a gateway carrying the vendor's name as userinfo is the operator's destination, not the vendor's: %v", err)
		}
	})

	t.Run("a base URL with no readable host is no claim", func(t *testing.T) {
		// Fail closed at the claim: a spelling url.Parse cannot read a host
		// from parses nowhere for the fold, yet several of them are spellings
		// the WHATWG parser the claude CLI spawns DOES resolve to api.z.ai —
		// it reads a lone slash or a backslash after the scheme as the
		// authority delimiter and strips the control characters out of the
		// host. Reading any of them as the operator's claim lets the mislabel
		// through, so each is refused here through the real seam: the typed
		// ErrModelUnavailable fires and a declared fallback route takes over.
		for _, tc := range []struct {
			name     string
			endpoint string
		}{
			{"a lone slash before the host", "https:/api.z.ai/api/anthropic"},
			{"an empty authority", "https:///api.z.ai/api/anthropic"},
			{"a percent-escaped host", "https://api%2Ez%2Eai/api/anthropic"},
			{"a host-shaped value with no scheme", "api%2Ez%2Eai/api/anthropic"},
			{"a newline in the host", "https://api.z.ai\n/api/anthropic"},
			{"a tab in the host", "https://api.z.ai\t/api/anthropic"},
			{"a carriage return in the host", "https://api.z.ai\r/api/anthropic"},
			{"backslashes in place of the double slash", "https:\\\\api.z.ai\\\\api\\\\anthropic"},
			{"a backslash after the scheme slash", "https:/\\\\api.z.ai/api/anthropic"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetClaudeCredEnv(t)
				t.Setenv("ANTHROPIC_BASE_URL", tc.endpoint)
				ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}, nil)
				assertFacadeSubstitution(t,
					refuseFacadeSubstitution(t, ctx, "zai", "claude-opus-5-5"),
					"zai", "claude-opus-5-5")
			})
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
// endpoint outside the vendor's published ensemble is the operator naming a
// destination — unset, or any of the vendor's own hosts however respelled
// (secrets.SameEndpoint folds every component that leaves the dialled
// authority untouched), is the facade's wiring, and the substitution verdict
// applies to it. The rows run both ways: a respelling the predicate reads as
// a claim lets the mislabel through, and a different destination it reads as
// the vendor's refuses a route nobody substituted.
func TestOperatorEndpointClaim(t *testing.T) {
	t.Run("zai", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			endpoint string
			want     bool
		}{
			{"unset is no claim", "", false},
			{"whitespace is no claim", "   ", false},
			{"the vendor's default is no claim", secrets.ZAIDefaultBaseURL, false},
			{"the family's mainland endpoint is no claim", secrets.ZAIChinaBaseURL, false},
			{"a respelling of the default is no claim", "  " + strings.ToUpper(secrets.ZAIDefaultBaseURL) + "//  ", false},
			{"a respelling of the mainland endpoint is no claim", strings.ToUpper(secrets.ZAIChinaBaseURL) + "/", false},
			{"an explicit default port names the same endpoint", "https://api.z.ai:443/api/anthropic", false},
			{"an explicit default port on the mainland endpoint", "https://open.bigmodel.cn:443/api/anthropic", false},
			{"a padded spelling of that port is the same route", "https://api.z.ai:0443/api/anthropic", false},
			{"a padded spelling on the mainland endpoint", "https://open.bigmodel.cn:0443/api/anthropic", false},
			// Respellings that leave the dialled authority untouched: the
			// call reaches the vendor, so the verdict is the vendor's
			// (secrets.SameEndpoint states the fold and its bound).
			{"userinfo does not route", "https://token@api.z.ai/api/anthropic", false},
			{"a fragment never leaves the client", "https://api.z.ai/api/anthropic#frag", false},
			{"the absolute-FQDN host is the same name", "https://api.z.ai./api/anthropic", false},
			{"the absolute-FQDN mainland host is the same name", "https://open.bigmodel.cn./api/anthropic", false},
			{"a query no vendor publishes", "https://api.z.ai/api/anthropic?x=1", false},
			{"a percent-escaped path character", "https://api.z.ai/api/anthropi%63", false},
			{"an empty path segment", "https://api.z.ai/api//anthropic", false},
			{"a dot segment", "https://api.z.ai/api/./anthropic", false},
			// A value url.Parse cannot read a host out of is no claim either,
			// whatever its shape: the verdict fails closed — a spelling that
			// parses nowhere or dials nowhere must not bless a route as the
			// operator's own. Several of these the WHATWG parser the claude
			// CLI spawns reads AS api.z.ai (a lone slash, backslashes, the
			// control characters it strips), so reading them as a claim would
			// let the mislabel through.
			{"a lone slash is a path, not an authority", "https:/api.z.ai/api/anthropic", false},
			{"an empty authority is no claim", "https:///api.z.ai/api/anthropic", false},
			{"a percent-escaped host is no claim", "https://api%2Ez%2Eai/api/anthropic", false},
			{"a host-shaped value with no scheme is no claim", "api%2Ez%2Eai/api/anthropic", false},
			{"a newline in the host is no claim", "https://api.z.ai\n/api/anthropic", false},
			{"a tab in the host is no claim", "https://api.z.ai\t/api/anthropic", false},
			{"a carriage return in the host is no claim", "https://api.z.ai\r/api/anthropic", false},
			{"backslashes in place of the double slash are no claim", "https:\\\\api.z.ai\\\\api\\\\anthropic", false},
			{"a backslash authority start is no claim", "https:/\\\\api.z.ai/api/anthropic", false},
			{"a non-default port is another destination", "https://api.z.ai:8443/api/anthropic", true},
			{"a destination that differs is the claim", "https://gateway.example/anthropic", true},
			{"the vendor's name as userinfo over another host is the claim", "https://api.z.ai@gateway.example/api/anthropic", true},
			{"a host containing the vendor's name is the claim", "https://api.z.ai.gateway.example/api/anthropic", true},
			// A port spelling the URL grammar refuses ("443.gateway.example"
			// is no port) fails the parse outright — the WHATWG parser the
			// CLI spawns fails it the same way, so nothing dials it, and the
			// verdict fails closed with the other hostless spellings above.
			{"a port-shaped label that parses nowhere is no claim", "https://api.z.ai:443.gateway.example/api/anthropic", false},
			{"another scheme is another destination", "http://api.z.ai/api/anthropic", true},
			{"another vendor's endpoint is a claim", secrets.MoonshotDefaultBaseURL, true},
			{"another vendor's second host is a claim", secrets.MoonshotChinaBaseURL, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := OperatorEndpointClaim(tc.endpoint, string(secrets.ProviderZAI)); got != tc.want {
					t.Errorf("OperatorEndpointClaim(%q, zai) = %v, want %v", tc.endpoint, got, tc.want)
				}
			})
		}
	})

	t.Run("moonshot", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			endpoint string
			want     bool
		}{
			{"unset is no claim", "", false},
			{"the default is no claim", secrets.MoonshotDefaultBaseURL, false},
			{"the .cn gateway is no claim", secrets.MoonshotChinaBaseURL, false},
			{"a respelling of the .cn gateway is no claim", strings.ToUpper(secrets.MoonshotChinaBaseURL) + "/", false},
			{"an explicit default port names the .ai endpoint", "https://api.moonshot.ai:443/anthropic", false},
			{"an explicit default port names the .cn gateway", "https://api.moonshot.cn:443/anthropic", false},
			{"a padded spelling of that port names the .ai endpoint", "https://api.moonshot.ai:0443/anthropic", false},
			{"userinfo on the .cn gateway does not route", "https://token@api.moonshot.cn/anthropic", false},
			{"the absolute-FQDN form of the .ai host", "https://api.moonshot.ai./anthropic", false},
			{"a destination that differs is the claim", "https://moonshot-gateway.example/anthropic", true},
			{"the vendor's name as userinfo over another host is the claim", "https://api.moonshot.ai@moonshot-gateway.example/anthropic", true},
			{"another vendor's endpoint is a claim", secrets.ZAIDefaultBaseURL, true},
			{"another vendor's second host is a claim", secrets.ZAIChinaBaseURL, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := OperatorEndpointClaim(tc.endpoint, string(secrets.ProviderMoonshot)); got != tc.want {
					t.Errorf("OperatorEndpointClaim(%q, moonshot) = %v, want %v", tc.endpoint, got, tc.want)
				}
			})
		}
	})

	t.Run("a slot that names no facade has no vendor endpoint", func(t *testing.T) {
		if got := OperatorEndpointClaim("", string(secrets.ProviderAnthropic)); got {
			t.Error("unset is no claim on any slot")
		}
		if got := OperatorEndpointClaim("https://api.anthropic.com", string(secrets.ProviderAnthropic)); !got {
			t.Error("any endpoint at all is the operator's on a slot that names no facade")
		}
	})
}
