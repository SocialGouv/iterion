package cloudsched

import (
	"context"
	"maps"
	"testing"
	"time"
)

func TestMemoryStore_KeyOverridesContract(t *testing.T) {
	assertKeyOverridesContract(t, NewMemoryStore())
}

// assertKeyOverridesContract is the key-pin conformance both store twins
// run (TestMongoStore_CAS calls it on Mongo): the pins round-trip through
// Create and Get; a patch that leaves the field nil keeps them; a patch
// carrying a map replaces them wholesale; an empty map clears them to no
// map at all; and the slot intent the ticker launches from carries them,
// because the tick launches the intent's snapshot, never the live row.
func assertKeyOverridesContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	pins := map[string]string{"openai_compatible": "key-design", "anthropic": "key-a"}
	sb := ScheduledBot{
		ID: "key-pins-contract", TenantID: "t-pins", BotID: "feed-watch",
		Cron: "0 9 * * *", NextFireAt: now, KeyOverrides: pins,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Create(ctx, sb); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := st.Get(ctx, sb.ID)
	if err != nil || !maps.Equal(got.KeyOverrides, pins) {
		t.Fatalf("after Create: key_overrides = %v (%v), want %v", got.KeyOverrides, err, pins)
	}

	disabled := true
	got, err = st.Update(ctx, sb.ID, SchedulePatch{Disabled: &disabled, UpdatedAt: now.Add(time.Minute)})
	if err != nil || !maps.Equal(got.KeyOverrides, pins) {
		t.Fatalf("a patch silent on the pins changed them: %v (%v), want %v", got.KeyOverrides, err, pins)
	}

	replaced := map[string]string{"openai_compatible": "key-tech"}
	got, err = st.Update(ctx, sb.ID, SchedulePatch{KeyOverrides: &replaced, UpdatedAt: now.Add(2 * time.Minute)})
	if err != nil || !maps.Equal(got.KeyOverrides, replaced) {
		t.Fatalf("a patch carrying pins did not replace them wholesale: %v (%v), want %v", got.KeyOverrides, err, replaced)
	}
	if got, err = st.Get(ctx, sb.ID); err != nil || !maps.Equal(got.KeyOverrides, replaced) {
		t.Fatalf("the replaced pins were not stored: %v (%v), want %v", got.KeyOverrides, err, replaced)
	}

	// The slot intent snapshots the row: the pins ride it to the launch.
	sb.KeyOverrides = replaced
	if err := st.PutIntent(ctx, sb); err != nil {
		t.Fatalf("PutIntent: %v", err)
	}
	intents, err := st.ClaimScheduleIntents(ctx, sb.ID, now, 1)
	if err != nil || len(intents) != 1 {
		t.Fatalf("ClaimScheduleIntents: %v (%v), want the one slot", intents, err)
	}
	if !maps.Equal(intents[0].Schedule.KeyOverrides, replaced) {
		t.Errorf("the slot intent the tick launches lost the pins: %v, want %v", intents[0].Schedule.KeyOverrides, replaced)
	}

	empty := map[string]string{}
	got, err = st.Update(ctx, sb.ID, SchedulePatch{KeyOverrides: &empty, UpdatedAt: now.Add(3 * time.Minute)})
	if err != nil || got.KeyOverrides != nil {
		t.Fatalf("an empty map did not clear the pins: %v (%v), want no map", got.KeyOverrides, err)
	}
	if got, err = st.Get(ctx, sb.ID); err != nil || got.KeyOverrides != nil {
		t.Fatalf("the clear was not stored: %v (%v), want no map", got.KeyOverrides, err)
	}
	if err := st.Delete(ctx, sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
