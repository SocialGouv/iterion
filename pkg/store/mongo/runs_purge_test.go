package mongo

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// The org purge's listing feeds a delete loop: without a tenant in the
// context it is refused before any query, rather than answering every run of
// every tenant.
func TestListRunIDsForPurge_RefusesAnUnscopedContext(t *testing.T) {
	s := &Store{}
	ids, err := s.ListRunIDsForPurge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no tenant") {
		t.Fatalf("ListRunIDsForPurge without a tenant = (%v, %v), want a refusal", ids, err)
	}
}

// TestListRunIDsForPurge_Mongo: the listing answers the tenant's live runs
// only — another tenant's run and a deleted (tombstoned) run are absent — and
// DeleteRun takes a run's bundle snapshots out of the bucket with its
// canonical IR. Gated on ITERION_TEST_MONGO_URI like the conformance suite.
func TestListRunIDsForPurge_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo purge listing")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	b := newInMemoryBlob()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_purge_list_" + bsonNonce(t), Blob: b})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	ta := store.WithTenant(ctx, "tenant-a")
	tb := store.WithTenant(ctx, "tenant-b")
	for _, id := range []string{"run-a1", "run-a2", "run-a3"} {
		if _, err := s.CreateRun(ta, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
	}
	if _, err := s.CreateRun(tb, "run-b1", "wf", nil); err != nil {
		t.Fatalf("CreateRun run-b1: %v", err)
	}
	if _, err := s.PutIRBlob(ctx, "run-a3", []byte(`{"ir":1}`)); err != nil {
		t.Fatalf("PutIRBlob: %v", err)
	}
	if _, err := s.PutIRBlob(ctx, "run-a3-bundle-d1", []byte(`{"snapshot":1}`)); err != nil {
		t.Fatalf("PutIRBlob snapshot: %v", err)
	}
	if err := s.DeleteRun(ta, "run-a3"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	for _, name := range []string{"run-a3", "run-a3-bundle-d1"} {
		key, err := blob.IRBlobKey(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.GetIRBlob(ctx, key); !errors.Is(err, blob.ErrArtifactNotFound) {
			t.Errorf("%s still in the bucket after DeleteRun: %v", key, err)
		}
	}

	ids, err := s.ListRunIDsForPurge(ta)
	if err != nil {
		t.Fatalf("ListRunIDsForPurge: %v", err)
	}
	slices.Sort(ids)
	if strings.Join(ids, ",") != "run-a1,run-a2" {
		t.Errorf("ListRunIDsForPurge(tenant-a) = %v, want run-a1,run-a2 — not run-a3 (deleted) nor run-b1 (another tenant)", ids)
	}
}
