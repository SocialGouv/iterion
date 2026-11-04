package connection

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

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// newTestMongoForgeStore builds the Mongo twin over a fresh database, gated
// on ITERION_TEST_MONGO_URI like every mongo suite in this tree (the
// mongo-conformance CI job is what runs it).
func newTestMongoForgeStore(t *testing.T) ForgeStore {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_forge_conn_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoForgeStore(db)
	schemaCtx, schemaCancel := mongotest.Ctx(t)
	defer schemaCancel()
	if err := st.EnsureSchema(schemaCtx); err != nil {
		t.Fatalf("mongo schema: %v", err)
	}
	return st
}

// The #2760 contract, held against both twins: every single-record read is
// tenant-scoped IN THE SIGNATURE, so a caller holding another tenant's id
// reads not-found — never the record, never a distinguishable "forbidden".
func TestForgeStore_EveryReadIsTenantScoped(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		forgeStoreTenantContract(t, NewMemoryForgeStore())
	})
	t.Run("mongo", func(t *testing.T) {
		forgeStoreTenantContract(t, newTestMongoForgeStore(t))
	})
}

func forgeStoreTenantContract(t *testing.T, st ForgeStore) {
	t.Helper()
	ctx := context.Background()
	exp := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	if err := st.Create(ctx, ForgeConnection{ID: "c1", TenantID: "t1", Status: StatusActive, SealedPayload: []byte("v1"), AccessTokenExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}

	// A read naming another tenant is a miss, although the id exists.
	if _, err := st.Get(ctx, "t2", "c1"); !errors.Is(err, ErrForgeConnectionNotFound) {
		t.Fatalf("Get across tenants: err = %v, want ErrForgeConnectionNotFound", err)
	}
	if _, err := st.Get(ctx, "t1", "c1"); err != nil {
		t.Fatalf("Get own tenant: %v", err)
	}
	// So is the refresh sweep's CAS …
	if _, err := st.RevokeIfPayload(ctx, "t2", "c1", []byte("v1"), time.Now(), false); !errors.Is(err, ErrForgeConnectionNotFound) {
		t.Fatalf("RevokeIfPayload across tenants: err = %v, want ErrForgeConnectionNotFound", err)
	}
	// … and a delete.
	if err := st.Delete(ctx, "t2", "c1"); !errors.Is(err, ErrForgeConnectionNotFound) {
		t.Fatalf("Delete across tenants: err = %v, want ErrForgeConnectionNotFound", err)
	}
	// An update may not MOVE the record between tenants: that would be a
	// silent grant of one tenant's credential to another, written as an
	// ordinary edit.
	moved, err := st.Get(ctx, "t1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	moved.TenantID = "t2"
	if err := st.Update(ctx, moved); !errors.Is(err, ErrForgeConnectionNotFound) {
		t.Fatalf("Update moving the tenant: err = %v, want ErrForgeConnectionNotFound", err)
	}
	if _, err := st.Get(ctx, "t2", "c1"); !errors.Is(err, ErrForgeConnectionNotFound) {
		t.Fatalf("the update moved the record: err = %v", err)
	}
}

// RevokeIfPayload is the compare-and-swap the refresh worker's markRevoked
// relies on: the write lands only while the stored credential is the one the
// caller loaded, and it names only the fields a revocation owns.
func TestForgeStore_RevokeIfPayloadIsAPartialCompareAndSwap(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		forgeStoreCASContract(t, NewMemoryForgeStore())
	})
	t.Run("mongo", func(t *testing.T) {
		forgeStoreCASContract(t, newTestMongoForgeStore(t))
	})
}

