package mongo

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// newTagsTestStore mirrors newPlanTestStore: a throwaway Mongo store, or a
// skip when ITERION_TEST_MONGO_URI is unset. Run with:
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27017' \
//	    devbox run -- go test ./pkg/store/mongo/ -run Tag
func newTagsTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo tag-store test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_tags_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = s.db.Drop(drop)
	})
	return s
}

func TestRunTagStore_RoundTrip(t *testing.T) {
	s := newTagsTestStore(t)
	ctx := store.WithIdentity(context.Background(), "t1", "u1")
	const runID = "run-tag-rt"

	got, err := s.GetRunTags(ctx, runID)
	if err != nil {
		t.Fatalf("get (empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("unset tags = %v, want empty", got)
	}

	want := []string{"release", "flaky"}
	if err := s.SetRunTags(ctx, runID, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ = s.GetRunTags(ctx, runID)
	if len(got) != 2 || got[0] != "release" || got[1] != "flaky" {
		t.Errorf("get = %v, want %v", got, want)
	}

	// Overwrite replaces the whole set.
	if err := s.SetRunTags(ctx, runID, []string{"customer-x"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = s.GetRunTags(ctx, runID)
	if len(got) != 1 || got[0] != "customer-x" {
		t.Errorf("after overwrite = %v, want [customer-x]", got)
	}
}

func TestRunTagStore_TenantScoping(t *testing.T) {
	s := newTagsTestStore(t)
	acme := store.WithIdentity(context.Background(), "acme", "u1")
	globex := store.WithIdentity(context.Background(), "globex", "u2")
	const runID = "run-tag-tenant"

	if err := s.SetRunTags(acme, runID, []string{"acme-tag"}); err != nil {
		t.Fatalf("set acme: %v", err)
	}
	// A different tenant sees nothing for the same run id.
	got, err := s.GetRunTags(globex, runID)
	if err != nil {
		t.Fatalf("get globex: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("globex tags = %v, want 0 (tenant leak)", got)
	}
}

func TestRunTagStore_DeleteRunCleanup(t *testing.T) {
	s := newTagsTestStore(t)
	ctx := store.WithIdentity(context.Background(), "t1", "u1")
	const runID = "run-tag-del"

	if err := s.SaveRun(ctx, &store.Run{ID: runID, WorkflowName: "wf", Status: store.RunStatusFinished}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := s.SetRunTags(ctx, runID, []string{"x"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	got, err := s.GetRunTags(ctx, runID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after DeleteRun, tags = %v, want 0 (orphaned run_tags)", got)
	}
}
