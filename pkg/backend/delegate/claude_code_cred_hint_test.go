package delegate

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A provider hint is operator TEXT, and nothing between the bot file and the
// routing switch folds its case: ir.SplitProviderStep trims a `provider:model`
// element but never cases it, a launch-time override is stored verbatim, and
// the compiler's unknown-hint diagnostic (C087) is a warning. A switch written
// on the exact literal therefore let `provider: "Moonshot"` miss every facade
// branch and fall through to the DEFAULT precedence — the run's Anthropic
// credential, a different account and a different bill, with nothing said.
//
// That is the exact failure the named refusal exists to prevent, defeated by a
// capital letter. The hint decides WHICH ACCOUNT PAYS, so folding it is not a
// style question: the sibling sites that already fold (piResolveModel,
// piUsageSource) do not route, and this one does.
func TestAnthropicCredEnv_FacadeHintIsFoldedBeforeRouting(t *testing.T) {
	for _, hint := range []string{"moonshot", "Moonshot", "MOONSHOT", "MoonShot", " moonshot", "moonshot ", "\tmoonshot\n"} {
		t.Run("unfunded_"+strings.TrimSpace(hint), func(t *testing.T) {
			resetClaudeCredEnv(t)
			// The tempting fallback, ambient and in the bundle at once.
			t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-anthropic")
			ctx := ctxWithCreds(t,
				map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ctx-anthropic"},
				map[string]string{string(secrets.OAuthKindClaudeCode): t.TempDir()},
			)

			got := anthropicCredEnvForCLI(ctx, hint, false)

			if v, present := got["ANTHROPIC_API_KEY"]; !present || v != "" {
				t.Errorf("hint %q routed to an Anthropic credential: ANTHROPIC_API_KEY present=%v val=%q — want the suppression map",
					hint, present, v)
			}
			if fp := providerFingerprint(got); fp != "anthropic-suppressed" {
				t.Errorf("hint %q: providerFingerprint = %q, want %q", hint, fp, "anthropic-suppressed")
			}
			err := facadeHintRefusal(hint, got)
			if err == nil {
				t.Fatalf("hint %q: no refusal — an unfunded moonshot node must be refused by name, whatever the spelling", hint)
			}
			if !strings.Contains(err.Error(), "MOONSHOT_API_KEY") {
				t.Errorf("hint %q: refusal %q does not name MOONSHOT_API_KEY", hint, err.Error())
			}
		})

		t.Run("funded_"+strings.TrimSpace(hint), func(t *testing.T) {
			resetClaudeCredEnv(t)
			ctx := ctxWithCreds(t, map[secrets.Provider]string{
				secrets.ProviderAnthropic: "sk-ctx-anthropic",
				secrets.ProviderMoonshot:  "moonshot-test",
			}, nil)

			got := anthropicCredEnvForCLI(ctx, hint, false)

			if got["ANTHROPIC_AUTH_TOKEN"] != "moonshot-test" {
				t.Errorf("hint %q: ANTHROPIC_AUTH_TOKEN = %q, want the run's Moonshot key", hint, got["ANTHROPIC_AUTH_TOKEN"])
			}
			if got["ANTHROPIC_BASE_URL"] != secrets.MoonshotDefaultBaseURL {
				t.Errorf("hint %q: ANTHROPIC_BASE_URL = %q, want %q", hint, got["ANTHROPIC_BASE_URL"], secrets.MoonshotDefaultBaseURL)
			}
			if err := facadeHintRefusal(hint, got); err != nil {
				t.Errorf("hint %q: funded node refused: %v", hint, err)
			}
		})
	}
}

// The class, not the site: z.ai reads the same switch, and so does the
// "anthropic" branch — whose miss is the same silence in the other direction.
// A run holding both keys with `provider: "Anthropic"` served the z.ai facade,
// which is precisely what that branch was written to forbid.
func TestAnthropicCredEnv_ZaiAndAnthropicHintsAreFoldedToo(t *testing.T) {
	for _, hint := range []string{"ZAI", " Zai ", "zAI"} {
		t.Run("zai_"+strings.TrimSpace(hint), func(t *testing.T) {
			resetClaudeCredEnv(t)
			ctx := ctxWithCreds(t, map[secrets.Provider]string{
				secrets.ProviderAnthropic: "sk-ctx-anthropic",
				secrets.ProviderZAI:       "zai-test",
			}, nil)
			got := anthropicCredEnvForCLI(ctx, hint, false)
			if got["ANTHROPIC_AUTH_TOKEN"] != "zai-test" || got["ANTHROPIC_BASE_URL"] != secrets.ZAIDefaultBaseURL {
				t.Errorf("hint %q served %v, want the z.ai facade", hint, got)
			}
		})
	}
	for _, hint := range []string{"Anthropic", "ANTHROPIC", " anthropic "} {
		t.Run("anthropic_"+strings.TrimSpace(hint), func(t *testing.T) {
			resetClaudeCredEnv(t)
			// z.ai heads the default precedence, so an unfolded "Anthropic"
			// does not merely miss its branch — it serves the OTHER vendor.
			ctx := ctxWithCreds(t, map[secrets.Provider]string{
				secrets.ProviderAnthropic: "sk-ctx-anthropic",
				secrets.ProviderZAI:       "zai-test",
			}, nil)
			got := anthropicCredEnvForCLI(ctx, hint, false)
			if got["ANTHROPIC_API_KEY"] != "sk-ctx-anthropic" {
				t.Errorf("hint %q: ANTHROPIC_API_KEY = %q, want the run's Anthropic key", hint, got["ANTHROPIC_API_KEY"])
			}
			if got["ANTHROPIC_BASE_URL"] != "" {
				t.Errorf("hint %q: ANTHROPIC_BASE_URL = %q, want it cleared — the hint forces Anthropic-direct", hint, got["ANTHROPIC_BASE_URL"])
			}
		})
	}
}
