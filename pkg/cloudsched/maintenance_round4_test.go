package cloudsched

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRound4HeldSlotsRecheckOverlap(t *testing.T) {
	for _, overlap := range []string{"skip", "keepalive"} {
		t.Run(overlap, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ctx := context.Background()
			runs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			st := NewMemoryStore()
			now := time.Now().UTC().Truncate(time.Minute)
			marker := filepath.Join(t.TempDir(), "guard")
			sb := ScheduledBot{ID: "held", BotID: "bot", Cron: "* * * * *", NextFireAt: now, Overlap: overlap, Guard: "echo consumed >> '" + marker + "'; echo original", GuardVar: "input"}
			if err := st.Create(ctx, sb); err != nil {
				t.Fatal(err)
			}
			hold := true
			launches, skips := 0, 0
			ticker := &Ticker{Store: st, Now: func() time.Time { return now }, Gate: func(ctx context.Context, sb ScheduledBot) (bool, string, schedgate.TickRecord) {
				out := schedgate.Apply(ctx, schedgate.GateInput{Policy: sb.Policy(), Lister: runs, ScheduleID: sb.ID, Now: now})
				return out.Proceed, out.GuardStdout, out.Record
			}, Audit: func(r schedgate.TickRecord) {
				if r.Decision == schedgate.TickSkippedOverlap {
					skips++
				}
			}, Launch: func(ctx context.Context, sb ScheduledBot) error {
				if hold {
					return schedgate.ErrMaintenanceHold
				}
				launches++
				if strings.TrimSpace(sb.Vars["input"]) != "original" {
					t.Errorf("lost guard payload: %v", sb.Vars)
				}
				r, err := runs.CreateRun(ctx, fmt.Sprintf("run-%d", launches), sb.BotID, nil)
				if err != nil {
					return err
				}
				r.Status, r.Source = store.RunStatusRunning, &store.RunSource{Kind: store.RunSourceKindSchedule, ScheduleID: sb.ID}
				return runs.SaveRun(ctx, r)
			}}
			for i := 0; i < 3; i++ {
				if _, err := ticker.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if i < 2 {
					now = now.Add(time.Minute)
				}
			}
			prepared := 0
			for _, in := range st.intents {
				if in.Prepared && !in.Done {
					prepared++
				}
			}
			if prepared != 3 || launches != 0 {
				t.Fatalf("prepared=%d launches=%d", prepared, launches)
			}
			restarted := *ticker
			ticker = &restarted
			hold = false
			now = now.Add(20 * time.Second)
			for i := 0; i < 2; i++ {
				if _, err := ticker.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if launches != 1 || skips != 2 {
				t.Errorf("after disarm: active launches=%d overlap skips=%d, want 1/2", launches, skips)
			}
			if live := schedgate.LiveRunsForSchedule(ctx, runs, sb.ID, nil); len(live) != 1 {
				t.Errorf("persisted active runs=%v, want one", live)
			}
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "consumed") != 3 {
				t.Errorf("guard consumed again: %q", data)
			}
		})
	}
}
