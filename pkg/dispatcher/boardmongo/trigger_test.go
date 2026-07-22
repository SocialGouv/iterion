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
}
