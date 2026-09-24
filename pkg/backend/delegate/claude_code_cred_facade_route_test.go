package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// hostileAnthropicChannels are the ambient variables a runner pod, a
// developer shell or a baked container env can carry, each of which the CLI
// resolves on its own. A FUNDED facade route must neutralise every one: the
// cloud-provider switches outrank the facade's token in the CLI's precedence,
// and the Anthropic key and forfait token would otherwise travel to the
// facade's gateway alongside it.
var hostileAnthropicChannels = map[string]string{
	"ANTHROPIC_API_KEY":       "sk-ambient-anthropic",
	"CLAUDE_CODE_OAUTH_TOKEN": "oat-ambient-platform-forfait",
	"CLAUDE_CODE_USE_BEDROCK": "1",
	"CLAUDE_CODE_USE_VERTEX":  "1",
	"CLAUDE_CODE_USE_FOUNDRY": "1",
}

// Every route that ends on a facade — pinned or not, BYOK or process-env key,
// host or sandbox — gets the same neutralisation. One row per way in, because
// a route that builds its env elsewhere is where the next leak would be.
func TestFacadeRoute_NeutralisesEveryAmbientAnthropicChannel(t *testing.T) {
	type route struct {
		name     string
		hint     string
		byok     map[secrets.Provider]string
		envKey   string // process-env fallback variable, "" for none
		envVal   string
		wantSlot secrets.Provider
		wantKey  string
		wantBase string
	}
	routes := []route{
		{"hint_zai_byok", "zai", map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"}, "", "",
			secrets.ProviderZAI, "zai-byok", secrets.ZAIDefaultBaseURL},
		{"hint_zai_env_key", "zai", nil, "ZAI_API_KEY", "zai-env",
			secrets.ProviderZAI, "zai-env", secrets.ZAIDefaultBaseURL},
		{"hint_moonshot_byok", "moonshot", map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-byok"}, "", "",
			secrets.ProviderMoonshot, "moonshot-byok", secrets.MoonshotDefaultBaseURL},
		{"hint_moonshot_env_key", "moonshot", nil, "MOONSHOT_API_KEY", "moonshot-env",
			secrets.ProviderMoonshot, "moonshot-env", secrets.MoonshotDefaultBaseURL},
		{"default_precedence_zai_byok", "", map[secrets.Provider]string{secrets.ProviderZAI: "zai-byok"}, "", "",
			secrets.ProviderZAI, "zai-byok", secrets.ZAIDefaultBaseURL},
		{"default_precedence_moonshot_byok", "", map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-byok"}, "", "",
			secrets.ProviderMoonshot, "moonshot-byok", secrets.MoonshotDefaultBaseURL},
	}
	for _, rt := range routes {
		for _, sandboxed := range []bool{false, true} {
			name := rt.name + "/host"
			if sandboxed {
				name = rt.name + "/sandboxed"
			}
			t.Run(name, func(t *testing.T) {
				resetClaudeCredEnv(t)
				for k, v := range hostileAnthropicChannels {
					t.Setenv(k, v)
				}
				if rt.envKey != "" {
					t.Setenv(rt.envKey, rt.envVal)
				}
				ctx := context.Background()
				if rt.byok != nil {
					ctx = ctxWithCreds(t, rt.byok, nil)
				}

				got := anthropicCredEnvForCLI(ctx, rt.hint, sandboxed)

				// The route itself — or the neutralisation below proves
				// nothing about a facade.
				if got["ANTHROPIC_BASE_URL"] != rt.wantBase || got["ANTHROPIC_AUTH_TOKEN"] != rt.wantKey {
					t.Fatalf("not the %s route: base=%q token=%q", rt.wantSlot, got["ANTHROPIC_BASE_URL"], got["ANTHROPIC_AUTH_TOKEN"])
				}
				if fp := providerFingerprint(got); !strings.HasPrefix(fp, facadeSourcePrefix+string(rt.wantSlot)+":") {
					t.Errorf("providerFingerprint = %q, want the %s facade label", fp, rt.wantSlot)
				}
				for k := range hostileAnthropicChannels {
					if v, present := got[k]; !present || v != "" {
						t.Errorf("%s must be present-and-empty on a %s route (the spawn inherits the ambient value otherwise): present=%v val=%q",
							k, rt.wantSlot, present, v)
					}
				}
			})
		}
	}
}

// The ZAI_API_KEY shortcut auto-routes a host with NO Anthropic route of its
// own. A cloud-provider switch is one, and zaiEnv now clears it — so the
// shortcut honoured past a switch would silently turn a Bedrock/Vertex/Foundry
// host into a z.ai one. It stands down instead, and the inherited env decides.
func TestAnthropicCredEnv_ZAIShortcutStandsDownOnACloudProviderSwitch(t *testing.T) {
	for _, sw := range cloudProviderSwitches {
		t.Run(sw, func(t *testing.T) {
			resetClaudeCredEnv(t)
			for _, other := range cloudProviderSwitches {
				t.Setenv(other, "")
			}
			t.Setenv("ZAI_API_KEY", "zai-from-env")
			t.Setenv(sw, "1")
			if got := anthropicCredEnvForCLI(context.Background(), "", false); got != nil {
				t.Errorf("with %s set, the unpinned ZAI_API_KEY shortcut routed %v — want nil (the inherited env decides)", sw, got)
			}
		})
	}
	// …and it still fires on a host with no Anthropic route configured, so
	// the stand-down above is a discrimination, not a removal.
	resetClaudeCredEnv(t)
	for _, sw := range cloudProviderSwitches {
		t.Setenv(sw, "")
	}
	t.Setenv("ZAI_API_KEY", "zai-from-env")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oat-ambient")
	got := anthropicCredEnvForCLI(context.Background(), "", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "zai-from-env" {
		t.Fatalf("ZAI_API_KEY shortcut lost: got %v", got)
	}
	if v, present := got["CLAUDE_CODE_OAUTH_TOKEN"]; !present || v != "" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN must be present-and-empty on the z.ai route: present=%v val=%q", present, v)
	}
}
