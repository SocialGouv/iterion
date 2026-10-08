package mongo

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// D14 F7: an IR blob read is MEDIATED by the tenant of the run the key
// addresses. The runner's ctx carries its own run's tenant; a foreign
// tenant's key — and an unattributed caller — refuse with the same
// not-found the blob itself would return. Gated on ITERION_TEST_MONGO_URI
// like the conformance suite (needs a real replica set).
func TestIRBlobReadIsTenantMediated(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo IR blob mediation")
	}
	dbName := "iterion_ir_mediation_" + bsonNonce(t)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: dbName,
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})

	seed := func(t *testing.T, runID, tenant string) context.Context {
		t.Helper()
		tctx := store.WithTenant(ctx, tenant)
		if _, err := s.CreateRun(tctx, runID, "wf", nil); err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}
		return tctx
	}
	seed(t, "run-a", "tenant-a")
	seed(t, "run-b", "tenant-b")

	keyA, err := s.PutIRBlob(ctx, "run-a", []byte(`{"ir":"a"}`))
	if err != nil {
		t.Fatalf("PutIRBlob a: %v", err)
	}

	// The owner reads its own blob.
	if body, err := s.GetIRBlob(store.WithTenant(ctx, "tenant-a"), keyA); err != nil || string(body) != `{"ir":"a"}` {
		if _, lerr := s.LoadRun(store.WithTenant(ctx, "tenant-a"), "run-a"); lerr != nil {
			t.Fatalf("owner read: body=%q err=%v; LoadRun directly: %v", body, err, lerr)
		}
		t.Fatalf("owner read: body=%q err=%+v (LoadRun alone works)", body, err)
	}

	// A foreign tenant's ctx: refused with not-found, not the bytes.
	if _, err := s.GetIRBlob(store.WithTenant(ctx, "tenant-b"), keyA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign-tenant read: err = %v, want os.ErrNotExist", err)
	}

	// An unattributed caller: refused outright (fail closed).
	if _, err := s.GetIRBlob(ctx, keyA); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unattributed read: err = %v, want a mediation refusal", err)
	}

	// The snapshot shape: the embedded "<runID>-bundle-<digest>" resolves
	// to the owning run, so a foreign tenant is refused there too.
	snapKey, err := s.PutIRBlob(ctx, "run-a-bundle-deadbeef", []byte(`{"snapshot":true}`))
	if err != nil {
		t.Fatalf("PutIRBlob snapshot: %v", err)
	}
	if _, err := s.GetIRBlob(store.WithTenant(ctx, "tenant-b"), snapKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign snapshot read: err = %v, want os.ErrNotExist", err)
	}
	if body, err := s.GetIRBlob(store.WithTenant(ctx, "tenant-a"), snapKey); err != nil || string(body) != `{"snapshot":true}` {
		t.Fatalf("owner snapshot read: body=%q err=%v", body, err)
	}

	// A mediated read whose blob is genuinely absent: the owner's tenant
	// passes the mediation, and the blob layer's not-found maps to
	// os.ErrNotExist as before.
	absentKey, err := blob.IRBlobKey("run-a-absent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIRBlob(store.WithTenant(ctx, "tenant-a"), absentKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent blob for an owned run: err = %v, want os.ErrNotExist", err)
	}
}
