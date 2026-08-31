package boardmongo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// TestMongoStore_TriggerPrimitives covers the cloud trigger spine's board
// primitives: the atomic one-shot label consume and the CAS event cursor.
// Gated on ITERION_TEST_MONGO_URI like the conformance suite (runs in the
// mongo-conformance CI job).
func TestMongoStore_TriggerPrimitives(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo trigger suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_trigger_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "trig-tenant")

	// --- ConsumeLabels: one-shot, atomic, label-preserving ---
	iss, err := st.Create(native.Issue{Title: "t", State: native.StateInbox, Labels: []string{"bug", native.LabelTriageAuto}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ok, err := st.ConsumeLabels(iss.ID, []string{native.LabelTriageAuto})
	if err != nil || !ok {
		t.Fatalf("first consume: ok=%v err=%v", ok, err)
	}
	ok, err = st.ConsumeLabels(iss.ID, []string{native.LabelTriageAuto})
	if err != nil || ok {
		t.Fatalf("second consume must lose: ok=%v err=%v", ok, err)
	}
	card, _ := st.Get(iss.ID)
	if slices.Contains(card.Labels, native.LabelTriageAuto) || !slices.Contains(card.Labels, "bug") {
		t.Fatalf("labels after consume: %v", card.Labels)
	}

	// --- Cursor: init at tip (no history replay), CAS advance one winner ---
	cur, err := st.TriggerCursor()
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if cur == 0 {
		t.Fatalf("cursor should initialise at the current tip (events exist), got 0")
	}
	if evs, _ := st.EventsAfter(cur, 100); len(evs) != 0 {
		t.Fatalf("fresh cursor must not replay history: %d events", len(evs))
	}

	// New activity → events visible past the cursor.
	if _, err := st.Create(native.Issue{Title: "u", State: native.StateInbox, Labels: []string{native.LabelTriageAuto}}); err != nil {
		t.Fatalf("create u: %v", err)
	}
	evs, err := st.EventsAfter(cur, 100)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events after cursor: n=%d err=%v", len(evs), err)
	}
	last := evs[len(evs)-1].Seq

	// Two replicas race the same batch: exactly one advance wins.
	won1, err1 := st.AdvanceTriggerCursor(cur, last)
	won2, err2 := st.AdvanceTriggerCursor(cur, last)
	if err1 != nil || err2 != nil {
		t.Fatalf("advance errs: %v %v", err1, err2)
	}
	if won1 == won2 {
		t.Fatalf("exactly one advance must win: won1=%v won2=%v", won1, won2)
	}
	if cur2, _ := st.TriggerCursor(); cur2 != last {
		t.Fatalf("cursor after advance = %d, want %d", cur2, last)
	}

	// --- Effect outbox (ADR-094): idempotent upsert, exclusive claim, marks ---
	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := []trigger.EffectRow{{
		ID: trigger.EffectID("board:b:card:9", "sub1"), TenantID: "trig-tenant",
		Event: trigger.Event{ID: "board:b:card:9", Source: trigger.SourceBoard},
		SubID: "sub1", State: trigger.EffectPending, CreatedAt: now, UpdatedAt: now,
	}}
	if err := st.UpsertPending(ctx, rows); err != nil {
		t.Fatalf("upsert effects: %v", err)
	}
	// Re-upsert after the row progressed must NOT reset it ($setOnInsert).
	claimed, err := st.ClaimDue(ctx, now, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: n=%d err=%v", len(claimed), err)
	}
	if again, _ := st.ClaimDue(ctx, now, 10); len(again) != 0 {
		t.Fatalf("second claim under a live lease returned %d rows, want 0", len(again))
	}
	if err := st.UpsertPending(ctx, rows); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if again, _ := st.ClaimDue(ctx, now, 10); len(again) != 0 {
		t.Fatal("re-upsert reset a claimed row back to pending — a racing replica would double-execute")
	}
	// Expired lease → reclaimable (orphaned worker recovery).
	if rec, _ := st.ClaimDue(ctx, now.Add(trigger.EffectLease+time.Second), 10); len(rec) != 1 {
		t.Fatal("expired-lease row not reclaimable")
	}
	// Reclaim after lease expiry counted an attempt (a hung worker's
	// attempt budget must burn down even though MarkRetry never runs).
	reclaimed, err := st.ClaimDue(ctx, now.Add(2*trigger.EffectLease+2*time.Second), 10)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaim: n=%d err=%v", len(reclaimed), err)
	}
	if reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaim attempts = %d, want 2 (each lease takeover spends one)", reclaimed[0].Attempts)
	}
	own := reclaimed[0]
	if err := st.MarkConsumed(ctx, own.ID, own.ClaimID); err != nil {
		t.Fatalf("mark consumed: %v", err)
	}
	// A STALE claim's write must be a no-op (the fence): the old claim id
	// from the first claim can no longer touch the row.
	if err := st.MarkDone(ctx, own.ID, rows[0].ClaimID+"-stale"); err != nil {
		t.Fatalf("stale mark done errored instead of no-oping: %v", err)
	}
	if err := st.MarkRetry(ctx, own.ID, own.ClaimID, own.Attempts, now.Add(-time.Second), "boom"); err != nil {
		t.Fatalf("mark retry: %v", err)
	}
	rec, err := st.ClaimDue(ctx, now, 10)
	if err != nil || len(rec) != 1 {
		t.Fatalf("claim after retry: n=%d err=%v", len(rec), err)
	}
	if !rec[0].ConsumeMarked || rec[0].Attempts != own.Attempts || rec[0].LastError != "boom" {
		t.Fatalf("retry row lost state: %+v — ConsumeMarked must survive a retry or the one-shot is double-spent/dropped", rec[0])
	}
	if err := st.MarkDone(ctx, rec[0].ID, rec[0].ClaimID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if left, _ := st.ClaimDue(ctx, now.Add(time.Hour), 10); len(left) != 0 {
		t.Fatalf("done row still claimable: %d", len(left))
	}
}
