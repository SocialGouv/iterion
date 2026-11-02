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

// assertPinsClearedByValue pins the sweep's contract on one store: the
// clear removes ONLY the entries whose value names the deleted record, and
// it judges the row as the store holds it at write time — the health
// stamps the evaluator and the run-outcome spine wrote, and any pin a
// concurrent writer landed after the sweep's list, survive a write from a
// caller that only ever held a listing copy of the row (#2644).
func assertPinsClearedByValue(t *testing.T, st SubscriptionStore, ctx context.Context, id string) {
	t.Helper()
	// THE FINDING'S WINDOW: concurrent writers land between the sweep's
	// list and its clear — a health stamp (the machine lane's most
	// frequent write to the row) and an operator's pin edit adding a pin
	// the listing copy cannot know.
	if err := st.MarkLaunchError(ctx, id, "launched under the dead key's default", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sub, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	sub.SecretOverrides["added"] = "sec-added"
	if err := st.Update(ctx, sub); err != nil {
		t.Fatal(err)
	}

	cleared, err := st.ClearCredentialPins(ctx, id, "key-dead")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("the clear reported nothing removed — the dead pin is still stored")
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.KeyOverrides) != 1 || got.KeyOverrides["openai"] != "key-alive" {
		t.Fatalf("key_overrides = %v, want only the surviving pin — a stored pin naming the deleted key seals the team's default in silence", got.KeyOverrides)
	}
	// The landing pin edit survives: the clear judged the stored row, not
	// the caller's listing copy — a wholesale rewrite from that copy would
	// drop "added", an explicit choice lost in silence.
	if got.SecretOverrides["added"] != "sec-added" {
		t.Fatalf("secret_overrides = %v — the clear clobbered a pin written after the sweep's snapshot", got.SecretOverrides)
	}
	// A key sweep is a KEY sweep: the secret pins that name no deleted key
	// do not move.
	if got.SecretOverrides["forge_token"] != "sec-dead" {
		t.Fatalf("secret_overrides = %v — the key clear moved a pin that does not name the deleted key", got.SecretOverrides)
	}
	if got.LastError != "launched under the dead key's default" || got.LastErrorAt == nil {
		t.Fatalf("the pin clear clobbered the launch verdict %q — a health write landing between the sweep's list and its clear must survive", got.LastError)
	}
	if got.LastRunID != "run-1" || got.LastRunStatus != "success" || got.LastRunAt == nil {
		t.Fatalf("the pin clear clobbered the run outcome: %s/%s/%v", got.LastRunID, got.LastRunStatus, got.LastRunAt)
	}

	// A row holding no such pin is a no-op, not a failure.
	cleared, err = st.ClearCredentialPins(ctx, id, "key-never-pinned")
	if err != nil || cleared {
		t.Fatalf("a no-op clear = (%v, %v), want (false, nil)", cleared, err)
	}

	// A row that died between the sweep's list and its clear has no pins
	// left to clear: ErrSubscriptionNotFound, never a silent success.
	cleared, err = st.ClearCredentialPins(ctx, "absent", "key-dead")
	if !errors.Is(err, ErrSubscriptionNotFound) || cleared {
		t.Fatalf("ClearCredentialPins on a missing subscription = (%v, %v), want (false, ErrSubscriptionNotFound)", cleared, err)
	}
}

