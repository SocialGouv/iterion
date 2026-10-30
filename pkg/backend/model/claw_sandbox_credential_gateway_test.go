package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// gatewayEnvClearer clears every env entry the claw predicate reads, so a
// test's verdict is its own scenario and never a launcher's ambient key.
func gatewayEnvClearer(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OPENAI_API_KEY", "AZURE_OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"ZAI_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
		"GROQ_API_KEY", "DEEPSEEK_API_KEY", "MISTRAL_API_KEY", "AWS_ACCESS_KEY_ID",
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", compatgw.APIKeyEnv, compatgw.BaseURLEnv,
	} {
		t.Setenv(name, "")
	}
	t.Setenv("HOME", t.TempDir())
}

// TestClawSandboxCredentialTheGatewayRouteIsAChannel is the #1768 witness
// for a gateway-served node: a keyless openai_compatible gateway is a
// documented valid configuration (compatgw.APIKeyEnv — "Empty is valid: a
// gateway may sit behind network auth alone"), the dispatch forwards the
// route to the container, and the predicate must answer YES on it — reading
// it as "no credential channel" refused runs the node's own dispatch would
// have served (probe 4 of the tip's adversarial review). The widening arm
// below pins the OTHER side: with no gateway env at all, the node still
// reads as no channel — the channel is the forwarded ROUTE, never the bare
// model spec.
func TestClawSandboxCredentialTheGatewayRouteIsAChannel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gatewayURL string
		want       bool
	}{
		{"keyless gateway: the route is the channel", "http://10.0.0.5:4000", true},
		{"no gateway env: still no channel", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gatewayEnvClearer(t)
			t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
			if tc.gatewayURL != "" {
				t.Setenv(compatgw.BaseURLEnv, tc.gatewayURL)
			}
			const model = "openai_compatible/operator-gateway-model"
			channel, ok := ClawSandboxCredential(context.Background(), model, delegate.SandboxHomeState{})
			if ok != tc.want {
				t.Fatalf("ClawSandboxCredential(gateway) = (%q, %v), want ok=%v", channel, ok, tc.want)
			}
			if ok && !strings.Contains(channel, compatgw.BaseURLEnv) {
				t.Fatalf("the gateway channel names nothing: %q", channel)
			}
		})
	}
}
