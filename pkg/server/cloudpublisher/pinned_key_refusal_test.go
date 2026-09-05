package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// A webhook `key_overrides` pin bypasses the evidence predicate BY DESIGN —
// the operator named that key, and honouring the pin over the optimisation
// is what keeps the predicate an optimisation. But when the pinned key
// carries fresh refusals, the only trace was the ABSENCE of a skip log:
// every run of that webhook fed the same wall, and nothing said so (#629
// pt 4). The pin still wins; it is no longer silent.
func TestResolve_pinnedKeyWithFreshRefusalsWarns(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-pinned", "fp-pinned")
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-healthy", "fp-healthy")
	pinned := firstKeyWithFingerprint(t, keys, "fp-pinned")

	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-pinned")

	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-pin", "", "team1", "owner1", "",
		nil, map[string]string{string(secrets.ProviderAnthropic): pinned.ID}, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	rec, err := rs.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "run-pin", rec.SealedBundle)
	if err != nil {
		t.Fatal(err)
	}
	// The pin is still honoured — that is the contract this warns about,
	// not one it changes.
	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "sk-pinned" {
		t.Fatalf("anthropic key = %q, want the PINNED key: a warning must not silently re-route the operator's choice", got)
	}
	log := buf.String()
	if !strings.Contains(log, pinned.ID) {
		t.Fatalf("log does not name the pinned key %q:\n%s", pinned.ID, log)
	}
	if !strings.Contains(log, "fp-pinned") {
		t.Fatalf("log does not name the refused credential:\n%s", log)
	}
	if !strings.Contains(strings.ToLower(log), "fair-usage") {
		t.Fatalf("log does not say WHY the provider is refusing:\n%s", log)
	}
}

// A pinned key with nothing against it must stay quiet: a warning that
// fires on every launch is one nobody reads.
func TestResolve_pinnedHealthyKeyIsQuiet(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-pinned", "fp-pinned")
	pinned := firstKeyWithFingerprint(t, keys, "fp-pinned")

	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, usageCaps: usagecap.NewMemStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}

	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-pin2", "", "team1", "owner1", "",
		nil, map[string]string{string(secrets.ProviderAnthropic): pinned.ID}, nil, model.ModelOverrides{}, nil); err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if strings.Contains(buf.String(), "pinned api key") {
		t.Fatalf("a healthy pinned key must produce no pin warning:\n%s", buf.String())
	}
}

func firstKeyWithFingerprint(t *testing.T, st secrets.ApiKeyStore, fp string) secrets.ApiKey {
	t.Helper()
	all, err := st.ListByTeam(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	for _, k := range all {
		if k.Fingerprint == fp {
			return k
		}
	}
	t.Fatalf("no key with fingerprint %s", fp)
	return secrets.ApiKey{}
}
