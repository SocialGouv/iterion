package cloudpublisher

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A bundle the walk seals today — vendor keys only, no gateway key — is
// byte for byte what it was before the PR-B'1 carriage: the URL maps and
// the ambient flag stay absent, and the fingerprint map too (the server's
// fingerprint rides with a GATEWAY key only; a vendor key's identity is
// the runner's own recompute). PR-C' breaks this witness on purpose, the
// day the walk seals a gateway key.
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
		t.Fatal("the walk sealed a gateway key — inertness is over, lift this test with PR-C'")
	}
}
