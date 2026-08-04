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
