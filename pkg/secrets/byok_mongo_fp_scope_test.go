package secrets

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The Mongo twin of TestMemoryApiKey_MarkFingerprintUsed_ScopesToTheContextTenant:
// a tenant on the context narrows the UpdateMany to that tenant's rows; no
// tenant keeps the cross-tenant bump. Skipped without ITERION_TEST_MONGO_URI
// (the mongo-conformance CI job gates it).
func TestMongoApiKey_MarkFingerprintUsed_ScopesToTheContextTenant(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	for _, tenant := range []string{"team-a", "team-b"} {
		if err := s.Create(store.WithTenant(ctx, tenant), ApiKey{
			ID: "k-" + tenant, ScopeTeamID: tenant, ScopeUserID: "u-" + tenant,
			Provider: ProviderAnthropic, Name: "k-" + tenant, Fingerprint: "fp-shared",
		}); err != nil {
			t.Fatalf("create %s: %v", tenant, err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkFingerprintUsed(store.WithTenant(ctx, "team-a"), "fp-shared", now); err != nil {
		t.Fatalf("scoped bump: %v", err)
	}
	a, err := s.Get(store.WithTenant(ctx, "team-a"), "k-team-a")
	if err != nil {
		t.Fatalf("read k-team-a: %v", err)
	}
	if a.LastUsedAt == nil || !a.LastUsedAt.Equal(now) {
		t.Fatalf("team-a's row not bumped: %v", a.LastUsedAt)
	}
	b, err := s.Get(store.WithTenant(ctx, "team-b"), "k-team-b")
	if err != nil {
		t.Fatalf("read k-team-b: %v", err)
	}
	if b.LastUsedAt != nil {
		t.Fatalf("team-b's row bumped by a team-a-scoped run: %v", b.LastUsedAt)
	}

	later := now.Add(time.Minute)
	if err := s.MarkFingerprintUsed(ctx, "fp-shared", later); err != nil {
		t.Fatalf("unscoped bump: %v", err)
	}
	b, err = s.Get(store.WithTenant(ctx, "team-b"), "k-team-b")
	if err != nil {
		t.Fatalf("read k-team-b: %v", err)
	}
	if b.LastUsedAt == nil || !b.LastUsedAt.Equal(later) {
		t.Fatalf("unscoped bump must move team-b's row too: %v", b.LastUsedAt)
	}
}
