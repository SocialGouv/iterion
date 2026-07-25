package schedgate

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type fakeLister struct {
	ids     []string
	listErr error
	runs    map[string]*store.Run
}

func (f *fakeLister) ListRunsBySchedule(_ context.Context, _ string) ([]string, error) {
	return f.ids, f.listErr
}

func (f *fakeLister) LoadRun(_ context.Context, id string) (*store.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return nil, store.ErrRunNotFound
	}
	return r, nil
}

func TestLiveRunsForSchedule(t *testing.T) {
	f := &fakeLister{
		ids: []string{"r1", "r2", "r3", "r4"},
		runs: map[string]*store.Run{
			"r1": {ID: "r1", Status: store.RunStatusRunning},
			"r2": {ID: "r2", Status: store.RunStatusFinished},
			"r3": {ID: "r3", Status: store.RunStatusPausedWaitingHuman},
			// r4 missing on disk: must be skipped, not fatal.
		},
	}
	got := LiveRunsForSchedule(context.Background(), f, "weekly", nil)
	want := []string{"r1", "r3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("live = %v, want %v", got, want)
	}
}

func TestLiveRunsForSchedule_ListErrorIsNonBlocking(t *testing.T) {
	f := &fakeLister{listErr: errors.New("boom")}
	if got := LiveRunsForSchedule(context.Background(), f, "weekly", nil); got != nil {
		t.Fatalf("live = %v, want nil on list error", got)
	}
}

func TestLiveRunsForSchedule_EmptyInputs(t *testing.T) {
	if got := LiveRunsForSchedule(context.Background(), nil, "weekly", nil); got != nil {
		t.Fatalf("nil lister: got %v", got)
	}
	if got := LiveRunsForSchedule(context.Background(), &fakeLister{}, "", nil); got != nil {
		t.Fatalf("empty schedule id: got %v", got)
	}
}

func TestLiveAndStaleRunsForSchedule_Staleness(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	staleAfter := 5 * time.Minute
	f := &fakeLister{
		ids: []string{"fresh", "stale", "pausedOld", "done"},
		runs: map[string]*store.Run{
			// running, updated 1m ago → alive.
			"fresh": {ID: "fresh", Status: store.RunStatusRunning, UpdatedAt: now.Add(-1 * time.Minute)},
			// running, updated 10m ago → stale (reapable).
			"stale": {ID: "stale", Status: store.RunStatusRunning, UpdatedAt: now.Add(-10 * time.Minute)},
			// paused and old → NEVER stale (legitimately idle).
			"pausedOld": {ID: "pausedOld", Status: store.RunStatusPausedWaitingHuman, UpdatedAt: now.Add(-1 * time.Hour)},
			"done":      {ID: "done", Status: store.RunStatusFinished, UpdatedAt: now.Add(-30 * time.Minute)},
		},
	}
	live, stale := LiveAndStaleRunsForSchedule(context.Background(), f, "keep", staleAfter, now, nil)
	if want := []string{"fresh", "pausedOld"}; !reflect.DeepEqual(live, want) {
		t.Fatalf("live = %v, want %v", live, want)
	}
	if want := []string{"stale"}; !reflect.DeepEqual(stale, want) {
		t.Fatalf("stale = %v, want %v", stale, want)
	}
}

func TestLiveAndStaleRunsForSchedule_ZeroStaleAfterNeverStale(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	f := &fakeLister{
		ids: []string{"old"},
		runs: map[string]*store.Run{
			"old": {ID: "old", Status: store.RunStatusRunning, UpdatedAt: now.Add(-1 * time.Hour)},
		},
	}
	// staleAfter == 0 is the non-keepalive path: no run is ever stale.
	live, stale := LiveAndStaleRunsForSchedule(context.Background(), f, "s", 0, now, nil)
	if !reflect.DeepEqual(live, []string{"old"}) || stale != nil {
		t.Fatalf("live=%v stale=%v, want [old] and nil", live, stale)
	}
}
