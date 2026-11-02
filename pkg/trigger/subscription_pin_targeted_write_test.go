package trigger

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// seedPinnedSubscription writes one subscription holding pins on both maps
// plus every health stamp the lanes write, and returns it as stored.
func seedPinnedSubscription(t *testing.T, st SubscriptionStore, ctx context.Context, id string) Subscription {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	sub := Subscription{
		ID: id, TenantID: "t1", BotID: "review-pr", Enabled: true,
		KeyOverrides:    map[string]string{"anthropic": "key-dead", "openai": "key-alive"},
		SecretOverrides: map[string]string{"forge_token": "sec-dead"},
		LastError:       "previous launch verdict", LastErrorAt: &now,
		LastRunID: "run-1", LastRunStatus: "success", LastRunAt: &now,
	}
	if err := st.Create(ctx, sub); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// assertPinsReplaced pins the sweep's contract on one store: the two pin
// maps are replaced wholesale (never merged) and the health stamps the
// evaluator and the run-outcome spine wrote survive a write from a caller
// that only ever held a listing copy of the row (#2644).
func assertPinsReplaced(t *testing.T, st SubscriptionStore, ctx context.Context, id string) {
	t.Helper()
	if err := st.SetCredentialPins(ctx, id, map[string]string{"openai": "key-alive"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.KeyOverrides) != 1 || got.KeyOverrides["openai"] != "key-alive" {
		t.Fatalf("key_overrides = %v, want only the surviving pin — a merged map would read as pins the sweep dropped", got.KeyOverrides)
	}
	if len(got.SecretOverrides) != 0 {
		t.Fatalf("secret_overrides = %v, want the clear", got.SecretOverrides)
	}
	if got.LastError != "previous launch verdict" || got.LastErrorAt == nil {
		t.Fatalf("the pin write clobbered the launch verdict %q — a health write landing between the sweep's list and its set must survive", got.LastError)
	}
	if got.LastRunID != "run-1" || got.LastRunStatus != "success" || got.LastRunAt == nil {
		t.Fatalf("the pin write clobbered the run outcome: %s/%s/%v", got.LastRunID, got.LastRunStatus, got.LastRunAt)
	}

	// A row that died between the sweep's list and its write has no pins
	// left to clear: ErrSubscriptionNotFound, never a silent success.
	if err := st.SetCredentialPins(ctx, "absent", nil, nil); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("SetCredentialPins on a missing subscription = %v, want ErrSubscriptionNotFound", err)
	}
}

func TestSetCredentialPins_IsATargetedWrite(t *testing.T) {
	st := NewMemorySubscriptionStore()
	seedPinnedSubscription(t, st, context.Background(), "swept")
	assertPinsReplaced(t, st, context.Background(), "swept")
}

func TestMongoSubscriptionStoreSetCredentialPinsIsATargetedWrite(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; requires Mongo conformance harness")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("iterion_pin_sweep_" + bson.NewObjectID().Hex())
	t.Cleanup(func() {
		dropCtx, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(dropCtx)
		_ = client.Disconnect(dropCtx)
	})
	st := NewMongoSubscriptionStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	seedPinnedSubscription(t, st, ctx, "swept")
	assertPinsReplaced(t, st, ctx, "swept")

	// The raw document carries the proof the memory twin cannot: the
	// $unset really removed the field (omitempty parity with Create) and
	// the untouched health fields are still in it.
	coll := db.Collection(SubscriptionsCollectionName)
	var raw bson.M
	if err := coll.FindOne(ctx, bson.M{"_id": "swept"}).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["key_overrides"]; !present {
		t.Fatal("key_overrides missing from the raw document — the surviving pin must be stored, not defaulted")
	}
	if _, present := raw["secret_overrides"]; present {
		t.Fatal("secret_overrides still present after the clear — a stored empty map reads as a pin that names nothing")
	}
	if _, present := raw["last_error"]; !present {
		t.Fatal("last_error missing from the raw document — the targeted write dropped a health field")
	}
}
