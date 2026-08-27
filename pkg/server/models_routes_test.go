package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func getModelCaps(t *testing.T, base, spec, provider string) (modelCapabilitiesResponse, int) {
	t.Helper()
	q := url.Values{"spec": {spec}}
	if provider != "" {
		q.Set("provider", provider)
	}
	resp, err := http.Get(base + "/api/model-capabilities?" + q.Encode())
	if err != nil {
		t.Fatalf("GET model-capabilities: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return modelCapabilitiesResponse{}, resp.StatusCode
	}
	var out modelCapabilitiesResponse
	decodeJSONResp(t, resp, &out)
	return out, http.StatusOK
}

// The qualified path is the full answer: limits, published prices, the
// capability flags, and the source that tells a client how long the answer is
// worth caching.
func TestModelCapabilities_QualifiedSpec(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"anthropic/claude-sonnet-4-6": {
			ContextWindow: 1_000_000, MaxOutputTokens: 64_000,
			InputCostPerM: 3, OutputCostPerM: 15,
		},
	})))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "anthropic/claude-sonnet-4-6", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Source != string(model.SourceAggregator) {
		t.Errorf("Source = %q, want aggregator", got.Source)
	}
	if got.ContextWindow != 1_000_000 || got.MaxOutputTokens != 64_000 {
		t.Errorf("limits = %d/%d, want 1000000/64000", got.ContextWindow, got.MaxOutputTokens)
	}
	if got.InputCostPerM != 3 || got.OutputCostPerM != 15 {
		t.Errorf("price = %v/%v, want 3/15", got.InputCostPerM, got.OutputCostPerM)
	}
	if got.ToolCall == nil || !*got.ToolCall {
		t.Error("qualified spec must carry the capability flags")
	}
}

// A model the aggregator does not carry still answers — from the curated
// table — and says so, because "curated" is what tells a client the answer can
// still improve when the background refresh lands.
func TestModelCapabilities_CuratedSourceCanStillImprove(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(nil)))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "anthropic/glm-5.2", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Source != string(model.SourceCurated) {
		t.Errorf("Source = %q, want curated", got.Source)
	}
	if got.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want the curated 1000000", got.ContextWindow)
	}
	if got.InputCostPerM != 0 || got.OutputCostPerM != 0 {
		t.Errorf("price = %v/%v, want 0/0 — unknown, and a client must not print it as free",
			got.InputCostPerM, got.OutputCostPerM)
	}
}

// A BARE id must not 400. `.bot` files pin bare ids and a studio picker holds
// one while the operator types, so rejecting it would blank the caption in the
// common case.
func TestModelCapabilities_BareIDIsServedNotRejected(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"anthropic/claude-opus-5": {
			ContextWindow: 1_000_000, MaxOutputTokens: 64_000,
			InputCostPerM: 5, OutputCostPerM: 25,
		},
	})))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "claude-opus-5", "")
	if status != http.StatusOK {
		t.Fatalf("bare id status = %d, want 200", status)
	}
	if got.Source != string(model.SourceAggregator) {
		t.Errorf("Source = %q, want aggregator", got.Source)
	}
	if got.ContextWindow != 1_000_000 || got.InputCostPerM != 5 {
		t.Errorf("bare answer = %d ctx / %v in, want 1000000 / 5", got.ContextWindow, got.InputCostPerM)
	}
	// No provider was named, so no curated branch could be taken — the flags
	// must be OMITTED rather than defaulted to a false nothing supports.
	if got.Reasoning != nil || got.ToolCall != nil || got.Temperature != nil {
		t.Errorf("bare answer carries capability flags (%v/%v/%v); a bare id names no provider to resolve them against",
			got.Reasoning, got.ToolCall, got.Temperature)
	}
	if got.Provider != "" {
		t.Errorf("Provider = %q, want empty — none was supplied and none may be invented", got.Provider)
	}
}

// An explicit provider is honoured: the caller genuinely had one, so the answer
// upgrades to the qualified path.
func TestModelCapabilities_ExplicitProviderQualifiesTheBareID(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"anthropic/claude-opus-5": {ContextWindow: 1_000_000, InputCostPerM: 5, OutputCostPerM: 25},
	})))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "claude-opus-5", "anthropic")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Provider != "anthropic" || got.Spec != "anthropic/claude-opus-5" {
		t.Errorf("resolved = %q / %q, want anthropic / anthropic/claude-opus-5", got.Provider, got.Spec)
	}
	if got.ToolCall == nil {
		t.Error("a provider was supplied, so the capability flags must resolve")
	}
}

// A bare id nothing knows still answers 200 with zeros — "unknown", which the
// caption renders as no information rather than as a free, context-less model.
func TestModelCapabilities_UnknownBareIDIsEmptyNotAnError(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(nil)))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "some-vendor-model-nobody-ships", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Source != string(model.SourceCurated) {
		t.Errorf("Source = %q, want curated", got.Source)
	}
	if got.ContextWindow != 0 || got.MaxOutputTokens != 0 || got.InputCostPerM != 0 {
		t.Errorf("unknown model answered %+v, want zeros", got)
	}
}

func TestModelCapabilities_MissingSpecIs400(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Get(hs.URL + "/api/model-capabilities")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
