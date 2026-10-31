package secrets

import (
	"slices"
	"testing"
)

// FacadeVendorEndpoints is the vendor-published endpoint ensemble the
// substitution verdicts judge: the default each resolver stamps BESIDE the
// second hosts of the same Anthropic wire — Moonshot's .cn gateway and
// Zhipu's open.bigmodel.cn for the z.ai family. A mislabeled claude id must
// not slip through because the operator (or the facade's own docs) spelled
// the vendor's other host.
func TestFacadeVendorEndpointsHoldsEveryPublishedHost(t *testing.T) {
	zai := FacadeVendorEndpoints(ProviderZAI)
	if !slices.Equal(zai, []string{ZAIDefaultBaseURL, ZAIChinaBaseURL}) {
		t.Errorf("z.ai ensemble = %v, want [%s %s] — the default and Zhipu's mainland endpoint of the same family",
			zai, ZAIDefaultBaseURL, ZAIChinaBaseURL)
	}

	moonshot := FacadeVendorEndpoints(ProviderMoonshot)
	if !slices.Equal(moonshot, []string{MoonshotDefaultBaseURL, MoonshotChinaBaseURL}) {
		t.Errorf("moonshot ensemble = %v, want [%s %s] — the default and the .cn gateway of the same wire",
			moonshot, MoonshotDefaultBaseURL, MoonshotChinaBaseURL)
	}

	for _, other := range []Provider{ProviderAnthropic, ProviderOpenAI, ProviderBedrock,
		ProviderVertex, ProviderAzure, ProviderOpenRouter, ProviderXAI,
		ProviderOpenAICompatible, Provider("nonesuch")} {
		if got := FacadeVendorEndpoints(other); got != nil {
			t.Errorf("FacadeVendorEndpoints(%q) = %v, want nil — a provider that names no Anthropic-wire facade publishes no vendor endpoint",
				other, got)
		}
	}

	// The ensemble is the tree's own facts, spelled exactly as published.
	if MoonshotChinaBaseURL != "https://api.moonshot.cn/anthropic" {
		t.Errorf("MoonshotChinaBaseURL = %q, want the .cn gateway of the .ai wire", MoonshotChinaBaseURL)
	}
	if ZAIChinaBaseURL != "https://open.bigmodel.cn/api/anthropic" {
		t.Errorf("ZAIChinaBaseURL = %q, want Zhipu's published Anthropic-compatible path", ZAIChinaBaseURL)
	}
}
