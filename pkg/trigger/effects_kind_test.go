package trigger

import (
	"context"
	"testing"
	"time"
)

// TestEffectKind_AbsentReadsAsLaunch is the expand half of the expand/contract
// rollout: every row written before the discriminator existed — and every row
// a replica that predates it still writes — carries no Kind, and MUST read as
// a launch. A row that read as anything else would be executed by the wrong
// arm the moment the new binary claimed it.
func TestEffectKind_AbsentReadsAsLaunch(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  EffectRow
		want string
	}{
		{"absent (a row from before the field)", EffectRow{}, EffectKindLaunch},
		{"explicit launch", EffectRow{Kind: EffectKindLaunch}, EffectKindLaunch},
		{"projection", EffectRow{Kind: EffectKindProjection}, EffectKindProjection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.EffectKind(); got != tc.want {
				t.Fatalf("EffectKind() = %q, want %q", got, tc.want)
			}
			if got, want := tc.row.IsProjection(), tc.want == EffectKindProjection; got != want {
				t.Fatalf("IsProjection() = %v, want %v", got, want)
			}
		})
	}
}

// TestMemoryEffectOutbox_KindSurvivesAClaim guards the memory twin: the kind is
// what the worker dispatches on, so a store that dropped it on the round trip
// would silently run a projection through the launch arm.
func TestMemoryEffectOutbox_KindSurvivesAClaim(t *testing.T) {
	ctx := context.Background()
	ob := NewMemoryEffectOutbox()
	now := time.Now().UTC()
	rows := []EffectRow{
		{ID: "e|sub1", TenantID: "t", SubID: "sub1", CreatedAt: now, UpdatedAt: now},
		{ID: ProjectionEffectID("e"), TenantID: "t", Kind: EffectKindProjection, CreatedAt: now, UpdatedAt: now},
	}
	if err := ob.UpsertPending(ctx, rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A row stored with no kind is NORMALIZED to launch by the store, the way
	// State already is — so what sits in the store is explicit, and only rows
	// from an older binary are ever blank.
	if r, ok := ob.Row("e|sub1"); !ok || r.Kind != EffectKindLaunch {
		t.Fatalf("stored launch row kind = %q (found=%v), want %q", r.Kind, ok, EffectKindLaunch)
	}
	claimed, err := ob.ClaimDue(ctx, now, 10)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim: n=%d err=%v", len(claimed), err)
	}
	kinds := map[string]string{}
	for _, r := range claimed {
		kinds[r.ID] = r.EffectKind()
	}
	if kinds["e|sub1"] != EffectKindLaunch {
		t.Fatalf("claimed launch row kind = %q, want %q", kinds["e|sub1"], EffectKindLaunch)
	}
	if kinds[ProjectionEffectID("e")] != EffectKindProjection {
		t.Fatalf("claimed projection row kind = %q, want %q", kinds[ProjectionEffectID("e")], EffectKindProjection)
	}
}

// TestProjectionEffectID_CarriesNoSubscriptionID: a projection row is owed to
// the tenant's board binding, never to a subscription. Its key must not
// collide with any (event, subscription) pair, and the row must carry an EMPTY
// SubID — which is what makes it structurally unlistable and undeletable
// through /api/v1/triggers (a subscription CRUD keyed on a subscription id).
func TestProjectionEffectID_CarriesNoSubscriptionID(t *testing.T) {
	const eventID = "board:b:card:7"
	proj := ProjectionEffectID(eventID)
	if proj == EffectID(eventID, "") {
		t.Fatal("the projection row key equals the key of a launch row with an empty sub id — a blank sub id must not address a projection")
	}
	for _, sub := range []string{"sub1", "@projection", "projection"} {
		if got := EffectID(eventID, sub); got == proj {
			t.Fatalf("subscription %q produces the projection row key %q — a subscription could collide with the projection row", sub, got)
		}
	}
}
