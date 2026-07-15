package mongo

import (
	"context"
	"fmt"
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
	// Mongo is cloud-only and every query is tenant-scoped:
	// withTenantFilter fail-closed-panics on a ctx with no tenant, so use
	// an identity-carrying ctx (the conformance suite does the same).
	ctx := store.WithIdentity(context.Background(), "t1", "u1")
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

// TestPlanStore_SeqIndependentOfEvents locks in the counter design: plan
// snapshots draw their seq from allocPlanSeq (the `next_plan_seq` field of
// the shared {tenant_id, run_id} counter document), independently of the
// event seq counter (`next_seq`). Interleaving event appends must not
// perturb the plan sequence — both start at 0 per run and advance on their
// own.
func TestPlanStore_SeqIndependentOfEvents(t *testing.T) {
	s := newPlanTestStore(t)
	ctx := store.WithIdentity(context.Background(), "t1", "u1")
	const runID = "run-plan-indep"

	// Burn some event seqs first (next_seq → 3); plan seqs must ignore them.
	for i := 0; i < 3; i++ {
		if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	// Distinct snapshots interleaved with more events: plan seqs are 0,1,2.
	for want := 0; want < 3; want++ {
		snap := planSnap("n", want, store.PlanTodo{Content: "step", Status: "s", ActiveForm: string(rune('a' + want))})
		got, wrote, err := s.AppendPlanSnapshot(ctx, runID, snap)
		if err != nil {
			t.Fatalf("append plan %d: %v", want, err)
		}
		if !wrote || got.Seq != want {
			t.Errorf("plan append %d: wrote=%v seq=%d, want true/%d (independent of event seq)", want, wrote, got.Seq, want)
		}
		if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatalf("interleave event %d: %v", want, err)
		}
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

	// A second tenant appends under its OWN run id (run ids are globally
	// unique, one tenant per run) and starts a fresh sequence at 0. We use
	// a distinct run id rather than reusing acme's: the (run_id, seq)
	// unique index is global (mirroring the events collection), so two
	// tenants can never legitimately share a run id — the isolation that
	// matters is the read fence asserted just above.
	const globexRunID = "run-plan-tenant-globex"
	g, wrote, err := s.AppendPlanSnapshot(globex, globexRunID, planSnap("n", 0, store.PlanTodo{Content: "globex work", Status: "pending"}))
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
	// Tenant-scoped ctx: withTenantFilter panics on a tenant-less ctx.
	ctx := store.WithIdentity(context.Background(), "t1", "u1")
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

// TestPlanStore_LegacySeqsHealCounter pins the deploy-migration path: a run
// whose snapshots were written by the pre-counter max-read allocation has
// docs at 0..N and NO counter field. The first counter-backed append
// collides on seq 0 — the write site must re-seed the counter from the
// persisted tail and land the snapshot at N+1 instead of burning every
// retry and LOSING it.
func TestPlanStore_LegacySeqsHealCounter(t *testing.T) {
	s := newPlanTestStore(t)
	ctx := store.WithTenant(context.Background(), "acme")
	runID := "run-legacy-heal"

	// Seed 15 "legacy" docs directly (no counter document), as the
	// max-read era left them.
	for i := 0; i < 15; i++ {
		doc := runPlanDoc{
			TenantID:  "acme",
			RunID:     runID,
			Seq:       i,
			NodeID:    "n",
			Iteration: 0,
			Tool:      "TodoWrite",
			Timestamp: time.Now().UTC(),
			Todos:     []store.PlanTodo{{Content: fmt.Sprintf("legacy %d", i), Status: "pending"}},
		}
		if _, err := s.runPlans.InsertOne(ctx, doc); err != nil {
			t.Fatalf("seed legacy doc %d: %v", i, err)
		}
	}

	written, wrote, err := s.AppendPlanSnapshot(ctx, runID, planSnap("n", 1, store.PlanTodo{Content: "fresh", Status: "pending"}))
	if err != nil {
		t.Fatalf("append after legacy seqs: %v", err)
	}
	if !wrote {
		t.Fatalf("append after legacy seqs: wrote=false, snapshot lost")
	}
	if written.Seq != 15 {
		t.Fatalf("healed seq = %d, want 15 (tail+1)", written.Seq)
	}
	// The counter is now ahead of the tail: the next append is collision-free.
	w2, wrote2, err := s.AppendPlanSnapshot(ctx, runID, planSnap("n", 2, store.PlanTodo{Content: "fresh 2", Status: "pending"}))
	if err != nil || !wrote2 || w2.Seq != 16 {
		t.Fatalf("post-heal append: seq=%d wrote=%v err=%v, want 16/true/nil", w2.Seq, wrote2, err)
	}
}
