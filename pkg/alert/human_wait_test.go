package alert

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestStallReadsPersistedHumanWaitAndRearms(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "child", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, "child", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	m := NewManager(WithStallTimeout(time.Minute), WithHumanWaitLookup(func(ctx context.Context, _ string, _ time.Time) bool {
		child, err := s.LoadRun(ctx, "child")
		return err == nil && child.Status.IsPaused()
	}))
	defer m.Stop()
	m.Observe(store.Event{RunID: "parent", Type: store.EventNodeStarted, NodeID: "subbot", Timestamp: base})
	if got := m.checkStalls(base.Add(10 * time.Minute)); len(got) != 0 {
		t.Fatalf("human wait raised a false stall: %+v", got)
	}
	// No parent event is emitted when an external actor resumes the child.
	// The next observation must pull that new state and re-arm the watchdog.
	if err := s.UpdateRunStatus(ctx, "child", store.RunStatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	if got := m.checkStalls(base.Add(11 * time.Minute)); len(got) != 1 || got[0].Kind != KindStall {
		t.Fatalf("genuine silence after the wait was muted: %+v", got)
	}
}

func TestHumanWaitLookupDoesNotBlockObserveOrAlertFromAStaleView(t *testing.T) {
	base := time.Now()
	entered := make(chan struct{})
	release := make(chan struct{})
	m := NewManager(WithStallTimeout(time.Minute), WithHumanWaitLookup(func(ctx context.Context, _ string, _ time.Time) bool {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return false
	}))
	defer m.Stop()
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, Timestamp: base})
	done := make(chan []Alert, 1)
	go func() { done <- m.checkStalls(base.Add(10 * time.Minute)) }()
	<-entered
	// Observe must remain available while the persisted lookup is blocked.
	observed := make(chan struct{})
	go func() {
		m.Observe(store.Event{RunID: "r", Type: store.EventToolCalled, Timestamp: base.Add(10 * time.Minute)})
		close(observed)
	}()
	select {
	case <-observed:
	case <-time.After(time.Second):
		m.Stop()
		t.Fatal("lookup held the observer mutex")
	}
	close(release)
	if got := <-done; len(got) != 0 {
		t.Fatalf("lookup notified from a stale view: %+v", got)
	}
}

func TestStopCancelsHumanWaitLookup(t *testing.T) {
	entered := make(chan struct{})
	m := NewManager(WithStallTimeout(time.Minute), WithHumanWaitLookup(func(ctx context.Context, _ string, _ time.Time) bool {
		close(entered)
		<-ctx.Done()
		return false
	}))
	base := time.Now()
	m.Observe(store.Event{RunID: "r", Type: store.EventNodeStarted, Timestamp: base})
	done := make(chan []Alert, 1)
	go func() { done <- m.checkStalls(base.Add(10 * time.Minute)) }()
	<-entered
	m.Stop()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("Stop produced a late alert: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the persisted lookup")
	}
}
