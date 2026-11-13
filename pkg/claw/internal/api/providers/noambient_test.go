package providers_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/anthropic"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/bedrock"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/foundry"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/moonshot"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/zai"
)

// The Anthropic-shaped providers hand the flag to the client they build, so
// the wire contract the main client carries holds on every route one of them
// serves. A provider that stops threading it re-opens the ambient read the
// flag closed.
func TestAnthropicShapedProvidersThreadNoAmbientHeaders(t *testing.T) {
	providers := map[string]func() api.Provider{
		"anthropic": func() api.Provider { return anthropic.New() },
		"zai":       func() api.Provider { return zai.New() },
		"moonshot":  func() api.Provider { return moonshot.New() },
	}
	for name, newProvider := range providers {
		for _, noAmbient := range []bool{true, false} {
			cli, err := newProvider().NewClient(api.ProviderConfig{
				APIKey: "k", Model: "m", NoAmbientHeaders: noAmbient,
				ExtraHeaders: map[string]string{"X-Explicit": "yes"},
			})
			if err != nil {
				t.Fatalf("%s: NewClient: %v", name, err)
			}
			c, ok := cli.(*api.Client)
			if !ok {
				t.Fatalf("%s: client type %T, want *api.Client", name, cli)
			}
			if c.NoAmbientHeaders != noAmbient {
				t.Errorf("%s: client NoAmbientHeaders = %t, want %t", name, c.NoAmbientHeaders, noAmbient)
			}
			if c.ExtraHeaders["X-Explicit"] != "yes" {
				t.Errorf("%s: explicit headers lost: %v", name, c.ExtraHeaders)
			}
		}
	}
}

// The foundry provider honours the flag at its own identity resolution: under
// it the ambient block and CLAW_USER_AGENT stay out of the resolved Identity,
// without it (control) they still merge under the explicit config.
func TestFoundryHonoursNoAmbientHeaders(t *testing.T) {
	t.Setenv("AZURE_OPENAI_ENDPOINT", "https://my-resource.openai.azure.com")
	t.Setenv("AZURE_OPENAI_API_KEY", "stub-key")
	t.Setenv(api.EnvCustomHeaders, "X-Operator-Secret: s3cr3t")
	t.Setenv(api.EnvUserAgent, "operator-agent/1")

	for _, noAmbient := range []bool{true, false} {
		cli, err := foundry.New().NewClient(api.ProviderConfig{
			Model: "foundry/dep-1", APIKey: "stub-key", NoAmbientHeaders: noAmbient,
			ExtraHeaders: map[string]string{"X-Explicit": "yes"},
		})
		if err != nil {
			t.Fatalf("noAmbient=%t: NewClient: %v", noAmbient, err)
		}
		id := cli.(*foundry.Client).Identity
		if _, ok := id.ExtraHeaders["X-Explicit"]; !ok {
			t.Errorf("noAmbient=%t: explicit header lost: %v", noAmbient, id.ExtraHeaders)
		}
		_, secret := id.ExtraHeaders["X-Operator-Secret"]
		ambientUA := id.UserAgent == "operator-agent/1"
		if noAmbient && (secret || ambientUA) {
			t.Errorf("noAmbient=%t: ambient identity resolved into the client (secret present: %t, ua %q)", noAmbient, secret, id.UserAgent)
		}
		if !noAmbient && !secret && !ambientUA {
			t.Errorf("noAmbient=%t: control — the ambient block should still merge without the flag (identity %v)", noAmbient, id.ExtraHeaders)
		}
	}
}

// Bedrock resolves no client identity — SigV4 signs what the SDK sets — so it
// refuses the flag by name instead of silently accepting a no-op.
func TestBedrockRefusesNoAmbientHeaders(t *testing.T) {
	_, err := bedrock.New().NewClient(api.ProviderConfig{Model: "anthropic.claude-sonnet-4-20250514-v1:0", NoAmbientHeaders: true})
	if err == nil {
		t.Fatal("bedrock accepted NoAmbientHeaders, which it can never honour")
	}
	if !strings.Contains(err.Error(), "NoAmbientHeaders") {
		t.Errorf("refusal must name the option, got %v", err)
	}
}
