package model

// Serial (Setenv): the registry's factories read the process environment.

import (
	"context"
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

// The vendor's OWN base URL is not the operator's claim: ZAI_API_KEY plus
// ANTHROPIC_BASE_URL pointed at z.ai's documented endpoint is the exact
// wiring z.ai publishes, the delegate refuses a claude id on it, and the
// registry built the client — GLM served under the claude label with the
// run record blessing the mislabel. The verdict judges the endpoint with
// the delegate's own fold (case, spacing, trailing slash), so a cosmetic
// spelling of the vendor URL buys no exemption either.
func TestClawRegistryRefusesTheVendorBaseURLForAClaudeID(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "sk-zai-fixture")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")

	for _, base := range []string{
		secrets.ZAIDefaultBaseURL,
		"  " + strings.ToUpper(secrets.ZAIDefaultBaseURL) + "/  ",
	} {
		t.Setenv("ANTHROPIC_BASE_URL", base)
		_, err := NewRegistry().Resolve("anthropic/claude-opus-5-5")
		var unavailable *delegate.ErrModelUnavailable
		if !errors.As(err, &unavailable) {
			t.Fatalf("Resolve(anthropic/claude-opus-5-5) with ANTHROPIC_BASE_URL=%q = %v, want the typed refusal — "+
				"the vendor's own base URL is the facade's wiring, not an operator's claim", base, err)
		}
		if unavailable.Provider != string(secrets.ProviderZAI) {
			t.Errorf("Provider = %q, want zai", unavailable.Provider)
		}
	}

	// The id the vendor serves keeps the same env as its route: the base
	// URL the operator set is where the GLM call goes.
	t.Setenv("ANTHROPIC_BASE_URL", secrets.ZAIDefaultBaseURL)
	client, err := NewRegistry().Resolve("anthropic/glm-5.3")
	if err != nil {
		t.Fatalf("anthropic/glm-5.3 on the vendor base URL is the route it names: %v", err)
	}
	cc, ok := client.(*api.Client)
	if !ok {
		t.Fatalf("client is %T, want the anthropic-protocol client", client)
	}
	if cc.BaseURL != secrets.ZAIDefaultBaseURL {
		t.Errorf("BaseURL = %q, want the vendor endpoint the operator's env names", cc.BaseURL)
	}
}

// Both moonshot factories judge the endpoint the way the delegate does:
// MOONSHOT_BASE_URL pointed at Moonshot's OWN published endpoint is the
// facade's wiring, not a claim — a substituted id is refused on it. An
// endpoint that genuinely differs (the enterprise proxy case) stays the
// operator's claim and passes.
func TestClawMoonshotFactoriesJudgeTheVendorBaseURLAsTheFacade(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "sk-moonshot-fixture")

	for _, base := range []string{
		"", // unset: the published endpoint is what the factory resolves to
		secrets.MoonshotDefaultBaseURL,
		strings.ToUpper(secrets.MoonshotDefaultBaseURL) + "/",
	} {
		t.Setenv("MOONSHOT_BASE_URL", base)
		if _, err := NewRegistry().Resolve("moonshot/claude-opus-5-5"); err == nil ||
			!strings.Contains(err.Error(), "moonshot") {
			t.Fatalf("env factory: Resolve(moonshot/claude-opus-5-5) with MOONSHOT_BASE_URL=%q = %v, "+
				"want the facade-substitution refusal", base, err)
		}
		client, err := NewRegistry().Resolve("moonshot/kimi-k2")
		if err != nil {
			t.Errorf("env factory: moonshot/kimi-k2 must keep resolving with MOONSHOT_BASE_URL=%q: %v", base, err)
			continue
		}
		if mc, ok := client.(*api.Client); !ok || mc.BaseURL != moonshotBaseURL() {
			t.Errorf("env factory: kimi client base = %v, want %q", client, moonshotBaseURL())
		}
	}

	// A destination that differs from the vendor default is the claim: the
	// proxy may genuinely serve the claude id.
	t.Setenv("MOONSHOT_BASE_URL", "https://moonshot-gateway.example/anthropic")
	client, err := NewRegistry().Resolve("moonshot/claude-opus-5-5")
	if err != nil {
		t.Fatalf("env factory: an operator-named moonshot gateway must not be refused: %v", err)
	}
	if mc, ok := client.(*api.Client); !ok || mc.BaseURL != "https://moonshot-gateway.example/anthropic" {
		t.Errorf("env factory: claude client base = %v, want the operator's gateway", client)
	}
}

// The BYOK factory buys the same verdict, never a second one: a tenant key
// spent on the vendor's own endpoint is refused for a substituted id, and a
// gateway the operator named is honoured.
func TestClawMoonshotBYOKJudgesTheVendorBaseURLAsTheFacade(t *testing.T) {
	t.Cleanup(SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
		return func(provider string) string {
			if provider == string(secrets.ProviderMoonshot) {
				return "sk-moonshot-byok"
			}
			return ""
		}, true
	}))

	t.Setenv("MOONSHOT_BASE_URL", secrets.MoonshotDefaultBaseURL)
	if _, err := NewRegistry().ResolveWithContext(context.Background(), "moonshot/claude-opus-5-5"); err == nil ||
		!strings.Contains(err.Error(), "moonshot") {
		t.Fatalf("BYOK factory: ResolveWithContext(moonshot/claude-opus-5-5) on the vendor base URL = %v, "+
			"want the facade-substitution refusal", err)
	}

	t.Setenv("MOONSHOT_BASE_URL", "https://moonshot-gateway.example/anthropic")
	if _, err := NewRegistry().ResolveWithContext(context.Background(), "moonshot/claude-opus-5-5"); err != nil {
		t.Errorf("BYOK factory: an operator-named moonshot gateway must not be refused: %v", err)
	}
}
