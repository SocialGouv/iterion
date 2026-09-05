package runner

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// scopeSpyKeyStore records, per fingerprint, the tenant the runner put on
// the bump's context — "" for a cross-tenant bump.
type scopeSpyKeyStore struct {
	secrets.ApiKeyStore
	scopes map[string]string
}

func (s *scopeSpyKeyStore) MarkFingerprintUsed(ctx context.Context, fingerprint string, at time.Time) error {
	tenant, _ := store.TenantFromContext(ctx)
	s.scopes[fingerprint] = tenant
	return nil
}

// #659 — the metering bump's scope follows the key's tier. A tenant's own
// key is bumped under the run's tenant, so another tenant holding the
// byte-identical secret never reads its own key as "in use"; a platform-
// tier or pool-lent key is bumped across tenants, because its row lives
// elsewhere (the platform sentinel, the donor's tenant) and it serves every
// tenant. A fingerprint any slot holds from another tier goes cross-tenant.
func TestMarkCredFingerprintsUsed_ScopesByTier(t *testing.T) {
	spy := &scopeSpyKeyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore(), scopes: map[string]string{}}
	r := &Runner{cfg: Config{ApiKeys: spy, Logger: iterlog.Nop()}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Fingerprints: map[string]string{
			string(secrets.ProviderAnthropic):  "fp-tenant-own",
			string(secrets.ProviderZAI):        "fp-platform",
			string(secrets.ProviderOpenAI):     "fp-pool",
			string(secrets.ProviderXAI):        "fp-shared",
			string(secrets.ProviderOpenRouter): "fp-shared", // same secret: one slot tenant-own, one platform
		},
		PlatformSourced: map[string]bool{string(secrets.ProviderZAI): true, string(secrets.ProviderOpenRouter): true},
		PoolSourced:     map[string]bool{string(secrets.ProviderOpenAI): true},
	})
	r.markCredFingerprintsUsed(ctx, &queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, time.Now().UTC())

	want := map[string]string{
		"fp-tenant-own": "team-a",
		"fp-platform":   "",
		"fp-pool":       "",
		"fp-shared":     "",
	}
	if len(spy.scopes) != len(want) {
		t.Fatalf("bumped %v, want exactly %v", spy.scopes, want)
	}
	for fp, tenant := range want {
		if got, ok := spy.scopes[fp]; !ok || got != tenant {
			t.Errorf("%s bumped under tenant %q (present=%v), want %q", fp, got, ok, tenant)
		}
	}

	// A message with no tenant (legacy) bumps cross-tenant, as before.
	spy.scopes = map[string]string{}
	r.markCredFingerprintsUsed(ctx, &queue.RunMessage{RunID: "run-2"}, time.Now().UTC())
	if got := spy.scopes["fp-tenant-own"]; got != "" {
		t.Fatalf("a tenant-less message scoped the bump to %q", got)
	}
}
