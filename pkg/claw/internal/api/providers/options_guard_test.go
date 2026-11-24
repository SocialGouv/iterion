package providers_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/anthropic"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/bedrock"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/foundry"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/moonshot"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/vertex"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/zai"
)

const refusal = "honoured only by the OpenAI provider"

// Options only the OpenAI provider honours are refused by every other
// provider, so a caller relying on one of them — an egress-policy client —
// learns that it did not apply. NoAmbientHeaders is not in the table: every
// identity-resolving provider honours it (TestAnthropicShapedProvidersThreadNoAmbientHeaders,
// TestFoundryHonoursNoAmbientHeaders) and bedrock, which resolves none,
// refuses it by name (TestBedrockRefusesNoAmbientHeaders).
func TestOpenAIOnlyOptionsRefusedByOtherProviders(t *testing.T) {
	providers := map[string]api.Provider{
		"anthropic": anthropic.New(),
		"bedrock":   bedrock.New(),
		"foundry":   foundry.New(),
		"moonshot":  moonshot.New(),
		"vertex":    vertex.New(),
		"zai":       zai.New(),
	}
	options := map[string]api.ProviderConfig{
		"HTTPClient":           {APIKey: "k", Model: "m", HTTPClient: &http.Client{}},
		"OpenAIWireAPI":        {APIKey: "k", Model: "m", OpenAIWireAPI: api.OpenAIWireChat},
		"OpenAIStreamUsage":    {APIKey: "k", Model: "m", OpenAIStreamUsage: true},
		"OpenAIModelVerbatim":  {APIKey: "k", Model: "m", OpenAIModelVerbatim: true},
		"OpenAIGenericRequest": {APIKey: "k", Model: "m", OpenAIGenericRequest: true},
	}
	for name, p := range providers {
		for option, cfg := range options {
			_, err := p.NewClient(cfg)
			if err == nil || !strings.Contains(err.Error(), refusal) || !strings.Contains(err.Error(), option) {
				t.Errorf("%s with %s: err = %v, want a refusal naming %s", name, option, err, option)
			}
		}
		// Control: a config setting none of them is never refused by the guard,
		// whatever else the provider needs from its environment.
		if _, err := p.NewClient(api.ProviderConfig{APIKey: "k", Model: "m"}); err != nil && strings.Contains(err.Error(), refusal) {
			t.Errorf("%s: the guard refused a config setting none of the options: %v", name, err)
		}
	}
}
