package model

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The usage-cap meter reads the env factory's subscription refusal through
// AmbientEnvRefused: where it answers true the factory itself refuses the walk
// — no client, no account spent — and where it answers false the factory
// builds a client. Every row is judged against the real factory, never
// against a second description of its order.
func TestAmbientEnvRefused_AgreesWithTheEnvFactory(t *testing.T) {
	forfait := writeClaudeCodeCreds(t)
	const gateway = "https://llm-gw.example.test/anthropic"
	for _, tc := range []struct {
		name    string
		env     map[string]string
		spec    string
		refused bool
	}{
		{"a subscription bearer under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_AUTH_TOKEN": "sk-ant-oat-pod"},
			"anthropic/claude-sonnet-4", true},
		{"a gateway bearer under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_AUTH_TOKEN": "tok-gw"},
			"anthropic/claude-sonnet-4", true},
		{"a GLM id on the gateway under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_AUTH_TOKEN": "tok-gw"},
			"anthropic/glm-5.3", true},
		{"the host's Claude Code forfait under the switch",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "CLAUDE_CONFIG_DIR": forfait},
			"anthropic/claude-sonnet-4", true},
		{"an API key ahead of the bearer",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_API_KEY": "sk-pod", "ANTHROPIC_AUTH_TOKEN": "sk-ant-oat-pod"},
			"anthropic/claude-sonnet-4", false},
		{"a bearer bound for z.ai's own endpoint",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic", "ANTHROPIC_AUTH_TOKEN": "zai-key"},
			"anthropic/claude-sonnet-4", false},
		{"the z.ai synthesis for a GLM id",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1", "ZAI_API_KEY": "zai-pod", "ANTHROPIC_AUTH_TOKEN": "sk-ant-oat-pod"},
			"anthropic/glm-5.3", false},
		{"the switch off",
			map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-ant-oat-pod"},
			"anthropic/claude-sonnet-4", false},
		{"nothing to spend",
			map[string]string{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1"},
			"anthropic/claude-sonnet-4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{
				"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ZAI_API_KEY",
				"ITERION_FORBID_SUBSCRIPTION_OAUTH",
			} {
				t.Setenv(k, "")
			}
			// No host forfait unless the row plants one: the factory reads it last.
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := AmbientEnvRefused(tc.spec)
			_, err := NewRegistry().Resolve(tc.spec)
			factoryRefused := errors.Is(err, secrets.ErrSubscriptionOAuthForbidden)
			if got != tc.refused {
				t.Errorf("AmbientEnvRefused(%q) = %v, want %v", tc.spec, got, tc.refused)
			}
			if got != factoryRefused {
				t.Errorf("AmbientEnvRefused(%q) = %v but the factory refused=%v (err=%v) — the meter's reading disagrees with the walk it mirrors", tc.spec, got, factoryRefused, err)
			}
		})
	}

	// A claw route names its id bare as often as prefixed: both spellings are
	// one id to the walk.
	t.Run("a bare id reads as its prefixed spec", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_BASE_URL", "")
		t.Setenv("ZAI_API_KEY", "")
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-oat-pod")
		if !AmbientEnvRefused("claude-sonnet-4") {
			t.Error("AmbientEnvRefused(claude-sonnet-4) = false, want the refusal its prefixed spec gets")
		}
	})
}

// AmbientEnvFundsGLM is the meter's reading of the factory's GLM walk: under
// the switch a bearer-only gateway pod is refused outright, so the walk funds
// no ambient account; an API key on the same pod is spent, not refused.
func TestAmbientEnvFundsGLM_FollowsTheSubscriptionSwitch(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gw.example.test/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-gw")

	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	if !AmbientEnvFundsGLM() {
		t.Fatal("bench: the switch off, the gateway bearer funds no GLM id")
	}
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	if AmbientEnvFundsGLM() {
		t.Error("AmbientEnvFundsGLM() = true under the switch — the factory refuses the bearer, no account spends")
	}
	if _, err := NewRegistry().Resolve("anthropic/glm-5.3"); !errors.Is(err, secrets.ErrSubscriptionOAuthForbidden) {
		t.Errorf("bench: the factory resolved the GLM id under the switch (err=%v) — the row would pin a refusal it never makes", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-gw")
	if !AmbientEnvFundsGLM() {
		t.Error("AmbientEnvFundsGLM() = false with an API key ahead of the bearer — the factory spends the key")
	}
}