// The drop is unconditional across BOTH maps. The contract removes every
// entry whose value names recordID, and a row whose key pins AND secret
// pins name the same id — reachable across the two id spaces only by a
// collision or a hand-seeded store, but the contract carves that out
// nowhere — must leave neither standing: a short-circuit on the first
// map's drop leaves the second map's dead pin stored under a "cleared"
// answer, and the memory twin then diverges from its Mongo twin, which
// scans both.
func assertBothMapsSwept(t *testing.T, st SubscriptionStore, ctx context.Context) {
	t.Helper()
	if err := st.Create(ctx, Subscription{
		ID: "both-maps", TenantID: "t1", BotID: "review-pr", Enabled: true,
		KeyOverrides:    map[string]string{"anthropic": "dead-everywhere", "openai": "key-alive"},
		SecretOverrides: map[string]string{"forge_token": "dead-everywhere"},
	}); err != nil {
		t.Fatal(err)
	}
	cleared, err := st.ClearCredentialPins(ctx, "both-maps", "dead-everywhere")
	if err != nil || !cleared {
		t.Fatalf("the both-maps clear = (%v, %v), want (true, nil)", cleared, err)
	}
	got, err := st.Get(ctx, "both-maps")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.KeyOverrides) != 1 || got.KeyOverrides["openai"] != "key-alive" {
		t.Fatalf("key_overrides = %v, want only the surviving pin", got.KeyOverrides)
	}
	if _, stands := got.SecretOverrides["forge_token"]; stands {
		t.Fatalf("secret_overrides = %v — the secret map was never scanned once the key map dropped something; the contract removes every entry naming the record", got.SecretOverrides)
	}
}

func TestClearCredentialPins_IsAValueGuardedWriteTimeClear(t *testing.T) {
	st := NewMemorySubscriptionStore()
	seedPinnedSubscription(t, st, context.Background(), "swept")
	assertPinsClearedByValue(t, st, context.Background(), "swept")
	assertBothMapsSwept(t, st, context.Background())
}

func TestMongoSubscriptionStoreClearCredentialPinsIsAValueGuardedWriteTimeClear(t *testing.T) {
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
	assertPinsClearedByValue(t, st, ctx, "swept")
	assertBothMapsSwept(t, st, ctx)

	// The raw document carries the proof the memory twin cannot: the
	// per-entry $unset really removed the dead ENTRY (not a defaulted
	// field) while the surviving pin, and every untouched health field,
	// are still in it. An emptied field may linger as an empty document —
	// a whole-row write re-omits it (omitempty) and every read treats it
	// as the clear — so the entry level is where the proof lives.
	coll := db.Collection(SubscriptionsCollectionName)
	var raw struct {
		KeyOverrides    map[string]string `bson:"key_overrides"`
		SecretOverrides map[string]string `bson:"secret_overrides"`
		LastError       string            `bson:"last_error"`
	}
	if err := coll.FindOne(ctx, bson.M{"_id": "swept"}).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, dead := raw.KeyOverrides["anthropic"]; dead {
		t.Fatalf("the dead pin entry is still in the raw document (%v) — the clear must remove the entry, not default the field", raw.KeyOverrides)
	}
	if raw.KeyOverrides["openai"] != "key-alive" {
		t.Fatalf("the surviving pin entry is gone from the raw document: %v", raw.KeyOverrides)
	}
	if len(raw.SecretOverrides) != 2 || raw.SecretOverrides["forge_token"] != "sec-dead" || raw.SecretOverrides["added"] != "sec-added" {
		t.Fatalf("secret_overrides = %v — the key clear moved pins that do not name the deleted key", raw.SecretOverrides)
	}
	if raw.LastError == "" {
		t.Fatal("last_error missing from the raw document — the clear dropped a health field")
	}
}

