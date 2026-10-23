package trigger

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

type round4ScheduleLauncher struct {
	st       *store.FilesystemRunStore
	hold     bool
	launches int
}

func (l *round4ScheduleLauncher) Launch(ctx context.Context, p LaunchPlan) (string, error) {
	if l.hold {
		return "", ErrMaintenanceHold
	}
	if strings.TrimSpace(p.Vars["input"]) != "original" {
		return "", fmt.Errorf("lost guard payload: %v", p.Vars)
	}
	l.launches++
	r, err := l.st.CreateRun(ctx, fmt.Sprintf("run-%d", l.launches), p.BotID, nil)
	if err != nil {
		return "", err
	}
	r.Status, r.Source = store.RunStatusRunning, p.SourceRef
	if err := l.st.SaveRun(ctx, r); err != nil {
		return "", err
	}
	return r.ID, nil
}
func TestRound4HeldSlotsRecheckOverlap(t *testing.T) {
	for _, overlap := range []string{"skip", "keepalive"} {
		t.Run(overlap, func(t *testing.T) {
			scrubHome(t)
			ctx := context.Background()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "guard")
			sub := minuteSub("held")
			sub.Overlap, sub.Guard, sub.GuardVar = overlap, "echo consumed >> '"+marker+"'; echo original", "input"
			subs := seed(t, sub)
			now := time.Now().UTC().Truncate(time.Minute)
			l := &round4ScheduleLauncher{st: st, hold: true}
			path := filepath.Join(t.TempDir(), "intents.json")
			var skips int
			newScheduler := func() *Scheduler {
				return NewScheduler(subs, l, WithSchedulerClock(func() time.Time { return now }), WithSchedulerStateFile(path), WithSchedulerGate(&ScheduleGate{Lister: st, Audit: func(r schedgate.TickRecord) {
					if r.Decision == schedgate.TickSkippedOverlap {
						skips++
					}
				}}))
			}
			s := newScheduler()
			s.tick(ctx, now)
			for i := 0; i < 3; i++ {
				now = now.Add(time.Minute)
				s.tick(ctx, now)
			}
			if len(s.pending) != 3 || l.launches != 0 {
				t.Fatalf("held=%d launches=%d", len(s.pending), l.launches)
			}
			s = newScheduler()
			l.hold = false
			now = now.Add(20 * time.Second)
			s.tick(ctx, now)
			s.tick(ctx, now)
			if l.launches != 1 || skips != 2 {
				t.Errorf("after disarm: active launches=%d overlap skips=%d, want 1/2", l.launches, skips)
			}
			if live := schedgate.LiveRunsForSchedule(ctx, st, sub.ID, nil); len(live) != 1 {
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
