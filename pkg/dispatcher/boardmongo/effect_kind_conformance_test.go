package boardmongo_test

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// runEffectOutboxKindSuite pins EffectRow.Kind on BOTH outbox twins.
//
// The kind is what the EffectWorker dispatches on: a store that dropped it, or
// normalized it to the wrong value, would silently run a projection through the
// launch arm — which loads a subscription that does not exist, retires the row,
// and leaves the reflect to the periodic pass with nobody the wiser.
func runEffectOutboxKindSuite(t *testing.T, ob trigger.EffectOutbox, tenant string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	const eventID = "board:b:card:42"
	launchID := trigger.EffectID(eventID, "sub-kind")
	projID := trigger.ProjectionEffectID(eventID)

	rows := []trigger.EffectRow{
		{
			// No Kind: what a caller (or a replica from before the field)
			// writes for a launch. It must come back as a launch.
			ID: launchID, TenantID: tenant, SubID: "sub-kind",
			Event:     trigger.Event{ID: eventID, Source: trigger.SourceBoard, Kind: trigger.KindCardMoved},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: projID, TenantID: tenant, Kind: trigger.EffectKindProjection,
			Event:     trigger.Event{ID: eventID, Source: trigger.SourceBoard, Kind: trigger.KindCardMoved},
			CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := ob.UpsertPending(ctx, rows); err != nil {
		t.Fatalf("upsert effect rows: %v", err)
	}
	claimed, err := ob.ClaimDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("claim due: %v", err)
	}
	got := map[string]trigger.EffectRow{}
	for _, r := range claimed {
		got[r.ID] = r
	}
	if len(got) != 2 {
		t.Fatalf("claimed %d distinct rows, want 2 (%v)", len(got), got)
	}
	if k := got[launchID].EffectKind(); k != trigger.EffectKindLaunch {
		t.Fatalf("a row upserted with no kind claimed back as %q, want %q — the launch arm would not run it",
			k, trigger.EffectKindLaunch)
	}
	if got[launchID].SubID != "sub-kind" {
		t.Fatalf("launch row lost its subscription id: %q", got[launchID].SubID)
	}
	proj := got[projID]
	if k := proj.EffectKind(); k != trigger.EffectKindProjection {
		t.Fatalf("projection row claimed back as %q, want %q — it would execute through the LAUNCH arm",
			k, trigger.EffectKindProjection)
	}
	if !proj.IsProjection() {
		t.Fatal("IsProjection() false on a claimed projection row")
	}
	if proj.SubID != "" {
		t.Fatalf("projection row carries sub_id %q — a projection is owed to the board binding, never to a subscription", proj.SubID)
	}
	// The two kinds are separate rows for the same event: a projection must
	// not collapse onto a launch row's key (or vice versa) on the upsert.
	if launchID == projID {
		t.Fatal("the launch and projection rows of one event share a key")
	}
	for _, r := range []trigger.EffectRow{got[launchID], proj} {
		if err := ob.MarkDone(ctx, r.ID, r.ClaimID); err != nil {
			t.Fatalf("mark done %s: %v", r.ID, err)
		}
	}
}

// runLegacyEffectRowSuite is the expand half of the rollout, measured against
// the shape it actually has to survive: a document already IN the collection,
// written by a replica that predates the discriminator, carrying no `kind`
// field at all. A raw insert is the only way to produce one — UpsertPending
// normalizes — and it is exactly what a rolling deploy leaves behind.
func runLegacyEffectRowSuite(ctx context.Context, t *testing.T, db *mongo.Database) {
	t.Helper()
	const tenant = "legacy-kind-tenant"
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.Collection(boardmongo.EffectsCollection).InsertOne(ctx, bson.M{
		"_id": "board:b:card:legacy|sub-legacy", "tenant_id": tenant, "sub_id": "sub-legacy",
		"state": trigger.EffectPending, "attempts": 0, "consume_marked": false,
		"not_before": time.Time{}, "created_at": now, "updated_at": now,
		"event": bson.M{"_id": "board:b:card:legacy", "source": string(trigger.SourceBoard)},
	}); err != nil {
		t.Fatalf("insert a pre-discriminator effect row: %v", err)
	}
	claimed, err := boardmongo.New(db, tenant).ClaimDue(ctx, now, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim the legacy row: n=%d err=%v", len(claimed), err)
	}
	if k := claimed[0].EffectKind(); k != trigger.EffectKindLaunch {
		t.Fatalf("a row with no stored `kind` reads as %q, want %q — the mixed-fleet window would strand every in-flight launch",
			k, trigger.EffectKindLaunch)
	}
	if claimed[0].IsProjection() {
		t.Fatal("a row with no stored `kind` reads as a projection — it would reach the reflect arm with no binding behind it")
	}
}