func forgeStoreCASContract(t *testing.T, st ForgeStore) {
	t.Helper()
	ctx := context.Background()
	exp := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	if err := st.Create(ctx, ForgeConnection{ID: "c1", TenantID: "t1", Status: StatusActive, SealedPayload: []byte("v1"), AccessTokenExpiresAt: &exp, DisplayName: "jo", WebhookBaseURL: "https://hook.example"}); err != nil {
		t.Fatal(err)
	}
	at := exp.Add(time.Hour)

	if ok, err := st.RevokeIfPayload(ctx, "t1", "c1", []byte("v0"), at, false); err != nil || ok {
		t.Fatalf("mismatching payload: written=%v err=%v, want false, nil", ok, err)
	}
	if got, _ := st.Get(ctx, "t1", "c1"); got.Status != StatusActive {
		t.Fatalf("a mismatching CAS wrote the row: status %q", got.Status)
	}
	if ok, err := st.RevokeIfPayload(ctx, "t1", "c1", []byte("v1"), at, false); err != nil || !ok {
		t.Fatalf("matching payload: written=%v err=%v, want true, nil", ok, err)
	}
	got, _ := st.Get(ctx, "t1", "c1")
	if got.Status != StatusNeedsReauth || !got.UpdatedAt.Equal(at) {
		t.Fatalf("a matching CAS did not flip the row: status %q updated_at %v", got.Status, got.UpdatedAt)
	}
	if got.DisplayName != "jo" || got.WebhookBaseURL != "https://hook.example" || got.AccessTokenExpiresAt == nil {
		t.Fatalf("the revoke touched fields it does not own: %+v", got)
	}
	if ok, _ := st.RevokeIfPayload(ctx, "t1", "c1", []byte("v1"), at, true); !ok {
		t.Fatal("clearExpiry: want written")
	}
	if got, _ := st.Get(ctx, "t1", "c1"); got.AccessTokenExpiresAt != nil {
		t.Fatal("clearExpiry must drop the recorded expiry")
	}
	if _, err := st.RevokeIfPayload(ctx, "t1", "nope", []byte("v1"), at, false); !errors.Is(err, ErrForgeConnectionNotFound) {
		t.Fatalf("missing row: err = %v, want ErrForgeConnectionNotFound", err)
	}
}

// The #2698 contract, held against both twins: the refresh sweep's scan never
// hands a connection whose credential is already dead (needs_reauth, revoked)
// to the refresher — before the status filter, every such row kept its past
// expiry and was swept into a guaranteed invalid_grant on every tick.
func TestForgeExpiringBefore_SkipsDeadCredentials(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		forgeExpiringBeforeContract(t, NewMemoryForgeStore())
	})
	t.Run("mongo", func(t *testing.T) {
		forgeExpiringBeforeContract(t, newTestMongoForgeStore(t))
	})
}

func forgeExpiringBeforeContract(t *testing.T, st ForgeStore) {
	t.Helper()
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	seed := func(c ForgeConnection) {
		t.Helper()
		if err := st.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	seed(ForgeConnection{ID: "due-active", TenantID: "t1", Kind: KindOAuthApp, Status: StatusActive, AccessTokenExpiresAt: &past})
	// The ticket's case: a needs_reauth row keeps its past expiry.
	seed(ForgeConnection{ID: "due-needs-reauth", TenantID: "t1", Kind: KindOAuthApp, Status: StatusNeedsReauth, AccessTokenExpiresAt: &past})
	seed(ForgeConnection{ID: "due-revoked", TenantID: "t1", Kind: KindOAuthApp, Status: StatusRevoked, AccessTokenExpiresAt: &past})
	// Degraded stays listed: the security-read lane still owes it a pass.
	seed(ForgeConnection{ID: "due-degraded", TenantID: "t1", Kind: KindGitHubApp, Status: StatusDegraded, AccessTokenExpiresAt: &past})
	// A row from before the status field keeps its historical behaviour.
	seed(ForgeConnection{ID: "due-no-status", TenantID: "t1", Kind: KindOAuthApp, AccessTokenExpiresAt: &past})
	seed(ForgeConnection{ID: "pat", TenantID: "t1", Kind: KindPAT, Status: StatusRevoked, AccessTokenExpiresAt: &past})
	seed(ForgeConnection{ID: "not-due", TenantID: "t1", Kind: KindOAuthApp, Status: StatusActive, AccessTokenExpiresAt: &future})

	due, err := st.ExpiringBefore(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range due {
		got[c.ID] = true
	}
	for _, want := range []string{"due-active", "due-degraded", "due-no-status"} {
		if !got[want] {
			t.Errorf("%s missing from the sweep", want)
		}
	}
	for _, refuse := range []string{"due-needs-reauth", "due-revoked", "pat", "not-due"} {
		if got[refuse] {
			t.Errorf("%s handed to the sweep — a dead credential is a guaranteed invalid_grant per tick", refuse)
		}
	}
}
