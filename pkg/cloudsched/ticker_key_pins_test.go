package cloudsched

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// TestTicker_ASlotLaunchesTheRowsCurrentKeyPins: a slot intent snapshots the
// row when its slot is materialised, and a slot held by maintenance is
// launched again later from that snapshot. The key pins are read live
// everywhere else, so the launch takes the row's CURRENT pins, the way it
// takes the row's current pause: a repin or a clear made while the slot was
// held reaches the replayed launch instead of spending the old key.
func TestTicker_ASlotLaunchesTheRowsCurrentKeyPins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after map[string]string
	}{
		{"a repin", map[string]string{"openai_compatible": "key-design"}},
		{"a clear", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mem := NewMemoryStore()
			now := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
			sb := ScheduledBot{ID: "s1", TenantID: "t1", BotID: "feed-watch", Cron: "0 6 * * *", NextFireAt: now,
				KeyOverrides: map[string]string{"openai_compatible": "key-tech"}}
			if err := mem.Create(ctx, sb); err != nil {
				t.Fatal(err)
			}
			var launched []map[string]string
			hold := true
			tk := &Ticker{Store: mem, Now: func() time.Time { return now },
				Launch: func(_ context.Context, sb ScheduledBot) error {
					launched = append(launched, sb.KeyOverrides)
					if hold {
						return schedgate.ErrMaintenanceHold
					}
					return nil
				}}
			if _, err := tk.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			after := tc.after
			if _, err := mem.Update(ctx, "s1", SchedulePatch{KeyOverrides: &after, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			hold = false
			now = now.Add(20 * time.Second) // past the intent's retry, before the next cron slot
			if _, err := tk.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if len(launched) != 2 {
				t.Fatalf("launch attempts = %d (%v), want the held one and its replay", len(launched), launched)
			}
			if !maps.Equal(launched[1], tc.after) {
				t.Errorf("the replayed slot launched with pins %v, want the row's current %v", launched[1], tc.after)
			}
		})
	}
}
