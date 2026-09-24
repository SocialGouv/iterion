package model

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestMoonshotBaseURL_DefaultAndOverride(t *testing.T) {
	t.Setenv("MOONSHOT_BASE_URL", "")
	if got := moonshotBaseURL(); got != secrets.MoonshotDefaultBaseURL {
		t.Fatalf("moonshotBaseURL() = %q, want %q", got, secrets.MoonshotDefaultBaseURL)
	}
	t.Setenv("MOONSHOT_BASE_URL", "https://api.moonshot.cn/anthropic")
	if got := moonshotBaseURL(); got != "https://api.moonshot.cn/anthropic" {
		t.Fatalf("moonshotBaseURL() = %q, want the .cn gateway override", got)
	}
}

// ANTHROPIC_BASE_URL is z.ai's own wiring knob: on a host configured for
// z.ai it holds z.ai's endpoint, and reading it here would send a Moonshot
// key to another vendor's gateway.
func TestMoonshotBaseURL_IgnoresAnthropicBaseURL(t *testing.T) {
	t.Setenv("MOONSHOT_BASE_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", secrets.ZAIDefaultBaseURL)
	if got := moonshotBaseURL(); got != secrets.MoonshotDefaultBaseURL {
		t.Fatalf("moonshotBaseURL() = %q, want %q — ANTHROPIC_BASE_URL must not retarget Moonshot", got, secrets.MoonshotDefaultBaseURL)
	}
}

// `moonshot/kimi-k2` resolves instead of dying on "unknown provider" — the
// whole point of naming the provider rather than redirecting a base URL.
func TestRegistry_MoonshotProviderRegistered(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "moonshot-test")
	t.Setenv("MOONSHOT_BASE_URL", "")

	r := NewRegistry()
	client, err := r.Resolve("moonshot/kimi-k2")
	if err != nil {
		t.Fatalf("Resolve(moonshot/kimi-k2): %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	caps, err := r.Capabilities("moonshot/kimi-k2")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.ToolCall {
		t.Error("kimi-k2 must be usable for tool calling — an agent node on a model without it is broken, not degraded")
	}
}

// A slashed Kimi alias must survive the spec split: the FIRST slash is the
// provider boundary and every one after it belongs to the model.
func TestRegistry_MoonshotKeepsSlashedAlias(t *testing.T) {
	prov, model, err := ParseModelSpec("moonshot/kimi-code/kimi-for-coding")
	if err != nil {
		t.Fatalf("ParseModelSpec: %v", err)
	}
	if prov != "moonshot" {
		t.Errorf("provider = %q, want moonshot", prov)
	}
	if model != "kimi-code/kimi-for-coding" {
		t.Errorf("model = %q, want the alias whole", model)
	}
}

// The env factory NAMES what is missing instead of building a credential-less
// client the caller happily proceeds with — that shape is #687's opaque 401
// loop, where nothing in the failure says which variable was expected.
func TestRegistry_MoonshotWithoutKeyRefusesByName(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "")
	_, err := NewRegistry().Resolve("moonshot/kimi-k2")
	if err == nil {
		t.Fatal("Resolve without a Moonshot key must fail, not build a credential-less client")
	}
	if !strings.Contains(err.Error(), "MOONSHOT_API_KEY") {
		t.Errorf("error %q does not name MOONSHOT_API_KEY", err.Error())
	}
}

// BYOK path: a per-run moonshot key builds the client even with no env key,
// which is what makes a team-scoped key spendable on a runner pod.
func TestRegistry_MoonshotProviderWithKey(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "")
	r := NewRegistry()

	SetCredentialsLookup(func(ctx context.Context) (func(string) string, bool) {
		return func(provider string) string {
			if provider == "moonshot" {
				return "byok-moonshot-key"
			}
			return ""
		}, true
	})
	t.Cleanup(func() {
		SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
			return nil, false
		})
	})

	client, err := r.ResolveWithContext(context.Background(), "moonshot/kimi-k2")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil BYOK client")
	}
	// Env-only Resolve fails — proof the BYOK factory is what served above
	// rather than a silent env fallback.
	if _, err := r.Resolve("moonshot/kimi-k2"); err == nil {
		t.Fatal("Resolve without env key should fail")
	}
}

// clawAnthropicProviderSlots decides the sandbox forfait crossing, so it must
// name exactly the keys claw's anthropic provider SPENDS — asked of the real
// env factory, one slot of the anthropic wire at a time, rather than read back
// from the list. A slot the factory spends but the list omits lets a forfait
// displace the tenant's own key; a slot the list names but the factory never
// reads keeps the forfait out of every anthropic node of a tenant holding it,
// and the platform's ambient key serves them instead.
func TestClawAnthropicProviderSlots_MatchWhatTheFactorySpends(t *testing.T) {
	for _, slot := range secrets.AnthropicWireSlotOrder {
		if secrets.OAuthKind(slot).Valid() {
			continue
		}
		t.Run(slot, func(t *testing.T) {
			envVar := byokEnvVar[secrets.Provider(slot)]
			if envVar == "" {
				t.Fatalf("slot %q has no byokEnvVar — it cannot cross the sandbox seam at all", slot)
			}
			for _, k := range []string{
				"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
				"ZAI_API_KEY", "MOONSHOT_API_KEY", "MOONSHOT_BASE_URL",
				"ITERION_FORBID_SUBSCRIPTION_OAUTH",
			} {
				t.Setenv(k, "")
			}
			// No forfait on disk: the factory's last resort must not answer.
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			key := "key-for-" + slot
			t.Setenv(envVar, key)

			client, err := NewRegistry().Resolve("anthropic/claude-haiku-4-5")
			if err != nil {
				t.Fatalf("Resolve(anthropic/…): %v", err)
			}
			cc, ok := client.(*api.Client)
			if !ok {
				t.Fatalf("anthropic factory returned %T, want *api.Client", client)
			}
			spends := cc.APIKey == key || cc.OAuthToken == key
			listed := slices.Contains(clawAnthropicProviderSlots, secrets.Provider(slot))
			if spends != listed {
				t.Errorf("with only %s set, the anthropic factory spends it: %v; clawAnthropicProviderSlots lists %q: %v — the forfait crossing decides on the wrong keys",
					envVar, spends, slot, listed)
			}
			if listed && envVar != "ANTHROPIC_API_KEY" && !slices.Contains(anthropicWireShadowEnv(), envVar) {
				t.Errorf("anthropicWireShadowEnv() = %v, missing %q — that ambient value would outrank the forfait in the container", anthropicWireShadowEnv(), envVar)
			}
		})
	}
	if heldAnthropicWireAPIKey(secrets.Credentials{}) {
		t.Error("heldAnthropicWireAPIKey reported a key on empty credentials")
	}
}
