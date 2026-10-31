package model

// Serial (Setenv): the registry's factories read the process environment.

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Claw parity (the backends' rule): the claw registry's ZAI_API_KEY
// synthesis reroutes an anthropic-wire call to z.ai when no Anthropic
// credential is reachable — for a GLM id that IS the route; for a claude
// id it is the same silent substitution the claude_code delegate refuses,
// so the registry refuses it with the same typed carrier.
func TestClawRegistryRefusesTheZAISubstitutionOfAClaudeID(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "sk-zai-fixture")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")

	_, err := NewRegistry().Resolve("anthropic/claude-opus-5-5")
	var unavailable *delegate.ErrModelUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("Resolve(anthropic/claude-opus-5-5) with only ZAI_API_KEY = %v, want a typed ErrModelUnavailable — "+
			"the z.ai synthesis would serve GLM under the claude label", err)
	}
	if unavailable.Provider != string(secrets.ProviderZAI) {
		t.Errorf("Provider = %q, want zai", unavailable.Provider)
	}

	// A GLM id IS the z.ai route — served, never refused.
	if _, err := NewRegistry().Resolve("anthropic/glm-5.3"); err != nil {
		t.Errorf("anthropic/glm-5.3 must keep resolving to z.ai: %v", err)
	}
}

// The operator's own gateway is their claim on claw too: an explicit
// ANTHROPIC_BASE_URL points the wire at a destination that may genuinely
// serve the id, and the vendor-substitution verdict does not reach it.
func TestClawRegistryHonoursTheOperatorsGatewayForAClaudeID(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example/anthropic")
	t.Setenv("ZAI_API_KEY", "sk-zai-fixture")

	client, err := NewRegistry().Resolve("anthropic/claude-opus-5-5")
	if err != nil {
		t.Fatalf("an operator-named endpoint must not be refused: %v", err)
	}
	cc, ok := client.(*api.Client)
	if !ok {
		t.Fatalf("client is %T, want the anthropic-protocol client", client)
	}
	if cc.BaseURL != "https://gateway.example/anthropic" {
		t.Errorf("BaseURL = %q, want the operator's gateway", cc.BaseURL)
	}
}

// The moonshot factory on claw is the same facade shape: a non-kimi id
// would be substituted under its label.
func TestClawRegistryRefusesTheMoonshotSubstitution(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "sk-moonshot-fixture")
	t.Setenv("MOONSHOT_BASE_URL", "")

	if _, err := NewRegistry().Resolve("moonshot/claude-opus-5-5"); err == nil ||
		!strings.Contains(err.Error(), "moonshot") {
		t.Fatalf("Resolve(moonshot/claude-opus-5-5) = %v, want the facade-substitution refusal", err)
	}
	if _, err := NewRegistry().Resolve("moonshot/kimi-k2"); err != nil {
		t.Errorf("moonshot/kimi-k2 must keep resolving: %v", err)
	}
}
