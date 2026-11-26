package secrets

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// gatewayLookupStore is a key store that hosts the gateway lookup.
type gatewayLookupStore interface {
	ApiKeyStore
	GatewayKeyLookup
}

// exerciseGatewayKeyLookup is shared by both twins: a team's gateway row and a
// platform gateway row are found under the context of a third tenant, a vendor
// row whose fingerprint is asked for is not, and the empty fingerprint matches
// nothing — not even a row stored before fingerprint stamping.
func exerciseGatewayKeyLookup(t *testing.T, s gatewayLookupStore, ctx context.Context) {
	t.Helper()
	create := func(tenant string, k ApiKey) {
		t.Helper()
		k.TenantID, k.ScopeTeamID = tenant, tenant
		if err := s.Create(store.WithTenant(ctx, tenant), k); err != nil {
			t.Fatalf("create %s: %v", k.ID, err)
		}
	}
	create("team-a", ApiKey{ID: "k-team-gw", Provider: ProviderOpenAICompatible, Name: "team gw", BaseURL: "https://gw-a.example", Fingerprint: "fp-team-gw"})
	create(PlatformTenantID, ApiKey{ID: "k-platform-gw", Provider: ProviderOpenAICompatible, Name: "platform gw", BaseURL: "https://gw-p.example", Fingerprint: "fp-platform-gw"})
	create("team-a", ApiKey{ID: "k-vendor", Provider: ProviderAnthropic, Name: "vendor", Fingerprint: "fp-vendor"})
	create("team-a", ApiKey{ID: "k-unstamped-gw", Provider: ProviderOpenAICompatible, Name: "legacy gw", BaseURL: "https://gw-legacy.example"})

	got, err := s.GatewayBaseURLsByFingerprint(store.WithTenant(ctx, "team-reader"), []string{"fp-team-gw", "fp-platform-gw", "fp-vendor", "fp-unknown", ""})
	if err != nil {
		t.Fatalf("GatewayBaseURLsByFingerprint: %v", err)
	}
	want := map[string]string{"fp-team-gw": "https://gw-a.example", "fp-platform-gw": "https://gw-p.example"}
	if !maps.Equal(got, want) {
		t.Fatalf("lookup under a third tenant's context = %v, want %v — every tenant's gateway rows, no vendor row, no unstamped row", got, want)
	}
}

// exerciseGatewayKeyLookupRefusesTwoURLs is shared by both twins: two rows
// carrying one fingerprint with different URLs cannot both be the gateway that
// fingerprint dials, so the lookup refuses rather than picking one. The Mongo
// fingerprint index is sparse and not unique, so the store can hold the pair:
// a corrupt row, or two legacy rows whose fingerprint does not fold the URL in.
// Two rows on one URL are one gateway, and answer it.
func exerciseGatewayKeyLookupRefusesTwoURLs(t *testing.T, s gatewayLookupStore, ctx context.Context) {
	t.Helper()
	tctx := store.WithTenant(ctx, "team-a")
	for _, k := range []ApiKey{
		{ID: "k-1", Provider: ProviderOpenAICompatible, Name: "one", BaseURL: "https://one.example", Fingerprint: "fp-shared"},
		{ID: "k-2", Provider: ProviderOpenAICompatible, Name: "two", BaseURL: "https://two.example", Fingerprint: "fp-shared"},
		{ID: "k-3", Provider: ProviderOpenAICompatible, Name: "same-a", BaseURL: "https://same.example", Fingerprint: "fp-same"},
		{ID: "k-4", Provider: ProviderOpenAICompatible, Name: "same-b", BaseURL: "https://same.example", Fingerprint: "fp-same"},
	} {
		k.TenantID, k.ScopeTeamID = "team-a", "team-a"
		if err := s.Create(tctx, k); err != nil {
			t.Fatalf("create %s: %v", k.ID, err)
		}
	}
	got, err := s.GatewayBaseURLsByFingerprint(tctx, []string{"fp-same"})
	if err != nil || !maps.Equal(got, map[string]string{"fp-same": "https://same.example"}) {
		t.Fatalf("lookup of two rows on one URL = %v (%v), want that URL", got, err)
	}
	if got, err := s.GatewayBaseURLsByFingerprint(tctx, []string{"fp-shared", "fp-same"}); err == nil || !strings.Contains(err.Error(), "fp-shared") {
		t.Fatalf("lookup = %v (%v), want an error naming the fingerprint on two URLs", got, err)
	}
}

func TestGatewayKeyLookup_Memory(t *testing.T) {
	exerciseGatewayKeyLookup(t, NewMemoryApiKeyStore(), context.Background())
}

func TestGatewayKeyLookup_MemoryRefusesAFingerprintOnTwoURLs(t *testing.T) {
	exerciseGatewayKeyLookupRefusesTwoURLs(t, NewMemoryApiKeyStore(), context.Background())
}

func TestGatewayKeyLookup_Mongo(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	exerciseGatewayKeyLookup(t, s, ctx)
}

func TestGatewayKeyLookup_MongoRefusesAFingerprintOnTwoURLs(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	exerciseGatewayKeyLookupRefusesTwoURLs(t, s, ctx)
}