// seedLegacyNamedSubscription writes one subscription whose secret pins
// carry names a Mongo path cannot address — a '.' reads as a nested path
// and a $ names an operator, so a targeted write addressing
// "<field>.<name>" would match nothing stored under the literal key. New
// names cannot hold those bytes (the routes' validators refuse them);
// rows written before that guard existed can.
func seedLegacyNamedSubscription(t *testing.T, st SubscriptionStore, ctx context.Context, id string) {
	t.Helper()
	if err := st.Create(ctx, Subscription{
		ID: id, TenantID: "t1", BotID: "review-pr", Enabled: true,
		KeyOverrides: map[string]string{"anthropic": "key-alive"},
		SecretOverrides: map[string]string{
			"dead.name":   "rec-dead",
			"$rooted":     "rec-dead",
			"forge_token": "rec-dead",
			"mid$name":    "rec-alive",
			"live":        "rec-alive",
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// assertLegacyNamedPinsSwept pins the clear's two-arm contract on one
// store: an unaddressable-named entry whose value names the deleted record
// is dropped by the subdocument rewrite, not left standing behind a
// silent no-match, and everything the clear does not name survives —
// including a plain-named pin landed after the clear, which the rewrite
// must judge from the row as the store holds it NOW.
func assertLegacyNamedPinsSwept(t *testing.T, st SubscriptionStore, ctx context.Context, id string) {
	t.Helper()
	cleared, err := st.ClearCredentialPins(ctx, id, "rec-dead")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("the clear reported nothing removed — the legacy-named dead pin is still stored and no delete can ever revisit it")
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dead.name", "$rooted", "forge_token"} {
		if _, standing := got.SecretOverrides[name]; standing {
			t.Fatalf("legacy-named pin %q survives the clear: %v — the $unset arm cannot address it and the dead pin stands while the delete answers 204", name, got.SecretOverrides)
		}
	}
	if got.SecretOverrides["mid$name"] != "rec-alive" || got.SecretOverrides["live"] != "rec-alive" {
		t.Fatalf("secret_overrides = %v — the clear moved pins naming another record", got.SecretOverrides)
	}
	if got.KeyOverrides["anthropic"] != "key-alive" {
		t.Fatalf("key_overrides = %v — the secret clear moved the other map", got.KeyOverrides)
	}

	// A pin landed after the first sweep survives a SECOND legacy sweep of
	// the same row: the rewrite judges the row as the store holds it now,
	// never the caller's snapshot — a wholesale write of a stale map would
	// drop "added-later" here.
	got.SecretOverrides["added-later"] = "sec-added"
	if err := st.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	cleared, err = st.ClearCredentialPins(ctx, id, "rec-alive")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("the second clear reported nothing removed — both rec-alive pins are stored")
	}
	got, err = st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, standing := got.SecretOverrides["added-later"]; !standing {
		t.Fatalf("secret_overrides = %v — the rewrite wrote a stale map back and clobbered a pin landed in between", got.SecretOverrides)
	}
	if _, standing := got.SecretOverrides["mid$name"]; standing {
		t.Fatalf("the second clear left the unaddressable-named pin behind: %v", got.SecretOverrides)
	}

	// Every entry naming the record is gone: a repeat is the wanted state,
	// reporting nothing cleared and writing nothing.
	steady, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err = st.ClearCredentialPins(ctx, id, "rec-dead")
	if err != nil || cleared {
		t.Fatalf("a repeat clear of swept legacy pins = (%v, %v), want (false, nil)", cleared, err)
	}
	after, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != steady.Version {
		t.Fatalf("a no-op clear moved the version %d → %d — the fence must not punish an editor who did nothing", steady.Version, after.Version)
	}
}

// The memory twin has no paths to mis-address, which is the point of
// running the same contract on it: the two arms answer identically.
func TestClearCredentialPins_DropsALegacyNameThePathCannotAddress(t *testing.T) {
	st := NewMemorySubscriptionStore()
	ctx := context.Background()
	seedLegacyNamedSubscription(t, st, ctx, "legacy")
	assertLegacyNamedPinsSwept(t, st, ctx, "legacy")
}

func TestMongoSubscriptionStoreClearCredentialPinsDropsALegacyNameThePathCannotAddress(t *testing.T) {
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
	db := client.Database("iterion_pin_legacy_" + bson.NewObjectID().Hex())
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
	seedLegacyNamedSubscription(t, st, ctx, "legacy")
	assertLegacyNamedPinsSwept(t, st, ctx, "legacy")

	// The raw document carries the proof the memory twin cannot: the
	// rewrite really removed the unaddressable ENTRY from the stored
	// subdocument, and the surviving entries are still in it.
	coll := db.Collection(SubscriptionsCollectionName)
	var raw struct {
		SecretOverrides map[string]string `bson:"secret_overrides"`
	}
	if err := coll.FindOne(ctx, bson.M{"_id": "legacy"}).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.SecretOverrides) != 1 || raw.SecretOverrides["added-later"] != "sec-added" {
		t.Fatalf("raw secret_overrides = %v, want only the pin landed after the sweeps", raw.SecretOverrides)
	}
}
