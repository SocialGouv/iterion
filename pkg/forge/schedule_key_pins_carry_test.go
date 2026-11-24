package forge

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
)

// TestSyncSchedules_CarriesTheRowsKeyPins: a schedule row's key pins are the
// schedule PATCH's voice, and syncSchedules rebuilds every row from a
// literal — a pin the carry list omits is erased by the next `bots enable`,
// and the row's runs then spend whichever key the walk orders first, the
// silent replacement the pin exists to prevent. The integration's own pins
// are read live at the tick and never projected, so a row that pins nothing
// comes out of the rebuild pinning nothing.
func TestSyncSchedules_CarriesTheRowsKeyPins(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	now := time.Unix(1700000000, 0).UTC()
	o := &Orchestrator{
		Schedules: mem,
		Now:       func() time.Time { return now },
		NewID:     func() string { return "s1" },
	}
	inv := map[string][]bundle.Invocation{
		"feed-watch": {{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 6 * * *"}}},
	}
	ctx := context.Background()
	const repoURL = "https://github.com/acme/veille.git"
	if err := o.syncSchedules(ctx, "t1", "ri-1", repoURL, inv, nil, "u1", nil); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	rows, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows) != 1 || rows[0].KeyOverrides != nil {
		t.Fatalf("first sync: want 1 row pinning nothing, got %+v", rows)
	}

	pins := map[string]string{"openai_compatible": "key-design"}
	if _, err := mem.Update(ctx, rows[0].ID, cloudsched.SchedulePatch{KeyOverrides: &pins, UpdatedAt: now.Add(time.Minute)}); err != nil {
		t.Fatalf("pin the row: %v", err)
	}
	if err := o.syncSchedules(ctx, "t1", "ri-1", repoURL, inv, nil, "u1", nil); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	rows, _ = mem.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows) != 1 {
		t.Fatalf("re-sync: want 1 row, got %d", len(rows))
	}
	if !maps.Equal(rows[0].KeyOverrides, pins) {
		t.Errorf("the row's key pins after a rebuild = %v, want %v carried as they stood", rows[0].KeyOverrides, pins)
	}
}
