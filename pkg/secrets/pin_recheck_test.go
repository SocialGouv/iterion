package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// #2644. RecheckPinnedRecords is the one judgment the pin-landing writers
// share once their row is stored (pkg/server reconcileLandedPins, pkg/forge
// reconcileStoredPins): does each record a pin names still live. These
// witnesses read the verdicts against the real memory stores — the
// not-found sentinel they answer is the one the Mongo stores answer — with a
// wrapper only where a read must fail the way an outage does.

// countingKeys serves Get from the real store, counts every read, and fails
// the ids it marks broken with a store error that is not the not-found
// sentinel.
type countingKeys struct {
	ApiKeyStore
	broken map[string]bool
	reads  map[string]int
}

func (c *countingKeys) Get(ctx context.Context, id string) (ApiKey, error) {
	c.reads[id]++
	if c.broken[id] {
		return ApiKey{}, errors.New("key store unavailable")
	}
	return c.ApiKeyStore.Get(ctx, id)
}

// countingSecrets is countingKeys' twin for the stored secrets.
type countingSecrets struct {
	GenericSecretStore
	reads map[string]int
}

func (c *countingSecrets) Get(ctx context.Context, id string) (GenericSecret, error) {
	c.reads[id]++
	return c.GenericSecretStore.Get(ctx, id)
}

// pinRecheckStores seeds, under tenant t1, a live api key and a live stored
// secret, and under t2 a key a t1 pin may name but t1 cannot see.
func pinRecheckStores(t *testing.T) (*countingKeys, *countingSecrets) {
	t.Helper()
	ctx := context.Background()
	keys := NewMemoryApiKeyStore()
	for _, k := range []ApiKey{
		{ID: "key-live", TenantID: "t1", ScopeTeamID: "t1", Provider: ProviderAnthropic, Name: "live", Fingerprint: "fp-live", CreatedAt: time.Now().UTC()},
		{ID: "key-foreign", TenantID: "t2", ScopeTeamID: "t2", Provider: ProviderOpenAI, Name: "foreign", Fingerprint: "fp-foreign", CreatedAt: time.Now().UTC()},
	} {
		if err := keys.Create(store.WithTenant(ctx, k.TenantID), k); err != nil {
			t.Fatal(err)
		}
	}
	gen := NewMemoryGenericSecretStore()
	if err := gen.Create(store.WithTenant(ctx, "t1"), GenericSecret{
		ID: "sec-live", TenantID: "t1", ScopeTeamID: "t1", Name: "forge_token", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return &countingKeys{ApiKeyStore: keys, broken: map[string]bool{"key-broken": true}, reads: map[string]int{}},
		&countingSecrets{GenericSecretStore: gen, reads: map[string]int{}}
}

// The verdicts name each record once, in a stable order, and tell its three
// states apart: alive, revoked (the store's not-found under the tenant —
// a foreign tenant's record included, which the tenant's launch cannot
// spend either), and unjudged (a read the store could not complete, which
// is never a revocation: sweeping on it would clear a live pin over an
// outage).
func TestRecheckPinnedRecords_JudgesEachRecordOnceUnderTheTenant(t *testing.T) {
	keys, gen := pinRecheckStores(t)
	keyPins := map[string]string{
		"anthropic": "key-live", "mistral": "key-live",
		"openai": "key-dead", "gemini": "key-broken", "groq": "key-foreign",
	}
	secretPins := map[string]string{"forge_token": "sec-live", "deploy_token": "sec-dead", "mirror_token": "sec-live"}

	got := RecheckPinnedRecords(context.Background(), keys, gen, "t1", keyPins, secretPins)

	want := []struct {
		id, kind string
		revoked  bool
		failed   bool
	}{
		{"key-broken", "api key", false, true},
		{"key-dead", "api key", true, false},
		{"key-foreign", "api key", true, false},
		{"key-live", "api key", false, false},
		{"sec-dead", "stored secret", true, false},
		{"sec-live", "stored secret", false, false},
	}
	if len(got) != len(want) {
		t.Fatalf("verdicts = %+v, want %d — one per record, however many pins name it", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.id || g.Kind != w.kind || g.Revoked != w.revoked || (g.Err != nil) != w.failed {
			t.Errorf("verdict %d = {ID:%s Kind:%s Revoked:%v Err:%v}, want {ID:%s Kind:%s Revoked:%v failed:%v}",
				i, g.ID, g.Kind, g.Revoked, g.Err, w.id, w.kind, w.revoked, w.failed)
		}
	}
	if g := got[0]; g.Err == nil || !strings.Contains(g.Err.Error(), "key store unavailable") {
		t.Errorf("the broken read's verdict carries %v, want the store's own failure", g.Err)
	}
	for id, n := range keys.reads {
		if n != 1 {
			t.Errorf("api key %s read %d times, want once", id, n)
		}
	}
	for id, n := range gen.reads {
		if n != 1 {
			t.Errorf("stored secret %s read %d times, want once", id, n)
		}
	}
}

// A store that is not wired leaves its family unjudged — no verdict, no
// read — and judges the other family all the same.
func TestRecheckPinnedRecords_AStoreNotWiredJudgesNothing(t *testing.T) {
	keys, gen := pinRecheckStores(t)
	keyPins := map[string]string{"anthropic": "key-dead"}
	secretPins := map[string]string{"forge_token": "sec-dead"}

	got := RecheckPinnedRecords(context.Background(), nil, gen, "t1", keyPins, secretPins)
	if len(got) != 1 || got[0].ID != "sec-dead" || !got[0].Revoked {
		t.Fatalf("verdicts without a key store = %+v, want the secret's revocation alone", got)
	}
	got = RecheckPinnedRecords(context.Background(), keys, nil, "t1", keyPins, secretPins)
	if len(got) != 1 || got[0].ID != "key-dead" || !got[0].Revoked {
		t.Fatalf("verdicts without a secret store = %+v, want the key's revocation alone", got)
	}
	if len(keys.reads) != 1 || len(gen.reads) != 1 {
		t.Fatalf("reads = keys %v secrets %v, want one each — an unwired store is never asked", keys.reads, gen.reads)
	}
}
