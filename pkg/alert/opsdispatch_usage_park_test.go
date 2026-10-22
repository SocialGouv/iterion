package alert_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// countingRuns counts the per-run store loads the ops sweep spends.
type countingRuns struct {
	store.RunStore
	mu    sync.Mutex
	loads map[string]int
}

func (c *countingRuns) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	c.mu.Lock()
	c.loads[id]++
	c.mu.Unlock()
	return c.RunStore.LoadRun(ctx, id)
}

func (c *countingRuns) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads[id]
}

// The ops sweep's cheap pre-check and the transition marker Handle stamps are
// ONE key, derived in two places: from the run document on the bus path, from
// the LISTING on the sweep's. A usage park keys that marker on the provider
// wall, so the sweep has to derive the wall-keyed id too — which is why the
// wall travels in usernotify.RunRef (cmd/iterion/server.go fills it) and why
// RunRef.Episode() is the one derivation both consumers of that listing use.
//
// Deriving the per-transition key here instead looked for a marker nothing
// ever writes: the pre-check missed for the row's whole life and every parked
// run paid a full classification — two run loads — on every 2-minute pass for
// the 24h lookback, in exactly the provider-wide incident this component
// exists for.
//
// The control is the same row with a non-usage reason: its episode still keys
// on the transition, the stamp matches, and the pre-check skips it unloaded.
func TestOpsSweep_AUsageParkCostsNoRunLoadOnceItsEpisodeIsSettled(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingRuns{RunStore: rs, loads: map[string]int{}}
	sink := &captureSink{}
	d := &alert.OpsDispatcher{
		Runs:    counting,
		Claims:  usernotify.NewMemSentStore(),
		Sinks:   []alert.Sink{sink},
		BaseURL: "https://iterion.example.com",
	}

	wall := time.Now().UTC().Add(4 * 24 * time.Hour).Truncate(time.Second)
	park := func(id, reason string, withWall bool) *store.Run {
		run, cerr := rs.CreateRun(context.Background(), id, "feed-watch", nil)
		if cerr != nil {
			t.Fatal(cerr)
		}
		run.Status = store.RunStatusFailedResumable
		run.Name = "cyber digest"
		run.Error = "usage cap: weekly window exhausted"
		after := wall
		run.RetryState = &store.RunRetryState{
			RetryAfter: &after, Reason: reason, Code: "USAGE_LIMIT_BLOCKED", Attempts: 1,
		}
		if withWall {
			w := wall
			run.RetryState.Wall = &w
		}
		if serr := rs.SaveRun(context.Background(), run); serr != nil {
			t.Fatal(serr)
		}
		return run
	}

	parked := park("run-wall-park", store.RetryReasonUsageWindow, true)
	control := park("run-other-park", "sandbox_fail", false)

	ctx := context.Background()
	for _, id := range []string{parked.ID, control.ID} {
		ev := trigger.BuildRunOutcome(ctx, counting, id, nil)
		if err := d.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %s: %v", id, err)
		}
	}
	if n := len(sink.alerts()); n != 2 {
		t.Fatalf("delivered %d alerts, want 2 (one per parked run)", n)
	}

	// Both episodes are now DELIVERED and their transition markers stamped.
	// A sweep pass offering the same two rows must load neither.
	counting.mu.Lock()
	counting.loads = map[string]int{}
	counting.mu.Unlock()

	refOf := func(r *store.Run) usernotify.RunRef {
		return usernotify.RunRef{
			ID: r.ID, Status: string(r.Status), UpdatedAt: r.UpdatedAt,
			// Exactly what cmd/iterion/server.go now puts in the listing.
			UsageWall: store.UsageParkWall(r.Status, r.RetryState),
		}
	}
	reload := func(id string) *store.Run {
		r, lerr := rs.LoadRun(ctx, id)
		if lerr != nil || r == nil {
			t.Fatalf("reload %s: %v", id, lerr)
		}
		return r
	}
	list := func(_ context.Context, _, _ time.Time, _ int) ([]usernotify.RunRef, error) {
		return []usernotify.RunRef{refOf(reload(parked.ID)), refOf(reload(control.ID))}, nil
	}
	counting.mu.Lock()
	counting.loads = map[string]int{}
	counting.mu.Unlock()

	d.SweepOnce(ctx, list)

	if got := counting.count(control.ID); got != 0 {
		t.Errorf("the non-usage park cost %d run loads on a settled episode, want 0", got)
	}
	if got := counting.count(parked.ID); got != 0 {
		t.Fatalf("the usage park cost %d run loads on a settled episode, want 0: the sweep's pre-check and the marker Handle stamps must be the same key, or every parked run is re-classified on every pass for the whole lookback",
			got)
	}
	if n := len(sink.alerts()); n != 2 {
		t.Errorf("the sweep delivered %d alerts in total, want 2", n)
	}
}
