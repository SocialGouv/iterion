package mongo

import (
	"context"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Item 12 of #2401: DeleteRun swept run_seq with {run_id: id}, and the
// sequence counter has no run_id field — it is keyed by the compound _id
// {tenant_id, run_id} (events.go). Every deleted run therefore left its
// counter behind for the life of the database, and no test could see it: the
// delete reported success, because DeleteMany matching nothing is not an
// error.
//
// The witness reads the COLLECTION after the delete, not the delete's return
// value: the defect was invisible precisely because the call succeeded.
func newRunSeqTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo run_seq delete test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_runseq_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = s.db.Drop(drop)
	})
	return s
}

func TestDeleteRunLeavesNoSequenceCounter(t *testing.T) {
	s := newRunSeqTestStore(t)
	ctx := store.WithIdentity(context.Background(), "t-seq", "u1")
	const runID = "run-seq-delete"

	if _, err := s.CreateRun(ctx, runID, "wf", map[string]any{}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Allocating a seq is what MINTS the counter document — the append path
	// every event takes.
	if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventRunStarted, RunID: runID}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	minted, err := s.runSeq.CountDocuments(ctx, bson.M{"_id.run_id": runID})
	if err != nil {
		t.Fatalf("count minted counters: %v", err)
	}
	if minted != 1 {
		t.Fatalf("run_seq documents for the run before the delete = %d, want 1 — the counter is not minted where this witness thinks it is, so its verdict after the delete would mean nothing", minted)
	}
	// A second tenant's counter for the SAME run id: the compound key exists
	// to keep them apart (events.go), and the delete must not reach across
	// it. Inserted in the shape the allocation writes — the count above
	// proves that shape is the one production mints — because a run id is
	// globally unique in the runs collection, so the second counter cannot
	// be reached through a second CreateRun.
	if _, err := s.runSeq.InsertOne(ctx, bson.M{
		"_id":      bson.M{"tenant_id": "t-other", "run_id": runID},
		"next_seq": int64(3),
	}); err != nil {
		t.Fatalf("seed the other tenant's counter: %v", err)
	}

	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}

	left, err := s.runSeq.CountDocuments(ctx, bson.M{"_id.tenant_id": "t-seq", "_id.run_id": runID})
	if err != nil {
		t.Fatalf("count counters after delete: %v", err)
	}
	if left != 0 {
		t.Errorf("run_seq documents left for the deleted run = %d, want 0 — the counter outlives the run it counted", left)
	}
	kept, err := s.runSeq.CountDocuments(ctx, bson.M{"_id.tenant_id": "t-other", "_id.run_id": runID})
	if err != nil {
		t.Fatalf("count the other tenant's counters: %v", err)
	}
	if kept != 1 {
		t.Errorf("the other tenant's counter for the same run id = %d, want 1 — the delete crossed the compound key", kept)
	}
}

// TestDeleteRunReapsTheLegacySequenceCounter covers the pre-compound cohort
// named in events.go: a counter whose _id is the plain run id. It is the same
// counter under its old key, so a delete that leaves it leaves the orphan this
// repair exists to stop.
func TestDeleteRunReapsTheLegacySequenceCounter(t *testing.T) {
	s := newRunSeqTestStore(t)
	ctx := store.WithIdentity(context.Background(), "t-legacy", "u1")
	const runID = "run-seq-legacy"

	if _, err := s.CreateRun(ctx, runID, "wf", map[string]any{}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := s.runSeq.InsertOne(ctx, bson.M{"_id": runID, "next_seq": int64(7)}); err != nil {
		t.Fatalf("seed the legacy counter: %v", err)
	}

	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}

	left, err := s.runSeq.CountDocuments(ctx, bson.M{"_id": runID})
	if err != nil {
		t.Fatalf("count the legacy counter after delete: %v", err)
	}
	if left != 0 {
		t.Errorf("legacy run_seq document left = %d, want 0", left)
	}
}
