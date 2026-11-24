package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runDeleteByScopeUserSuite pins what an account's erasure asks of the two
// credential stores: every row scoped to the person goes, in every tenant,
// from a context naming none; the team-wide rows and another person's stay;
// an empty user — the team-wide scope — is refused.
func runDeleteByScopeUserSuite(t *testing.T, ctx context.Context, keys ApiKeyStore, gens GenericSecretStore) {
	t.Helper()
	teamA, teamB := store.WithTenant(ctx, "team-a"), store.WithTenant(ctx, "team-b")
	for _, c := range []struct {
		ctx context.Context
		key ApiKey
	}{
		{teamA, ApiKey{ID: "k-own-a", ScopeTeamID: "team-a", ScopeUserID: "gone", Provider: ProviderAnthropic, Name: "own a"}},
		{teamB, ApiKey{ID: "k-own-b", ScopeTeamID: "team-b", ScopeUserID: "gone", Provider: ProviderAnthropic, Name: "own b"}},
		{teamA, ApiKey{ID: "k-team", ScopeTeamID: "team-a", Provider: ProviderAnthropic, Name: "team-wide"}},
		{teamA, ApiKey{ID: "k-other", ScopeTeamID: "team-a", ScopeUserID: "stays", Provider: ProviderAnthropic, Name: "other"}},
	} {
		if err := keys.Create(c.ctx, c.key); err != nil {
			t.Fatalf("create key %s: %v", c.key.ID, err)
		}
	}
	for _, c := range []struct {
		ctx context.Context
		sec GenericSecret
	}{
		{teamA, GenericSecret{ID: "g-own-a", ScopeTeamID: "team-a", ScopeUserID: "gone", Name: "OWN_A"}},
		{teamB, GenericSecret{ID: "g-own-b", ScopeTeamID: "team-b", ScopeUserID: "gone", Name: "OWN_B"}},
		{teamA, GenericSecret{ID: "g-team", ScopeTeamID: "team-a", Name: "TEAM"}},
		{teamA, GenericSecret{ID: "g-other", ScopeTeamID: "team-a", ScopeUserID: "stays", Name: "OTHER"}},
	} {
		if err := gens.Create(c.ctx, c.sec); err != nil {
			t.Fatalf("create secret %s: %v", c.sec.ID, err)
		}
	}

	if _, err := keys.DeleteByScopeUser(ctx, ""); err == nil {
		t.Fatal("a key deletion naming no user answered no error")
	}
	if _, err := gens.DeleteByScopeUser(ctx, ""); err == nil {
		t.Fatal("a secret deletion naming no user answered no error")
	}
	if n, err := keys.DeleteByScopeUser(ctx, "gone"); err != nil || n != 2 {
		t.Fatalf("keys DeleteByScopeUser = %d (%v), want 2: one per tenant", n, err)
	}
	if n, err := gens.DeleteByScopeUser(ctx, "gone"); err != nil || n != 2 {
		t.Fatalf("secrets DeleteByScopeUser = %d (%v), want 2: one per tenant", n, err)
	}
	for id, c := range map[string]context.Context{"k-own-a": teamA, "k-own-b": teamB} {
		if _, err := keys.Get(c, id); !errors.Is(err, ErrApiKeyNotFound) {
			t.Fatalf("key %s survived: %v", id, err)
		}
	}
	for _, id := range []string{"k-team", "k-other"} {
		if _, err := keys.Get(teamA, id); err != nil {
			t.Fatalf("key %s went too: %v", id, err)
		}
	}
	for id, c := range map[string]context.Context{"g-own-a": teamA, "g-own-b": teamB} {
		if _, err := gens.Get(c, id); !errors.Is(err, ErrGenericSecretNotFound) {
			t.Fatalf("secret %s survived: %v", id, err)
		}
	}
	for _, id := range []string{"g-team", "g-other"} {
		if _, err := gens.Get(teamA, id); err != nil {
			t.Fatalf("secret %s went too: %v", id, err)
		}
	}
	if n, err := keys.DeleteByScopeUser(ctx, "gone"); err != nil || n != 0 {
		t.Fatalf("a second key deletion = %d (%v), want 0", n, err)
	}
}

func TestMemoryStores_DeleteByScopeUser(t *testing.T) {
	runDeleteByScopeUserSuite(t, context.Background(), NewMemoryApiKeyStore(), NewMemoryGenericSecretStore())
}

// TestMongoStores_DeleteByScopeUser is the Mongo leg, gated on
// ITERION_TEST_MONGO_URI like the sibling suites.
func TestMongoStores_DeleteByScopeUser(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo scope-user deletion suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_scope_user_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	runDeleteByScopeUserSuite(t, ctx, NewMongoApiKeyStore(db), NewMongoGenericSecretStore(db))
}
