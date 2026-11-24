package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// assertUpdateIsAVersionCAS pins the full-document update's optimistic
// concurrency on one store: an update commits only against the version its
// read carried and bumps the row's version, a stale copy conflicts instead
// of overwriting a newer write, and a missing row keeps answering
// ErrSubscriptionNotFound — the disambiguation a CAS adds (zero match = row
// gone OR version stale) must not swallow (#2644).
func assertUpdateIsAVersionCAS(t *testing.T, st SubscriptionStore, ctx context.Context) {
	t.Helper()
	if err := st.Create(ctx, Subscription{ID: "cas", TenantID: "t1", BotID: "review-pr", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Get(ctx, "cas")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 0 {
		t.Fatalf("a fresh row carries version %d, want 0", stored.Version)
	}

	// The read-modify-write at the version it read commits and stamps the
	// next version.
	edited := stored
	edited.BotID = "other-bot"
	if err := st.Update(ctx, edited); err != nil {
		t.Fatalf("the update at its read version = %v, want nil", err)
	}
	got, err := st.Get(ctx, "cas")
	if err != nil {
		t.Fatal(err)
	}
	if got.BotID != "other-bot" || got.Version != 1 {
		t.Fatalf("after the update: bot=%q version=%d, want other-bot/1", got.BotID, got.Version)
	}

	// The stale copy (a read taken before that update) conflicts — it must
	// not overwrite the write that landed after it was taken.
	if err := st.Update(ctx, edited); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatalf("a stale full-document update = %v, want ErrSubscriptionConflict", err)
	}
	if got, _ = st.Get(ctx, "cas"); got.BotID != "other-bot" {
		t.Fatalf("the conflicting update overwrote the row: bot=%q, want other-bot", got.BotID)
	}

	// A fresh read-modify-write passes.
	got.BotID = "third-bot"
	if err := st.Update(ctx, got); err != nil {
		t.Fatalf("the fresh read-modify-write = %v, want nil", err)
	}

	// A missing row stays ErrSubscriptionNotFound.
	got.ID = "absent"
	if err := st.Update(ctx, got); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("an update on a missing row = %v, want ErrSubscriptionNotFound", err)
	}
}

