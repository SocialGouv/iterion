package delegate

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// An operator who sets ANTHROPIC_BASE_URL means it: a self-hosted z.ai-
// compatible endpoint, a regional facade, a debugging proxy. The two branches
// that read the key from the process env honoured it; the two that read a
// TENANT-provisioned key pinned the vendor default instead, so the same
// deployment routed a team's key somewhere the operator's own key would never
// go. One resolver for all four.
func TestAnthropicCredEnv_ZAIHonoursTheOperatorsBaseURL(t *testing.T) {
	const override = "https://zai.internal.example/api/anthropic"

	t.Run("tenant key under the zai hint", func(t *testing.T) {
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", override)
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-tenant"}, nil)
		got := anthropicCredEnvForCLI(ctx, "zai", false)
		if got["ANTHROPIC_BASE_URL"] != override {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want the operator's %q", got["ANTHROPIC_BASE_URL"], override)
		}
		if got["ANTHROPIC_AUTH_TOKEN"] != "zai-tenant" {
			t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want the tenant key", got["ANTHROPIC_AUTH_TOKEN"])
		}
	})

	t.Run("tenant key under the default precedence", func(t *testing.T) {
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", override)
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-tenant"}, nil)
		got := anthropicCredEnvForCLI(ctx, "", false)
		if got["ANTHROPIC_BASE_URL"] != override {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want the operator's %q", got["ANTHROPIC_BASE_URL"], override)
		}
		if got["ANTHROPIC_AUTH_TOKEN"] != "zai-tenant" {
			t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want the tenant key", got["ANTHROPIC_AUTH_TOKEN"])
		}
	})

	// Unset, the vendor default stands — the override is an override, not a
	// requirement.
	t.Run("no override keeps the vendor default", func(t *testing.T) {
		resetClaudeCredEnv(t)
		ctx := ctxWithCreds(t, map[secrets.Provider]string{secrets.ProviderZAI: "zai-tenant"}, nil)
		for _, hint := range []string{"zai", ""} {
			got := anthropicCredEnvForCLI(ctx, hint, false)
			if got["ANTHROPIC_BASE_URL"] != secrets.ZAIDefaultBaseURL {
				t.Errorf("hint %q: ANTHROPIC_BASE_URL = %q, want %q", hint, got["ANTHROPIC_BASE_URL"], secrets.ZAIDefaultBaseURL)
			}
		}
	})
}
