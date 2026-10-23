package trigger

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Two long-lived coordinators must reload the journal under the same file
// exclusion. Caching it once resurrects already discharged intentions.
func TestRound5SchedulersReloadSharedJournal(t *testing.T) {
	ctx := context.Background()
	subs := seed(t, minuteSub("held"))
	l := &heldLauncher{hold: true}
	path := filepath.Join(t.TempDir(), "schedule.json")
	now := time.Now().UTC().Truncate(time.Minute)
	a := NewScheduler(subs, l, WithSchedulerStateFile(path))
	b := NewScheduler(subs, l, WithSchedulerStateFile(path))
	a.tick(ctx, now)
	now = now.Add(time.Minute)
	a.tick(ctx, now)
	b.tick(ctx, now)
	l.hold = false
	a.tick(ctx, now)
	b.tick(ctx, now)
	if len(l.plans) != 1 {
		t.Fatalf("shared journal launched %d times, want once", len(l.plans))
	}
}

type round5LaunchFunc func(context.Context, LaunchPlan) (string, error)

func (f round5LaunchFunc) Launch(ctx context.Context, p LaunchPlan) (string, error) { return f(ctx, p) }

func TestRound5LocalExclusionCoversOverlapAndPersistence(t *testing.T) {
	ctx := context.Background()
	subs := seed(t, minuteSub("held"))
	runs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var hold atomic.Bool
	hold.Store(true)
	var launches atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	l := round5LaunchFunc(func(ctx context.Context, p LaunchPlan) (string, error) {
		if hold.Load() {
			return "", ErrMaintenanceHold
		}
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
		id := fmt.Sprintf("run-%d", launches.Load()+1)
		r, err := runs.CreateRun(ctx, id, p.BotID, nil)
		if err != nil {
			return "", err
		}
		r.Status, r.Source = store.RunStatusRunning, p.SourceRef
		if err := runs.SaveRun(ctx, r); err != nil {
			return "", err
		}
		launches.Add(1)
		return id, nil
	})
	path := filepath.Join(t.TempDir(), "schedule.json")
	newScheduler := func() *Scheduler {
		return NewScheduler(subs, l, WithSchedulerStateFile(path), WithSchedulerGate(&ScheduleGate{Lister: runs}))
	}
	a, b := newScheduler(), newScheduler()
	now := time.Now().UTC().Truncate(time.Minute)
	a.tick(ctx, now)
	now = now.Add(time.Minute)
	a.tick(ctx, now)
	hold.Store(false)
	done := make(chan struct{})
	go func() { defer close(done); a.tick(ctx, now) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first scheduler did not reach persistence")
	}
	// The second process can only wait for the file lock, not pass its own
	// overlap read before the first process has persisted the admitted run.
	blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	b.tick(blocked, now)
	cancel()
	before := launches.Load()
	close(release)
	<-done
	b.tick(ctx, now)
	if before != 0 || launches.Load() != 1 {
		t.Fatalf("overlap/persistence interleaving launched %d before release, %d total; want 0/1", before, launches.Load())
	}
}
