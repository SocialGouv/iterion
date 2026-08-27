package usagecap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestMongoSettingsStore exercises the real Mongo settings store (same
// gating as the pkg/store/mongo conformance harness): the round trip, the
// "no record yet" nil answer, and the override-clearing replace.
func TestMongoSettingsStore(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo settings suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_usagecap_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})

	st := NewMongoSettingsStore(db)

	// Never written → (nil, nil), which the resolver reads as "env".
	rec, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if rec != nil {
		t.Fatalf("want nil record before first write, got %+v", rec)
	}

	if err := st.PutSettings(ctx, Settings{FiveHourPct: intp(80), WeekPct: intp(70), UpdatedBy: "root"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	rec, err = st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec == nil || rec.FiveHourPct == nil || *rec.FiveHourPct != 80 ||
		rec.WeekPct == nil || *rec.WeekPct != 70 || rec.UpdatedBy != "root" {
		t.Fatalf("round trip lost data: %+v", rec)
	}
	if rec.UpdatedAt.IsZero() {
		t.Fatalf("PutSettings must stamp UpdatedAt")
	}

	// Clearing an override must remove the field, not leave it lingering.
	if err := st.PutSettings(ctx, Settings{WeekPct: intp(60), UpdatedBy: "root"}); err != nil {
		t.Fatalf("put clear: %v", err)
	}
	rec, err = st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if rec.FiveHourPct != nil {
		t.Fatalf("cleared five_hour_pct must read nil, got %v", *rec.FiveHourPct)
	}
	if rec.WeekPct == nil || *rec.WeekPct != 60 {
		t.Fatalf("week_pct must survive the replace, got %+v", rec.WeekPct)
	}
}
