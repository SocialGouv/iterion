package cloudpublisher

import (
	"reflect"
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// refusedPinnedFixture seeds the knob's scenario: a PLATFORM-tier key of
// provider refused by its usage cap, beside a platform Claude forfait that
// fills the same wire family, with the run pinning that provider — the
// shape whose restore lands sealPinnedOnly on a taken family.
func refusedPinnedFixture(t *testing.T, provider secrets.Provider, rec *platformcfg.PlatformCredentials) *Publisher {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, provider, "sk-"+string(provider)+"-platform", "fp-"+string(provider)+"-platform")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform-forfait")
	st := usagecap.NewMemStore()
	recordRefusal(t, st, usagecap.ScopePlatform, "fp-"+string(provider)+"-platform")
	return &Publisher{
		apiKeys: keys, oauthForfait: oauth,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: testLogger(), usageCaps: st,
		platformAudience: audienceResolver(rec, nil),
	}
}

func parkRecord() *platformcfg.PlatformCredentials {
	return &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{RefusedPinnedKey: llmroute.RefusedPinnedPark}}
}

// The arbitrated default (#1999, ADR-121 § Arbitrated 1): a refused shared
// key stays OUT of the family the forfait holds — the routes naming its
// provider spend the forfait (claw bills it as extra usage) instead of
// parking on the key's own refusal.
func TestRefusedPinnedKey_DefaultForfaitKeepsTheKeyOut(t *testing.T) {
	p := refusedPinnedFixture(t, secrets.ProviderAnthropic, nil)
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundlePinned(t, p, rs, p.sealer, "run-rpk1", "team1", "owner1", []string{"anthropic"})
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("anthropic key = %q, want OUT — refused_pinned_key=forfait leaves the refused key to the forfait", got)
	}
	if got := b.PinnedAPIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("pinned anthropic key = %q, want OUT", got)
	}
	if len(b.OAuthCredentials) == 0 {
		t.Fatal("the forfait must still hold the family — the routes naming anthropic spend it")
	}
}

// `park` (settable and lockable) is the pre-policy behavior: the refused
// key comes back pinned-only and the routes naming its provider park on
// the key's own refusal with a durable retry.
func TestRefusedPinnedKey_ParkRestoresPinnedOnly(t *testing.T) {
	p := refusedPinnedFixture(t, secrets.ProviderAnthropic, parkRecord())
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundlePinned(t, p, rs, p.sealer, "run-rpk2", "team1", "owner1", []string{"anthropic"})
	if got := b.PinnedAPIKeys[secrets.ProviderAnthropic]; got != "sk-anthropic-platform" {
		t.Fatalf("pinned anthropic key = %q, want the refused key RESTORED pinned-only under park", got)
	}
	if !b.PlatformSourced[string(secrets.ProviderAnthropic)] {
		t.Fatal("a restored PLATFORM key must keep its platform-sourced metering scope")
	}
}

// The env dial is the record's deployment default: park without any
// record at all.
func TestRefusedPinnedKey_EnvDialPark(t *testing.T) {
	t.Setenv(llmroute.EnvRefusedPinnedKey, llmroute.RefusedPinnedPark)
	p := refusedPinnedFixture(t, secrets.ProviderAnthropic, nil)
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundlePinned(t, p, rs, p.sealer, "run-rpk3", "team1", "owner1", []string{"anthropic"})
	if got := b.PinnedAPIKeys[secrets.ProviderAnthropic]; got != "sk-anthropic-platform" {
		t.Fatalf("pinned anthropic key = %q, want restored under the env dial", got)
	}
}

// The carve-out travels with the knob (ADR-121 § Arbitrated 1): a FACADE
// key (z.ai) has no forfait alternative, so it is out of the knob's scope
// entirely — it comes back pinned-only even under the default forfait.
func TestRefusedPinnedKey_FacadeCarveOutAlwaysRestores(t *testing.T) {
	p := refusedPinnedFixture(t, secrets.ProviderZAI, nil)
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundlePinned(t, p, rs, p.sealer, "run-rpk4", "team1", "owner1", []string{"zai"})
	if got := b.PinnedAPIKeys[secrets.ProviderZAI]; got != "sk-zai-platform" {
		t.Fatalf("pinned zai key = %q, want RESTORED — the carve-out keeps facades out of the knob's scope", got)
	}
}

// The parity property the ticket demands: the preview predicts the same
// answer the launch seals, in BOTH directions of the knob.
func TestCredentialPreviewMatchesSealedBundleUnderRefusedPinnedKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *platformcfg.PlatformCredentials
	}{
		{"forfait default", nil},
		{"park", parkRecord()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := refusedPinnedFixture(t, secrets.ProviderAnthropic, tc.rec)
			p.identity = &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg}}}
			spec := previewSpec(poolTeam, "webhook:private")
			wf := wfPinning("anthropic")

			preview, err := previewReadOnly(p).PreviewCredentials(t.Context(), spec, wf)
			if err != nil {
				t.Fatal(err)
			}
			pinned := derivePinnedProviders(wf, model.ModelOverrides{}, nil)
			ctx := store.WithTenant(t.Context(), poolTeam)
			res, err := p.resolveAndSealCredentials(ctx, "oracle-run", poolOrg, poolTeam, spec.OwnerID, spec.Context.BotID, "", wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, pinned, nil)
			if err != nil {
				t.Fatal(err)
			}
			record, err := p.runSecrets.(*secrets.MemoryRunSecretsStore).Get(ctx, res.secretsRef)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, res.dek), record.TenantID, "", "oracle-run", record.KeyID, record.SealedBundle)
			if err != nil {
				t.Fatal(err)
			}
			live := bundleDescriptions(bundle)
			for provider := range bundle.PinnedAPIKeys {
				live = append(live, "platform:pinned_key:"+string(provider))
			}
			sort.Strings(live)
			var shown []string
			for _, c := range preview.Candidates {
				if c.Selected {
					d := c.Tier + ":" + c.Source + ":" + c.Provider
					if c.Source == "api_key" && c.Provider != "" && bundle.APIKeys[secrets.Provider(c.Provider)] == "" {
						d = c.Tier + ":pinned_key:" + c.Provider
					}
					shown = append(shown, d)
				}
			}
			sort.Strings(shown)
			if !reflect.DeepEqual(shown, live) {
				t.Fatalf("preview=%v live=%v", shown, live)
			}
			// The state line must say what happened to the refused key,
			// whichever way the knob answered.
			for _, c := range preview.Candidates {
				if c.Provider != "anthropic" || c.Source != "api_key" {
					continue
				}
				switch tc.name {
				case "forfait default":
					if c.Selected {
						t.Fatalf("the refused key's candidate is selected in the preview under forfait: %+v", c)
					}
				case "park":
					if !c.Selected {
						t.Fatalf("the refused key's candidate is not selected in the preview under park: %+v (%s)", c, c.Reason)
					}
				}
			}
		})
	}
}
