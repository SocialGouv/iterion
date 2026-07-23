package botsource

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

// TestMongoStore runs the bot-source store against a real Mongo (same env gating
// as the other Mongo conformance suites). It proves the cloud store honors
// tenant isolation, (tenant, slug) uniqueness, and version-guarded updates —
// the invariants the memory store test asserts, but on the durable backend.
func TestMongoStore(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo botsource suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_botsource_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})

	st := NewMongoStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema (idempotent): %v", err)
	}

	t1 := store.WithTenant(ctx, "team-1")
	t2 := store.WithTenant(ctx, "team-2")

	created, err := st.Create(t1, validSource("team-1", "reviewer"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || created.Version != 1 || created.Origin != "tenant" {
		t.Fatalf("Create defaults wrong: %+v", created)
	}

	// (tenant, slug) unique within a team; free across teams.
	if _, err := st.Create(t1, validSource("team-1", "reviewer")); !errors.Is(err, ErrSlugConflict) {
		t.Fatalf("want ErrSlugConflict, got %v", err)
	}
	if _, err := st.Create(t2, validSource("team-2", "reviewer")); err != nil {
		t.Fatalf("cross-tenant same slug must be allowed: %v", err)
	}

	// Missing tenant context fails closed.
	if _, err := st.Create(ctx, validSource("team-1", "x")); !errors.Is(err, ErrTenantMissing) {
		t.Fatalf("want ErrTenantMissing without tenant ctx, got %v", err)
	}

	// GetBySlug is tenant-scoped.
	got, err := st.GetBySlug(t1, "team-1", "reviewer")
	if err != nil || got.ID != created.ID {
		t.Fatalf("GetBySlug: %v id=%s", err, got.ID)
	}
	if _, err := st.GetBySlug(t1, "team-3", "reviewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for foreign tenant, got %v", err)
	}

	// Version-guarded update.
	created.Files["skills/help.md"] = "# help"
	updated, err := st.Update(t1, created)
	if err != nil || updated.Version != 2 {
		t.Fatalf("Update: %v version=%d", err, updated.Version)
	}
	created.Version = 1 // stale
	if _, err := st.Update(t1, created); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}

	// A foreign tenant cannot mutate or read across the boundary.
	if _, err := st.Get(t2, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Get must be ErrNotFound, got %v", err)
	}
	if err := st.Delete(t2, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Delete must be ErrNotFound, got %v", err)
	}

	list, err := st.ListByTenant(ctx, "team-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByTenant team-1: %v len=%d", err, len(list))
	}

	if err := st.Delete(t1, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(t1, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}
