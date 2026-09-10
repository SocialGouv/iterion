package alert

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestStoreSink_StallEpisodeBothEdges: the persisted-twin callback gets
// the stall AND the recovered alert, once each.
func TestStoreSink_StallEpisodeBothEdges(t *testing.T) {
	var persisted []Alert
	base := time.Now()
	clock := base
	m := NewManager(
		WithStallTimeout(5*time.Minute),
		WithStoreSink(func(a Alert) { persisted = append(persisted, a) }),
	)
	m.now = func() time.Time { return clock }

	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: base})

	clock = base.Add(6 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 1 {
		t.Fatalf("want stall, got %+v", fired)
	}
	if len(persisted) != 1 || persisted[0].Kind != KindStall {
		t.Fatalf("persisted = %+v, want one stall", persisted)
	}

	// Activity resumes → the recovered edge closes the episode.
	m.Observe(store.Event{RunID: "r1", Type: store.EventToolCalled, NodeID: "agent", Timestamp: base.Add(7 * time.Minute)})
	if len(persisted) != 2 || persisted[1].Kind != KindStallRecovered {
		t.Fatalf("persisted = %+v, want stall then stall_recovered", persisted)
	}

	// No spurious recovered on further activity (episode already closed).
	m.Observe(store.Event{RunID: "r1", Type: store.EventToolCalled, NodeID: "agent", Timestamp: base.Add(8 * time.Minute)})
	if len(persisted) != 2 {
		t.Fatalf("persisted grew on plain activity: %+v", persisted)
	}
}

// TestObserve_RunHealthReplayIsInert: replaying the persisted twin
// through Observe (the file tail WILL do this) advances nothing — no
// heartbeat, no episode reset, no new alerts.
func TestObserve_RunHealthReplayIsInert(t *testing.T) {
	var persisted []Alert
	base := time.Now()
	clock := base
	m := NewManager(
		WithStallTimeout(5*time.Minute),
		WithStoreSink(func(a Alert) { persisted = append(persisted, a) }),
	)
	m.now = func() time.Time { return clock }

	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: base})
	clock = base.Add(6 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 1 {
		t.Fatalf("want stall, got %+v", fired)
	}

	// The persisted run_health event comes back through the tail: it
	// must NOT re-arm the episode (else the next sweep would re-fire) —
	// and it must not emit a phantom "recovered".
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunHealth, NodeID: "agent", Timestamp: clock})
	if len(persisted) != 1 {
		t.Fatalf("run_health replay produced alerts: %+v", persisted)
	}
	clock = clock.Add(6 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("run_health replay re-armed the stall episode: %+v", fired)
	}
}

// TestObserve_WorkspaceCheckpointIsNotProgress: the runner's own timer is
// not the run working. A run stuck on a frozen key or a hung tool keeps a
// dirty workspace and keeps being checkpointed — and a failing push repeats
// its event forever — so counting either as progress would let a safety net
// blind the alarm it was laid beside.
func TestObserve_WorkspaceCheckpointIsNotProgress(t *testing.T) {
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5 * time.Minute))
	m.now = func() time.Time { return clock }

	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: base})
	// Two ticks of the checkpoint loop while the run makes no progress.
	clock = base.Add(2 * time.Minute)
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunWorkspaceCheckpoint, Timestamp: clock})
	clock = base.Add(4 * time.Minute)
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunWorkspaceCheckpoint, Timestamp: clock})

	clock = base.Add(6 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 1 {
		t.Fatalf("the checkpoint ticks kept a stalled run reading as alive: %+v", fired)
	}
	// And a REAL event still re-arms: the exclusion is about the timer,
	// not about silencing the run.
	clock = base.Add(7 * time.Minute)
	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: clock})
	clock = base.Add(8 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("a real event did not re-arm the run: %+v", fired)
	}
}

// TestStoreSink_PanicContained: a panicking store sink must never take
// down the dispatch path.
func TestStoreSink_PanicContained(t *testing.T) {
	base := time.Now()
	clock := base
	sink := &captureSink{}
	m := NewManager(
		WithStallTimeout(5*time.Minute),
		WithSinks(sink),
		WithStoreSink(func(Alert) { panic("boom") }),
	)
	m.now = func() time.Time { return clock }

	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: base})
	clock = base.Add(6 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 1 {
		t.Fatalf("stall lost to a store-sink panic: %+v", fired)
	}
}
