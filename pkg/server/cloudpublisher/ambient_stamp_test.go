package cloudpublisher

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The ambient stamp on trust (ADR-162 decision 7): every UNTRUSTED run's
// sealed bundle carries AmbientLLMRefused — the stamp ambientStampRefusal has
// honoured all along, under which the run's LLM routes are funded by the
// bundle's credentials only and #1768's ambient channel stops counting. A
// trusted run's bundle keeps whatever the credential tiers decided: the
// publisher adds the untrusted case, it never clears a stamp. Mutant — the
// stamp dropped or keyed on something else: the fork bundle reads clean, or
// the trusted one is stamped.
func TestEveryUntrustedRunCarriesTheAmbientStamp(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := store.WithTenant(context.Background(), "team1")
	keys := secrets.NewMemoryApiKeyStore()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte("zai-run-stamp-witness"))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := keys.Create(ctx, secrets.ApiKey{
		ID: id, ScopeTeamID: "team1", Provider: secrets.ProviderZAI, Name: "zai-stamp",
		SealedSecret: sealed, Fingerprint: "fp-zai-stamp", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	runSecrets := secrets.NewMemoryRunSecretsStore()
	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, runSecrets: runSecrets, sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}

	for _, row := range []struct {
		name  string
		trust store.RunTrust
		stamp bool
	}{
		{"a fork-lane run", store.RunTrustFork, true},
		{"a trusted run (the control)", store.RunTrustDefault, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			runID := "run-stamp-" + row.name
			creds, err := p.resolveAndSealCredentials(ctx, runID, "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, row.trust, nil, nil, nil)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			if creds.secretsRef == "" {
				t.Fatal("the run sealed no bundle — this row proves nothing without a credential to carry")
			}
			rec, err := runSecrets.Get(ctx, creds.secretsRef)
			if err != nil {
				t.Fatalf("RunSecrets.Get: %v", err)
			}
			bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", runID, rec.KeyID, rec.SealedBundle)
			if err != nil {
				t.Fatalf("OpenRunBundle: %v", err)
			}
			if bundle.AmbientLLMRefused != row.stamp {
				t.Errorf("bundle.AmbientLLMRefused = %v, want %v — %s", bundle.AmbientLLMRefused, row.stamp,
					"the stamp is what makes the fork lane's arms refuse the pod's money")
			}
		})
	}
}
