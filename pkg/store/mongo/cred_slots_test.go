package mongo

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The one eviction the cross-backend row cannot reach without a clock: a
// holder whose run document never came keeps its slot inside
// store.CredSlotGrace and loses it past it. Written straight into the row,
// the way a launch that died between its two writes leaves it. Skipped
// without ITERION_TEST_MONGO_URI, like the conformance suite.
func TestCredSlots_AHolderWithNoRunDocumentIsEvictedPastTheGrace(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo cred-slot test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_credslots_" + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	const fp = "fp-capped"
	stale := time.Now().UTC().Add(-store.CredSlotGrace - time.Minute)
	if _, err := s.credSlots.InsertOne(ctx, bson.M{"_id": fp, "holders": bson.A{bson.M{"run_id": "ghost-old", "at": stale}}}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	tctx := store.WithIdentity(context.Background(), "_test", "_test")
	if _, err := s.CreateRun(tctx, "live", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.SetRunCredStamp(tctx, "live", store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
		t.Fatalf("SetRunCredStamp: %v", err)
	}
	adm, err := s.ReserveCredSlot(tctx, fp, "live", 1)
	if err != nil {
		t.Fatalf("ReserveCredSlot: %v", err)
	}
	if !adm.Granted || adm.Held != 1 {
		t.Fatalf("a holder with no document past the grace kept its slot: %+v", adm)
	}
	// A fresh ghost (inside the grace) holds its slot against the next run.
	if _, err := s.credSlots.UpdateOne(ctx, bson.M{"_id": fp}, bson.M{"$push": bson.M{"holders": bson.M{"run_id": "ghost-fresh", "at": time.Now().UTC()}}}); err != nil {
		t.Fatalf("seed fresh ghost: %v", err)
	}
	if _, err := s.CreateRun(tctx, "late", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.SetRunCredStamp(tctx, "late", store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
		t.Fatalf("SetRunCredStamp: %v", err)
	}
	if adm, err := s.ReserveCredSlot(tctx, fp, "late", 2); err != nil || adm.Granted {
		t.Fatalf("a holder inside the grace was evicted: %+v %v", adm, err)
	}
	// Deleting a run frees its slot at once, not at the grace.
	if err := s.DeleteRun(tctx, "live"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	var row credSlotDoc
	if err := s.credSlots.FindOne(ctx, bson.M{"_id": fp}).Decode(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.holds("live") {
		t.Fatalf("a deleted run still holds a slot: %+v", row.Holders)
	}
}
