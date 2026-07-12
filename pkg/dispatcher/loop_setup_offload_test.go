package dispatcher

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// newOffloadTestDispatcher builds a dispatcher with a configurable
// MaxConcurrent and an explicit RunningState (so the in-progress transition
// fires), backed by the plain fakeTracker (whose updateBlock gate the Step-4
// tests use to wedge the off-actor setup worker).
func newOffloadTestDispatcher(t *testing.T, runner Runner, ft *fakeTracker, polling time.Duration, maxConcurrent int) *Dispatcher {
	t.Helper()
	dir := t.TempDir()
	wsDir := dir + "/ws"
	cfg := &Config{
		Name:     "test",
		Workflow: t.TempDir() + "/fake.bot",
		Tracker:  TrackerConfig{Kind: "fake"},
		Polling:  PollingConfig{IntervalMS: int(polling.Milliseconds())},
		Agent: AgentConfig{
			MaxConcurrent:     maxConcurrent,
			MaxRetryBackoffMS: 1000,
			RunningState:      "in_progress",
		},
		Workspace: WorkspaceConfig{Root: wsDir},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	if cfg.Polling.IntervalMS <= 0 {
		cfg.Polling.IntervalMS = DefaultPollingInterval
	}
	ws, err := NewWorkspaces(wsDir)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    ft,
		Runner:     runner,
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
		StoreDir:   dir + "/store",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestActorResponsiveWhileDispatchSetupInFlight proves ADR-028 Step 4: the
// post-claim dispatch setup I/O (the in-progress UpdateState + workspaces.Create)
// now runs on an off-actor worker, so the actor keeps processing commands while
// that transition is in flight. We gate the fake tracker's UpdateState on a
// channel, let dispatch claim the one ready issue (parking the setup worker in
// UpdateState), and — while it is still blocked — post a Reload through the
// actor's command channel and assert its handler runs (republishing the
// snapshot with the new name) within a tight deadline. If UpdateState still ran
// on the actor, the actor would be parked inside it and could not apply
// cmdReload.
func TestActorResponsiveWhileDispatchSetupInFlight(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:1", Identifier: "fake#1", Title: "go", WorkflowState: "ready"})
	ft.updateBlock = make(chan struct{})
	ft.updateEntered = make(chan struct{})

	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error { return nil }}
	// Long polling so only the kick-off tick fires while we hold the gate.
	c := newOffloadTestDispatcher(t, runner, ft, time.Hour, 4)

	// Release the gate first (LIFO defer) so the off-actor setup worker always
	// unblocks — Stop() waits on it via workersWG — even if an assertion fails.
	defer c.Stop()
	defer close(ft.updateBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Wait until the setup worker is provably parked inside the gated
	// UpdateState (i.e. the actor handed the I/O off and returned).
	select {
	case <-ft.updateEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch-setup worker never entered the blocked UpdateState call")
	}

	// While setup is blocked, post a command through the actor's command
	// channel. cmdReload.apply runs on the actor and republishes the snapshot,
	// so a responsive actor flips the published name.
	newCfg := *c.cfg.Load()
	newCfg.Name = "reloaded"
	c.Reload(&newCfg)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.Snapshot().Name == "reloaded" {
			return // actor processed a command concurrently with the in-flight setup
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("actor did not apply cmdReload while the in-progress UpdateState was in flight (snapshot name=%q) — dispatch setup is not off-actor", c.Snapshot().Name)
}

// TestDispatch_ClaimConflictAllocatesNothing proves the hard anti-race
// constraint of ADR-028 Step 4: tracker.Claim stays atomic on the actor and is
// the sole conflict authority. When Claim returns ErrClaimConflict the issue is
// skipped with NO running entry, NO slot consumed, NO transition, and NO
// off-actor setup worker launched — exactly as before the offload.
func TestDispatch_ClaimConflictAllocatesNothing(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{ID: "fake:cc", Identifier: "fake#cc", Title: "go", WorkflowState: "ready"})

	ctx := context.Background()
	// Someone else already holds the claim → the dispatcher's Claim conflicts.
	if err := ft.Claim(ctx, "fake:cc", "other-host"); err != nil {
		t.Fatalf("seed foreign claim: %v", err)
	}

	c, _ := newStateTestDispatcher(t, &StubRunner{}, ft, time.Hour, "in_progress")

	iss := tracker.Issue{ID: "fake:cc", Identifier: "fake#cc", Title: "go", WorkflowState: "ready"}
	c.dispatch(ctx, iss)

	if _, running := c.state.running["fake:cc"]; running {
		t.Fatal("claim conflict must not record a running entry")
	}
	if used := c.state.slotsByState["ready"]; used != 0 {
		t.Fatalf("claim conflict consumed a slot: slotsByState[ready] = %d, want 0", used)
	}
	if calls := ft.calls(); len(calls) != 0 {
		t.Fatalf("claim conflict performed an UpdateState transition: %+v, want none", calls)
	}

	// No off-actor setup worker may have been launched: workersWG must be empty
	// (Wait returns immediately). A leaked worker would block this.
	done := make(chan struct{})
	go func() { c.workersWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a setup worker was launched on a claim conflict (workersWG not drained)")
	}
}

// TestDispatch_SlotCountedFromClaimTime proves the concurrency cap holds from
// claim time: the slot is allocated on the actor the instant Claim succeeds,
// BEFORE the off-actor setup I/O runs — so a second eligible issue cannot be
// over-dispatched while the first issue's setup is still in flight. We gate the
// in-progress UpdateState so the first issue's setup worker parks, then assert
// exactly one slot is used (and one running entry) even though two ready issues
// are eligible under MaxConcurrent=1.
func TestDispatch_SlotCountedFromClaimTime(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:a", Identifier: "fake#a", Title: "go", WorkflowState: "ready"})
	ft.add(tracker.Issue{ID: "fake:b", Identifier: "fake#b", Title: "go", WorkflowState: "ready"})
	ft.updateBlock = make(chan struct{})
	ft.updateEntered = make(chan struct{})

	// A run that never returns keeps any started slot held; but the run worker
	// won't even start here because the setup worker is parked in UpdateState.
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	c := newOffloadTestDispatcher(t, runner, ft, time.Hour, 1)

	defer c.Stop()
	defer close(ft.updateBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Wait until the first claimed issue's setup worker is parked in the gated
	// UpdateState — i.e. its slot was already allocated on the actor.
	select {
	case <-ft.updateEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch-setup worker never reached the gated UpdateState")
	}

	// Give the actor a beat to (not) dispatch the second issue. With the slot
	// counted from claim time, the global cap is already full, so the second
	// candidate is skipped in the SAME cmdCandidates scan.
	time.Sleep(50 * time.Millisecond)

	snap := c.Snapshot()
	if snap.Slots.GlobalUsed != 1 {
		t.Fatalf("GlobalUsed = %d while setup in flight, want 1 (slot counted from claim time)", snap.Slots.GlobalUsed)
	}
	if len(snap.Running) != 1 {
		t.Fatalf("running entries = %d, want exactly 1 (cap enforced from claim time — no over-dispatch while setup in flight)", len(snap.Running))
	}
}
