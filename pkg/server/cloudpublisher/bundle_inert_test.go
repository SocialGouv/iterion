package cloudpublisher

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A bundle of vendor keys only — no gateway key — is byte for byte what it
// was before the PR-B'1 carriage: the URL maps and the ambient flag stay
// absent, and the fingerprint map too (the server's fingerprint rides with a
// GATEWAY key only; a vendor key's identity is the runner's own recompute).
// This holds for good: the gateway variant below is where PR-C' changed a
// bundle's bytes, and only for a run that seals a gateway key.
func TestEveryBundleSealedTodayIsByteForByteWhatItWas(t *testing.T) {
	p, sealer, keys, _ := audiencePublisher(t)
	// The team rows carry the fingerprints the create route stamps, so the
	// witness would catch a chokepoint that wrote them into the bundle for
	// vendor keys. seedKey stamps nothing, so the rows are built here.
	ctx := context.Background()
	now := time.Now().UTC()
	for _, tc := range []struct {
		prov secrets.Provider
		key  string
		fp   string
	}{{secrets.ProviderAnthropic, "sk-ant-team-0000", "fp-ant"}, {secrets.ProviderOpenAI, "sk-openai-team-0000", "fp-openai"}} {
		id := secrets.NewApiKeyID()
		sealed, err := secrets.SealAPIKey(sealer, id, []byte(tc.key))
		if err != nil {
			t.Fatal(err)
		}
		if err := keys.Create(ctx, secrets.ApiKey{ID: id, ScopeTeamID: "team1", Provider: tc.prov, Name: string(tc.prov),
			SealedSecret: sealed, Fingerprint: tc.fp, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform")
	b := resolveWithBot(t, p, sealer, "run-inert", "review-pr", nil)
	if len(b.APIKeys) == 0 {
		t.Fatalf("fixture: the walk sealed nothing, the inertness assertion would be vacuous: %+v", b)
	}
	if b.AmbientLLMRefused {
		t.Fatal("the ambient flag was set by a walk that cannot set it")
	}
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api_base_urls", "pinned_api_base_urls", "api_key_fingerprints"} {
		if strings.Contains(string(body), name) {
			t.Fatalf("a bundle of vendor keys carries %q: %s", name, body)
		}
	}
	if _, ok := b.APIKeys[secrets.ProviderOpenAICompatible]; ok {
		t.Fatal("the walk sealed a gateway key into a team that holds none — the fixture no longer tests a vendor-only bundle")
	}
}

// The gateway variant (#2005 PR-C'): a run whose decided gateway route the
// team's own key funds seals the PAIR — the key, its canonical URL in the
// paired map, the server's endpoint-aware fingerprint — and the ambient
// refusal of its team_gateway boundary (ADR-131 §4). The vendor key sealed
// beside it gains no URL and no fingerprint: those maps hold the gateway's
// entry alone.
func TestABundleSealingAGatewayKeyCarriesThePairAndTheAmbientRefusal(t *testing.T) {
	p, _ := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
	creds := resolveBoundaryRun(t, p, "run-inert-gw", "", mixedGatewayWorkflow(), store.RunTrustDefault, nil)
	b := openGatewayBundle(t, p, "run-inert-gw", creds)
	gw := secrets.ProviderOpenAICompatible
	if b.APIKeys[gw] != gwTeamKey || b.APIKeys[secrets.ProviderAnthropic] == "" {
		t.Fatalf("fixture: want the gateway and the anthropic keys sealed, got %v", b.APIKeys)
	}
	if !reflect.DeepEqual(b.APIBaseURLs, map[secrets.Provider]string{gw: gwURL}) {
		t.Errorf("api_base_urls = %v, want the gateway's URL alone", b.APIBaseURLs)
	}
	if !reflect.DeepEqual(b.APIKeyFingerprints, map[secrets.Provider]string{gw: gwFingerprint}) {
		t.Errorf("api_key_fingerprints = %v, want the gateway's server fingerprint alone", b.APIKeyFingerprints)
	}
	if !b.AmbientLLMRefused {
		t.Error("the bundle of a team_gateway run carries no ambient refusal")
	}
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"api_base_urls"`, `"api_key_fingerprints"`, `"ambient_llm_refused":true`} {
		if !strings.Contains(string(body), name) {
			t.Errorf("the sealed bytes lack %s: %s", name, body)
		}
	}
}
