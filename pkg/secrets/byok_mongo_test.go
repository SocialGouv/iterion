package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The tenant filter on the key store is invisible to MemoryApiKeyStore,
// which keys on id alone. That divergence is precisely what let a
// cross-tenant read ship: every in-memory test of the credential pool
// passed while the Mongo store found nothing. These run against a real
// Mongo (same gating as the other conformance suites — CI's
// mongo-conformance job sets ITERION_TEST_MONGO_URI).
func mongoKeyStore(t *testing.T) (*MongoApiKeyStore, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo api-key suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_byok_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	return NewMongoApiKeyStore(db), ctx
}

// A pooled key is read from the BORROWER's context — another tenant than
// the donor's. Get cannot serve that read, and a caller using it would
// mistake "wrong tenant" for "key deleted" and park an innocent donor's
// contribution. GetOwned is the deliberate cross-tenant door, bounded by
// ownership instead.
func TestMongoApiKey_GetOwnedCrossesTenantsWhereGetCannot(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	donorCtx := store.WithTenant(ctx, "team-donor")
	if err := s.Create(donorCtx, ApiKey{
		ID: "key-1", TenantID: "team-donor", ScopeTeamID: "team-donor",
		ScopeUserID: "alice", Provider: ProviderAnthropic, Name: "lent",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	borrowerCtx := store.WithTenant(ctx, "team-borrower")

	// The tenant-scoped read is blind to it — the defect's mechanism.
	if _, err := s.Get(borrowerCtx, "key-1"); !errors.Is(err, ErrApiKeyNotFound) {
		t.Errorf("Get from another tenant = %v, want ErrApiKeyNotFound", err)
	}

	// The owner-scoped read serves it, which is what makes lending work.
	got, err := s.GetOwned(borrowerCtx, "key-1", "alice")
	if err != nil {
		t.Fatalf("GetOwned from the borrower's tenant: %v", err)
	}
	if got.ID != "key-1" || got.ScopeUserID != "alice" {
		t.Errorf("got %+v, want alice's key-1", got)
	}
}

// Ownership is the whole boundary GetOwned offers: it must refuse a key
// that is not the named user's, and a team-wide key (no owner) outright —
// a team key is the team's to spend, never one member's to lend.
func TestMongoApiKey_GetOwnedRefusesWhatIsNotYours(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	tctx := store.WithTenant(ctx, "team-1")
	if err := s.Create(tctx, ApiKey{
		ID: "key-personal", TenantID: "team-1", ScopeTeamID: "team-1",
		ScopeUserID: "alice", Provider: ProviderAnthropic, Name: "alice's",
	}); err != nil {
		t.Fatalf("create personal: %v", err)
	}
	if err := s.Create(tctx, ApiKey{
		ID: "key-team", TenantID: "team-1", ScopeTeamID: "team-1",
		Provider: ProviderAnthropic, Name: "the team's",
	}); err != nil {
		t.Fatalf("create team: %v", err)
	}

	for _, tc := range []struct{ name, id, owner string }{
		{"another user's key", "key-personal", "mallory"},
		{"a team-wide key has no owner to lend it", "key-team", "alice"},
		{"no owner named at all", "key-personal", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.GetOwned(tctx, tc.id, tc.owner); !errors.Is(err, ErrApiKeyNotFound) {
				t.Errorf("GetOwned = %v, want ErrApiKeyNotFound", err)
			}
		})
	}
}

// The store stamps tenant_id from the CONTEXT and filters reads by it, so a
// key created under one tenant's context but scoped to another team is
// unreachable by the team it was meant to fund — a run there resolves nothing
// and falls back to the platform credential, silently. This pins the contract
// the api-keys HTTP layer must honour: create under the context of the team
// the key is FOR.
func TestMongoApiKey_ScopeWithoutMatchingTenantIsUnreachable(t *testing.T) {
	s, ctx := mongoKeyStore(t)

	// The defect shape: written under team-a's context, scoped to team-b.
	if err := s.Create(store.WithTenant(ctx, "team-a"), ApiKey{
		ID: "mis-stamped", ScopeTeamID: "team-b",
		Provider: ProviderAnthropic, Name: "meant for team-b", IsDefault: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.ListByTeam(store.WithTenant(ctx, "team-b"), "team-b", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a key stamped with another tenant must NOT be reachable as team-b's, got %d", len(got))
	}

	// The correct shape: written under the context of the team it is for.
	if err := s.Create(store.WithTenant(ctx, "team-b"), ApiKey{
		ID: "well-stamped", ScopeTeamID: "team-b",
		Provider: ProviderAnthropic, Name: "for team-b", IsDefault: true,
	}); err != nil {
		t.Fatalf("create scoped: %v", err)
	}
	got, err = s.ListByTeam(store.WithTenant(ctx, "team-b"), "team-b", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "well-stamped" {
		t.Fatalf("team-b must see exactly its own key, got %+v", got)
	}
	// From team-a's context the WELL-stamped key is out of reach — while the
	// mis-stamped one still answers, which is the whole asymmetry: the row is
	// visible to whoever created it and not to whoever it belongs to.
	other, err := s.ListByTeam(store.WithTenant(ctx, "team-a"), "team-b", "")
	if err != nil {
		t.Fatalf("list from team-a: %v", err)
	}
	for _, k := range other {
		if k.ID == "well-stamped" {
			t.Fatalf("team-b's key must not be reachable from team-a's context")
		}
	}
	if len(other) != 1 || other[0].ID != "mis-stamped" {
		t.Fatalf("the mis-stamped row is the one that answers here, got %+v", other)
	}
}

// The Mongo twin of TestMemoryApiKey_MarkFingerprintUsed: the metering
// bump must land in prod on the real store, not only in unit-test
// memory. Skipped without ITERION_TEST_MONGO_URI (mongo-conformance CI
// job gates it, like the other Mongo suites in this file).
func TestMongoApiKey_MarkFingerprintUsed(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	// Two rows sharing a fingerprint (an operator saved the same secret
	// twice on different tenants) — the update must land on BOTH, so a
	// run spending either row moves the meter on both.
	if err := s.Create(store.WithTenant(ctx, "team-a"), ApiKey{
		ID: "k1", TenantID: "team-a", ScopeTeamID: "team-a",
		ScopeUserID: "u1", Provider: ProviderAnthropic, Name: "k1", Fingerprint: "fp-shared",
	}); err != nil {
		t.Fatalf("create k1: %v", err)
	}
	if err := s.Create(store.WithTenant(ctx, "team-b"), ApiKey{
		ID: "k2", TenantID: "team-b", ScopeTeamID: "team-b",
		ScopeUserID: "u2", Provider: ProviderAnthropic, Name: "k2", Fingerprint: "fp-shared",
	}); err != nil {
		t.Fatalf("create k2: %v", err)
	}
	if err := s.Create(store.WithTenant(ctx, "team-c"), ApiKey{
		ID: "k3", TenantID: "team-c", ScopeTeamID: "team-c",
		ScopeUserID: "u3", Provider: ProviderAnthropic, Name: "k3", Fingerprint: "fp-untouched",
	}); err != nil {
		t.Fatalf("create k3: %v", err)
	}

	if err := s.MarkFingerprintUsed(ctx, "", now); err != nil {
		t.Errorf("empty fp errored: %v", err)
	}
	if err := s.MarkFingerprintUsed(ctx, "fp-shared", now); err != nil {
		t.Fatalf("MarkFingerprintUsed: %v", err)
	}
	k1, err := s.Get(store.WithTenant(ctx, "team-a"), "k1")
	if err != nil {
		t.Fatalf("read k1: %v", err)
	}
	if k1.LastUsedAt == nil || !k1.LastUsedAt.Equal(now) {
		t.Errorf("k1.last_used_at = %v, want %v", k1.LastUsedAt, now)
	}
	k2, err := s.Get(store.WithTenant(ctx, "team-b"), "k2")
	if err != nil {
		t.Fatalf("read k2: %v", err)
	}
	if k2.LastUsedAt == nil || !k2.LastUsedAt.Equal(now) {
		t.Errorf("k2.last_used_at = %v, want %v (a shared fingerprint must land on every matching row)", k2.LastUsedAt, now)
	}
	k3, err := s.Get(store.WithTenant(ctx, "team-c"), "k3")
	if err != nil {
		t.Fatalf("read k3: %v", err)
	}
	if k3.LastUsedAt != nil {
		t.Errorf("k3.last_used_at = %v, want nil (its fingerprint was untouched)", k3.LastUsedAt)
	}
}