// assertPinSweepConflictsAStaleEditor pins the resurrection witness: an
// editor whose snapshot predates the sweep's clear cannot write the swept
// pin back — the write conflicts and the retry re-reads the swept (cleared)
// row. This is the PUT window of #2644: without the version bump, a read
// before the delete's sweep and a write after it land the dead pin
// permanently, with no sweep ever coming behind the delete's 204.
func assertPinSweepConflictsAStaleEditor(t *testing.T, st SubscriptionStore, ctx context.Context) {
	t.Helper()
	if err := st.Create(ctx, Subscription{
		ID: "resurrect", TenantID: "t1", BotID: "review-pr", Enabled: true,
		KeyOverrides:    map[string]string{"anthropic": "key-dead", "openai": "key-alive"},
		SecretOverrides: map[string]string{"forge_token": "sec-dead"},
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := st.Get(ctx, "resurrect")
	if err != nil {
		t.Fatal(err)
	}
	if stale.KeyOverrides["anthropic"] != "key-dead" {
		t.Fatalf("the seeded pin = %q, want key-dead", stale.KeyOverrides["anthropic"])
	}

	// The delete's sweep half, driven the way the route drives it.
	cleared, err := st.ClearCredentialPins(ctx, "resurrect", "key-dead")
	if err != nil || !cleared {
		t.Fatalf("the sweep = (%v, %v), want (true, nil)", cleared, err)
	}

	// The editor that read before the sweep lands after it: refused.
	if err := st.Update(ctx, stale); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatalf("a pre-sweep editor landed = %v, want ErrSubscriptionConflict — the swept pin resurrects", err)
	}
	got, err := st.Get(ctx, "resurrect")
	if err != nil {
		t.Fatal(err)
	}
	if _, stands := got.KeyOverrides["anthropic"]; stands {
		t.Fatalf("the dead pin stands again: %v — a pin naming a deleted key seals the team's default in silence", got.KeyOverrides)
	}
	if got.KeyOverrides["openai"] != "key-alive" {
		t.Fatalf("the surviving pin moved: %v", got.KeyOverrides)
	}

	// The retry the 409 asks for re-reads the swept row and lands clean.
	fresh, err := st.Get(ctx, "resurrect")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(ctx, fresh); err != nil {
		t.Fatalf("the retry after the conflict = %v, want nil", err)
	}
}

// assertUpdateKeepsTheHealthStamps pins the marks' survival of a
// full-document write. MarkLaunchError and MarkRunOutcome are targeted
// writes that bump no version — a keepalive firing every tick must not
// conflict every operator edit — so a PUT whose read predates a fire passes
// the CAS, and must re-stamp the health fields from the stored row instead
// of writing its stale copy over them. Both directions: a stamp landed
// after the read survives the write, and a stamp CLEARED after the read
// stays cleared.
func assertUpdateKeepsTheHealthStamps(t *testing.T, st SubscriptionStore, ctx context.Context) {
	t.Helper()
	if err := st.Create(ctx, Subscription{ID: "health", TenantID: "t1", BotID: "review-pr", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stale, err := st.Get(ctx, "health")
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	if err := st.MarkLaunchError(ctx, "health", "gate refused", t1); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunOutcome(ctx, "health", "run-1", "failed", "boom", "E_BOOM", t2); err != nil {
		t.Fatal(err)
	}
	stale.BotID = "edited"
	if err := st.Update(ctx, stale); err != nil {
		t.Fatalf("an edit read before the marks = %v, want nil — a health mark must not conflict an operator's edit", err)
	}
	got, err := st.Get(ctx, "health")
	if err != nil {
		t.Fatal(err)
	}
	if got.BotID != "edited" {
		t.Fatalf("the edit did not land: bot=%q", got.BotID)
	}
	if got.LastError != "gate refused" || got.LastErrorAt == nil || !got.LastErrorAt.Equal(t1) {
		t.Fatalf("the launch verdict stamped after the edit's read was erased by the edit: last_error=%q at=%v", got.LastError, got.LastErrorAt)
	}
	if got.LastRunID != "run-1" || got.LastRunStatus != "failed" || got.LastRunError != "boom" || got.LastRunErrorCode != "E_BOOM" || got.LastRunAt == nil || !got.LastRunAt.Equal(t2) {
		t.Fatalf("the run outcome stamped after the edit's read was erased by the edit: id=%q status=%q err=%q code=%q at=%v", got.LastRunID, got.LastRunStatus, got.LastRunError, got.LastRunErrorCode, got.LastRunAt)
	}

	// The other direction: a copy read while the failure stood, written
	// after a success cleared it, must not resurrect the failure.
	stale, err = st.Get(ctx, "health")
	if err != nil {
		t.Fatal(err)
	}
	t3 := t2.Add(time.Minute)
	if err := st.MarkLaunchError(ctx, "health", "", t3); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunOutcome(ctx, "health", "run-2", "succeeded", "", "", t3); err != nil {
		t.Fatal(err)
	}
	stale.Enabled = false
	if err := st.Update(ctx, stale); err != nil {
		t.Fatalf("an edit read before the clearing marks = %v, want nil", err)
	}
	got, err = st.Get(ctx, "health")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("the edit did not land: still enabled")
	}
	if got.LastError != "" || got.LastErrorAt != nil {
		t.Fatalf("the cleared launch verdict came back with the edit: last_error=%q at=%v", got.LastError, got.LastErrorAt)
	}
	if got.LastRunID != "run-2" || got.LastRunStatus != "succeeded" || got.LastRunError != "" || got.LastRunErrorCode != "" || got.LastRunAt == nil || !got.LastRunAt.Equal(t3) {
		t.Fatalf("the cleared run failure came back with the edit: id=%q status=%q err=%q code=%q at=%v", got.LastRunID, got.LastRunStatus, got.LastRunError, got.LastRunErrorCode, got.LastRunAt)
	}
}

// The version is the store's concurrency stamp, not API surface: it never
// appears in a marshalled subscription, so the routes' answers and the
// studio payloads stay unchanged.
func TestSubscriptionVersionNeverLeavesTheServer(t *testing.T) {
	out, err := json.Marshal(Subscription{
		ID: "v", TenantID: "t1", BotID: "review-pr", Version: 7,
		KeyOverrides: map[string]string{"anthropic": "key-dead"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "version") {
		t.Fatalf("the subscription payload leaks the version: %s", out)
	}
}

func TestSubscriptionUpdateIsAVersionCAS(t *testing.T) {
	ctx := context.Background()
	assertUpdateIsAVersionCAS(t, NewMemorySubscriptionStore(), ctx)
	assertPinSweepConflictsAStaleEditor(t, NewMemorySubscriptionStore(), ctx)
	assertUpdateKeepsTheHealthStamps(t, NewMemorySubscriptionStore(), ctx)
}

func TestMongoSubscriptionStoreUpdateIsAVersionCAS(t *testing.T) {
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
	db := client.Database("iterion_pin_cas_" + bson.NewObjectID().Hex())
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
	assertUpdateIsAVersionCAS(t, st, ctx)
	assertPinSweepConflictsAStaleEditor(t, st, ctx)
	assertUpdateKeepsTheHealthStamps(t, st, ctx)

	// A row written before the version field existed (a deployed document
	// from an earlier release) stores no version: its first versioned write
	// must still land, matching the absence. The memory twin cannot prove
	// this — its rows always carry the field.
	coll := db.Collection(SubscriptionsCollectionName)
	if _, err := coll.InsertOne(ctx, bson.M{
		"_id": "pre-version", "tenant_id": "t1", "bot_id": "review-pr", "enabled": true,
		"key_overrides": bson.M{"anthropic": "key-dead"},
	}); err != nil {
		t.Fatal(err)
	}
	pre, err := st.Get(ctx, "pre-version")
	if err != nil {
		t.Fatal(err)
	}
	if pre.Version != 0 {
		t.Fatalf("a document without a version field reads version %d, want 0", pre.Version)
	}
	pre.BotID = "edited"
	if err := st.Update(ctx, pre); err != nil {
		t.Fatalf("the first write over a version-less document = %v, want nil", err)
	}
	if got, _ := st.Get(ctx, "pre-version"); got.BotID != "edited" || got.Version != 1 {
		t.Fatalf("after the first versioned write: bot=%q version=%d, want edited/1", got.BotID, got.Version)
	}
}
