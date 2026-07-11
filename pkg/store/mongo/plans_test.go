package mongo

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// newPlanTestStore builds a Mongo store against a throwaway database, or
// skips when ITERION_TEST_MONGO_URI is unset — same gate as the
// conformance suite (a standalone mongod suffices; the plan paths use
// only InsertOne / Find + the unique (run_id, seq) index, no
// transactions or change streams). Run with:
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27017' \
//	    devbox run -- go test ./pkg/store/mongo/ -run Plan
func newPlanTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo plan-store test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_plans_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	return s
}

func planSnap(node string, iter int, todos ...store.PlanTodo) store.PlanSnapshot {
	return store.PlanSnapshot{
		NodeID:    node,
		Iteration: iter,
		Tool:      "TodoWrite",
		Todos:     todos,
	}
}

// TestPlanStore_AppendListDedupe locks in the core PlanStore contract on
// Mongo: sequential appends get monotonic seqs, list is chronological,
// and a byte-identical consecutive snapshot dedupes (wrote=false, no new
// document) exactly like the filesystem impl.
func TestPlanStore_AppendListDedupe(t *testing.T) {
	s := newPlanTestStore(t)
	ctx := context.Background()
	const runID = "run-plan-1"

	first := planSnap("implement", 0, store.PlanTodo{Content: "step one", Status: "pending"})
	got, wrote, err := s.AppendPlanSnapshot(ctx, runID, first)
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if !wrote {
		t.Fatal("first append: wrote=false, want true")
	}
	if got.Seq != 0 {
		t.Errorf("first seq = %d, want 0", got.Seq)
	}

	// Byte-identical resend → dedupe: no write, previous snapshot back.
	dup, wrote, err := s.AppendPlanSnapshot(ctx, runID, planSnap("implement", 0, store.PlanTodo{Content: "step one", Status: "pending"}))
	if err != nil {
		t.Fatalf("append dup: %v", err)
	}
	if wrote {
		t.Error("dup append: wrote=true, want false (byte-identical dedupe)")
	}
	if dup.Seq != 0 {
		t.Errorf("dup seq = %d, want 0 (returns previous)", dup.Seq)
	}

	// A changed plan advances the sequence.
	second := planSnap("implement", 1,
		store.PlanTodo{Content: "step one", Status: "completed"},
		store.PlanTodo{Content: "step two", Status: "in_progress"})
	got2, wrote, err := s.AppendPlanSnapshot(ctx, runID, second)
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if !wrote || got2.Seq != 1 {
		t.Errorf("second append: wrote=%v seq=%d, want true/1", wrote, got2.Seq)
	}

	list, err := s.ListPlanSnapshots(ctx, runID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2 (dedup must not persist)", len(list))
	}
	if list[0].Seq != 0 || list[1].Seq != 1 {
		t.Errorf("list seqs = %d,%d, want 0,1 (chronological)", list[0].Seq, list[1].Seq)
	}
	if len(list[1].Todos) != 2 || list[1].Todos[1].Content != "step two" {
		t.Errorf("second snapshot round-trip lost todos: %+v", list[1].Todos)
	}
}

// TestPlanStore_TenantScoping locks in that a snapshot written under one
// tenant is invisible to another — the same tenant filter run_gitmeta
// carries — while an empty (local) context sees only untenanted docs.
func TestPlanStore_TenantScoping(t *testing.T) {
	s := newPlanTestStore(t)
	const runID = "run-plan-tenant"

	acme := store.WithIdentity(context.Background(), "acme", "u1")
	globex := store.WithIdentity(context.Background(), "globex", "u2")

	if _, _, err := s.AppendPlanSnapshot(acme, runID, planSnap("n", 0, store.PlanTodo{Content: "acme work", Status: "pending"})); err != nil {
		t.Fatalf("append acme: %v", err)
	}

	got, err := s.ListPlanSnapshots(acme, runID)
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("acme list len = %d, want 1", len(got))
	}

	// A different tenant sees nothing for the same run id.
	other, err := s.ListPlanSnapshots(globex, runID)
	if err != nil {
		t.Fatalf("list globex: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("globex list len = %d, want 0 (tenant leak)", len(other))
	}

	// The tenant's own append starts a fresh sequence — globex's first
	// snapshot for the same run id is seq 0, not fenced by acme's doc.
	g, wrote, err := s.AppendPlanSnapshot(globex, runID, planSnap("n", 0, store.PlanTodo{Content: "globex work", Status: "pending"}))
	if err != nil {
		t.Fatalf("append globex: %v", err)
	}
	if !wrote || g.Seq != 0 {
		t.Errorf("globex append: wrote=%v seq=%d, want true/0", wrote, g.Seq)
	}
}

// TestPlanStore_DeleteRunCleanup locks in that DeleteRun drops the run's
// plan snapshots alongside the other child collections (mirroring
// run_gitmeta) so a deleted run leaves no orphaned run_plans docs.
func TestPlanStore_DeleteRunCleanup(t *testing.T) {
	s := newPlanTestStore(t)
	ctx := context.Background()
	const runID = "run-plan-del"

	// A run document must exist for DeleteRun's runs.DeleteOne + blob
	// deletes to run cleanly.
	if err := s.SaveRun(ctx, &store.Run{ID: runID, WorkflowName: "wf", Status: store.RunStatusFinished}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if _, _, err := s.AppendPlanSnapshot(ctx, runID, planSnap("n", 0, store.PlanTodo{Content: "x", Status: "pending"})); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.DeleteRun(ctx, runID); err != nil {
		t.Fatalf("delete run: %v", err)
	}

	got, err := s.ListPlanSnapshots(ctx, runID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after DeleteRun, plan snapshots = %d, want 0 (orphaned run_plans)", len(got))
	}
}
