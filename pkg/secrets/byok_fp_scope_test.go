package secrets

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// #659 — MarkFingerprintUsed's scope comes from the context. Two tenants
// holding the byte-identical secret share a fingerprint; a bump scoped to
// one tenant must move ONLY that tenant's row (the studio reads
// last_used_at as "is this key still in use?" before a rotate or delete),
// while an unscoped bump — a platform-tier or pool-lent key, whose row lives
// in another tenant — moves every row carrying the fingerprint.
func TestMemoryApiKey_MarkFingerprintUsed_ScopesToTheContextTenant(t *testing.T) {
	st := NewMemoryApiKeyStore()
	create := func(id, tenant string) {
		t.Helper()
		if err := st.Create(store.WithTenant(context.Background(), tenant), ApiKey{
			ID: id, ScopeTeamID: tenant, ScopeUserID: "u-" + tenant, Provider: ProviderAnthropic,
			Name: id, Fingerprint: "fp-shared",
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	create("k-a", "team-a")
	create("k-b", "team-b")
	// Create stamps the tenant from the context, as the Mongo twin does.
	if got, _ := st.Get(context.Background(), "k-a"); got.TenantID != "team-a" {
		t.Fatalf("Create did not stamp TenantID from the context: %q", got.TenantID)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := st.MarkFingerprintUsed(store.WithTenant(context.Background(), "team-a"), "fp-shared", now); err != nil {
		t.Fatalf("scoped bump: %v", err)
	}
	a, _ := st.Get(context.Background(), "k-a")
	b, _ := st.Get(context.Background(), "k-b")
	if a.LastUsedAt == nil || !a.LastUsedAt.Equal(now) {
		t.Fatalf("team-a's row not bumped: %v", a.LastUsedAt)
	}
	if b.LastUsedAt != nil {
		t.Fatalf("team-b's row bumped by team-a's run (%v) — its key now reads as in use", b.LastUsedAt)
	}

	// Unscoped: both move (the pool/platform shape).
	later := now.Add(time.Minute)
	if err := st.MarkFingerprintUsed(context.Background(), "fp-shared", later); err != nil {
		t.Fatalf("unscoped bump: %v", err)
	}
	a, _ = st.Get(context.Background(), "k-a")
	b, _ = st.Get(context.Background(), "k-b")
	if a.LastUsedAt == nil || !a.LastUsedAt.Equal(later) || b.LastUsedAt == nil || !b.LastUsedAt.Equal(later) {
		t.Fatalf("unscoped bump must move every row: a=%v b=%v", a.LastUsedAt, b.LastUsedAt)
	}
}
