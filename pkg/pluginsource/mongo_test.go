package pluginsource

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

// TestMongoStore runs the plugin-source store against a real Mongo (same env
// gating as the other Mongo conformance suites). The memory twin asserts the
// same invariants in pluginsource_test.go: tenant isolation, (tenant, name)
// uniqueness, and the degraded readout being owned by Mark/ClearDegraded
// alone — never rewritten by an operator's Update.
func TestMongoStore(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo pluginsource suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_pluginsource_" + hex.EncodeToString(nonce))
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

	if err := st.Create(t1, validSource()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Create(t1, validSource()); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("same (tenant, name) twice = %v, want ErrNameConflict", err)
	}
	other := validSource()
	other.TenantID = "team-2"
	if err := st.Create(t2, other); err != nil {
		t.Fatalf("the same name in another team must be allowed: %v", err)
	}
	if err := st.Create(ctx, validSource()); !errors.Is(err, ErrTenantMissing) {
		t.Fatalf("Create without a tenant = %v, want ErrTenantMissing", err)
	}

	list, err := st.ListByTenant(t1, "team-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByTenant = %d (%v), want 1", len(list), err)
	}
	s := list[0]
	if _, err := st.Get(t2, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant reading the source = %v, want ErrNotFound", err)
	}

	// Health readout: tenant-scoped, and left alone by Update.
	if err := st.MarkDegraded(ctx, "team-2", s.ID, "not yours"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkDegraded from another tenant = %v, want ErrNotFound", err)
	}
	if err := st.MarkDegraded(ctx, "team-1", s.ID, "plugin: parse manifest: yaml: line 3"); err != nil {
		t.Fatalf("MarkDegraded: %v", err)
	}
	got, err := st.Get(t1, s.ID)
	if err != nil || !got.Degraded() || got.DegradedAt == nil {
		t.Fatalf("after MarkDegraded: %+v (%v)", got, err)
	}
	got.Ref = "v1.0.1"
	got.DegradedReason = "an operator's Update must not be able to write this"
	if err := st.Update(t1, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = st.Get(t1, s.ID)
	if got.Ref != "v1.0.1" || got.DegradedReason != "plugin: parse manifest: yaml: line 3" {
		t.Fatalf("Update must apply the operator's fields and leave the health readout alone: %+v", got)
	}
	if err := st.ClearDegraded(ctx, "team-1", s.ID); err != nil {
		t.Fatalf("ClearDegraded: %v", err)
	}
	if got, _ = st.Get(t1, s.ID); got.Degraded() || got.DegradedAt != nil {
		t.Fatalf("ClearDegraded must unset both fields: %+v", got)
	}
	if err := st.ClearDegraded(ctx, "team-1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ClearDegraded on an unknown id = %v, want ErrNotFound", err)
	}

	// Enabled filtering + delete.
	got.Enabled = false
	if err := st.Update(t1, got); err != nil {
		t.Fatal(err)
	}
	if enabled, err := st.ListEnabledByTenant(t1, "team-1"); err != nil || len(enabled) != 0 {
		t.Fatalf("ListEnabledByTenant after disabling = %d (%v), want 0", len(enabled), err)
	}
	if err := st.Delete(t1, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Delete(t1, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}
