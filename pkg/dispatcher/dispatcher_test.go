package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeTracker is a minimal in-memory Tracker used by the dispatcher
// tests. Safe for concurrent use.
type fakeTracker struct {
	mu        sync.Mutex
	issues    map[string]*tracker.Issue
	claims    map[string]string
	listCalls atomic.Int64

	panicListCandidates atomic.Bool

	// Optional gate used by TestSnapshotLockFreeWhileActorBlocked. When
	// listBlock is non-nil, the first ListCandidates call signals
	// listEntered (once) then blocks receiving on listBlock until the
	// test closes it — simulating a slow tracker call wedging the actor.
	listBlock     chan struct{}
	listEntered   chan struct{}
	listEnterOnce sync.Once

	// Optional gate used by the ADR-028 Step 3 tests. When releaseBlock is
	// non-nil, the first Release call signals releaseEntered (once) then
	// blocks receiving on releaseBlock until the test closes it — simulating
	// a slow tracker Release wedging the off-actor finish worker.
	releaseBlock     chan struct{}
	releaseEntered   chan struct{}
	releaseEnterOnce sync.Once

	// Optional gate used by the ADR-028 Step 4 tests. When updateBlock is
	// non-nil, the first UpdateState call signals updateEntered (once) then
	// blocks receiving on updateBlock until the test closes it — simulating a
	// slow in-progress transition wedging the off-actor dispatch-setup worker.
	updateBlock     chan struct{}
	updateEntered   chan struct{}
	updateEnterOnce sync.Once
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{
		issues: map[string]*tracker.Issue{},
		claims: map[string]string{},
	}
}

func (f *fakeTracker) add(iss tracker.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if iss.CreatedAt.IsZero() {
		iss.CreatedAt = time.Now()
	}
	f.issues[iss.ID] = &iss
}

func (f *fakeTracker) Name() string { return "fake" }

func (f *fakeTracker) ListCandidates(_ context.Context) ([]tracker.Issue, error) {
	f.listCalls.Add(1)
	if f.panicListCandidates.Swap(false) {
		panic("simulated ListCandidates panic")
	}
	if f.listBlock != nil {
		if f.listEntered != nil {
			f.listEnterOnce.Do(func() { close(f.listEntered) })
		}
		<-f.listBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tracker.Issue, 0, len(f.issues))
	for id, iss := range f.issues {
		if iss.WorkflowState != "ready" {
			continue
		}
		if _, claimed := f.claims[id]; claimed {
			continue
		}
		out = append(out, *iss)
	}
	return out, nil
}

func (f *fakeTracker) RefreshStates(_ context.Context, ids []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if iss, ok := f.issues[id]; ok {
			out[id] = iss.WorkflowState
		}
	}
	return out, nil
}

func (f *fakeTracker) UpdateState(_ context.Context, id, newState string) error {
	if f.updateBlock != nil {
		if f.updateEntered != nil {
			f.updateEnterOnce.Do(func() { close(f.updateEntered) })
		}
		<-f.updateBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issues[id]
	if !ok {
		return tracker.ErrNotFound
	}
	iss.WorkflowState = newState
	return nil
}

func (f *fakeTracker) Comment(_ context.Context, _, _ string) error { return tracker.ErrNotSupported }

func (f *fakeTracker) Claim(_ context.Context, id, marker string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.claims[id]; ok && cur != marker {
		return tracker.ErrClaimConflict
	}
	f.claims[id] = marker
	return nil
}

func (f *fakeTracker) Release(_ context.Context, id, _ string) error {
	if f.releaseBlock != nil {
		if f.releaseEntered != nil {
			f.releaseEnterOnce.Do(func() { close(f.releaseEntered) })
		}
		<-f.releaseBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, id)
	return nil
}

// newTestDispatcher builds a Dispatcher with a tmpDir-rooted workspace,
// a quiet logger, and a fake tracker.
func newTestDispatcher(t *testing.T, runner Runner, ft *fakeTracker, polling time.Duration) *Dispatcher {
	t.Helper()
	dir := t.TempDir()
	wsDir := dir + "/ws"
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: int(polling.Milliseconds())},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000},
		Workspace: WorkspaceConfig{Root: wsDir},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	cfg.applyDefaults()
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
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestDispatcherDispatchAndFinish(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{
		ID: "fake:1", Identifier: "fake#1",
		Title: "go", WorkflowState: "ready",
		Assignee: "feature_dev",
	})

	type capture struct {
		runID    string
		assignee string
	}
	dispatched := make(chan capture, 1)
	runner := &StubRunner{Handler: func(_ context.Context, spec DispatchSpec) error {
		dispatched <- capture{runID: spec.RunID, assignee: spec.Assignee}
		return nil
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case got := <-dispatched:
		if got.runID == "" {
			t.Fatal("empty run ID")
		}
		if got.assignee != "feature_dev" {
			t.Fatalf("dispatch spec dropped issue.Assignee: got %q want %q", got.assignee, "feature_dev")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}

	// Wait for the actor to process the finish + release.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if len(snap.Running) == 0 {
			ft.mu.Lock()
			_, claimed := ft.claims["fake:1"]
			ft.mu.Unlock()
			if !claimed {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dispatcher did not release claim, last snapshot=%+v", c.Snapshot())
}

// TestSnapshotLockFreeWhileActorBlocked proves ADR-028 Step 1: Snapshot()
// reads the last-published immutable snapshot lock-free and returns
// promptly even while the actor goroutine is provably blocked inside a
// slow tracker ListCandidates call. Before the read-path decouple,
// Snapshot() routed through the actor and would wedge until the 5s
// timeout, returning a zero Snapshot.
func TestSnapshotLockFreeWhileActorBlocked(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:1", Identifier: "fake#1", Title: "go", WorkflowState: "ready"})
	ft.listBlock = make(chan struct{})
	ft.listEntered = make(chan struct{})

	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error { return nil }}
	// Long polling so only the kick-off tick fires while we hold the gate.
	c := newTestDispatcher(t, runner, ft, time.Hour)

	// Release the gate first (LIFO defer) so the actor always unblocks,
	// even if an assertion below fails.
	defer c.Stop()
	defer close(ft.listBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Wait until the actor is provably parked inside ListCandidates.
	select {
	case <-ft.listEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("actor never entered the blocked ListCandidates call")
	}

	// While the actor is blocked, Snapshot() must return promptly with
	// the last-published (seeded) state — not wait on the wedged actor.
	got := make(chan Snapshot, 1)
	go func() { got <- c.Snapshot() }()
	select {
	case snap := <-got:
		if snap.Name != "test" {
			t.Fatalf("Snapshot returned an empty/zero view while actor blocked: %+v", snap)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Snapshot() blocked on the wedged actor — read path is not decoupled")
	}
}

// TestActorResponsiveWhileDiscoveryInFlight proves ADR-028 Step 2: the
// blocking tracker.ListCandidates call now runs on an off-actor discovery
// goroutine, so the actor keeps processing commands while discovery is in
// flight. We gate the fake tracker's ListCandidates on a channel, let the
// kick-off tick dispatch discovery to the side goroutine, then — while that
// call is still blocked — post a Reload through the actor's command channel
// and assert its handler runs (republishing the snapshot with the new name)
// within a tight deadline. Before Step 2 the actor was parked INSIDE
// ListCandidates during the kick-off tick and could not apply cmdReload, so
// the name would stay "test" until the gate released.
func TestActorResponsiveWhileDiscoveryInFlight(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:1", Identifier: "fake#1", Title: "go", WorkflowState: "ready"})
	ft.listBlock = make(chan struct{})
	ft.listEntered = make(chan struct{})

	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error { return nil }}
	// Long polling so only the kick-off tick fires while we hold the gate.
	c := newTestDispatcher(t, runner, ft, time.Hour)

	// Release the gate first (LIFO defer) so the off-actor discovery
	// goroutine always unblocks — Stop() waits on it via workersWG — even
	// if an assertion below fails.
	defer c.Stop()
	defer close(ft.listBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Wait until the discovery goroutine is provably parked inside
	// ListCandidates (i.e. the actor handed the I/O off and returned).
	select {
	case <-ft.listEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("discovery goroutine never entered the blocked ListCandidates call")
	}

	// While discovery is blocked, post a command through the actor's
	// command channel. cmdReload.apply runs on the actor and republishes
	// the snapshot, so a responsive actor flips the published name.
	newCfg := *c.cfg.Load()
	newCfg.Name = "reloaded"
	c.Reload(&newCfg)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.Snapshot().Name == "reloaded" {
			return // actor processed a command concurrently with in-flight discovery
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("actor did not apply cmdReload while ListCandidates was in flight (snapshot name=%q) — discovery is not off-actor", c.Snapshot().Name)
}

// driveGatedReleaseRun starts c, dispatches the seeded ready issue, drives it
// to a clean finish, and blocks until the off-actor finish worker is provably
// parked inside the gated tracker.Release. Returns once releaseEntered fires.
// The caller MUST `defer close(ft.releaseBlock)` (before `defer c.Stop()`, so
// LIFO unblocks the worker before Stop drains workersWG) and own ctx/cancel.
func driveGatedReleaseRun(t *testing.T, c *Dispatcher, ft *fakeTracker, ctx context.Context) {
	t.Helper()
	c.Start(ctx)
	select {
	case <-ft.releaseEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("finish worker never entered the blocked tracker.Release call")
	}
}

// TestActorResponsiveWhileFinishHTTPInFlight proves ADR-028 Step 3: finishRun's
// blocking tracker HTTP (Release + the clean-finish transition) now runs on an
// off-actor finish worker, so the actor keeps processing commands while that
// HTTP is in flight. We gate the fake tracker's Release on a channel, drive a
// run to a clean finish (so the finish worker parks in Release), and — while
// Release is still blocked — post a Reload through the actor's command channel
// and assert its handler runs (republishing the snapshot with the new name)
// within a tight deadline. Before Step 3 the actor was parked INSIDE the
// synchronous Release in finishRun and could not apply cmdReload.
func TestActorResponsiveWhileFinishHTTPInFlight(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:1", Identifier: "fake#1", Title: "go", WorkflowState: "ready"})
	ft.releaseBlock = make(chan struct{})
	ft.releaseEntered = make(chan struct{})

	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error { return nil }}
	// Long polling so only the kick-off tick fires while we hold the gate.
	c := newTestDispatcher(t, runner, ft, time.Hour)

	// Release the gate first (LIFO defer) so the off-actor finish worker always
	// unblocks — Stop() waits on it via workersWG — even if an assertion fails.
	defer c.Stop()
	defer close(ft.releaseBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driveGatedReleaseRun(t, c, ft, ctx)

	// The slot must already be freed: finishRun deletes the running entry on
	// the actor BEFORE handing the Release HTTP to the worker.
	if n := len(c.Snapshot().Running); n != 0 {
		t.Fatalf("running entries = %d while Release in flight, want 0 (slot freed before the release HTTP)", n)
	}

	// While Release is blocked, post a command through the actor's command
	// channel. cmdReload.apply runs on the actor and republishes the snapshot,
	// so a responsive actor flips the published name.
	newCfg := *c.cfg.Load()
	newCfg.Name = "reloaded"
	c.Reload(&newCfg)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.Snapshot().Name == "reloaded" {
			return // actor processed a command concurrently with the in-flight finish HTTP
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("actor did not apply cmdReload while tracker.Release was in flight (snapshot name=%q) — finishRun HTTP is not off-actor", c.Snapshot().Name)
}

// TestSlotFreedBeforeReleaseHTTP proves the freed concurrency slot is
// observable immediately — Snapshot().Slots.GlobalUsed drops to 0 — even though
// the tracker Release HTTP for the finished run has NOT completed. Slot
// accounting (actor, in-memory) is decoupled from the release HTTP (off-actor
// worker). See ADR-028 Step 3.
func TestSlotFreedBeforeReleaseHTTP(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:1", Identifier: "fake#1", Title: "go", WorkflowState: "ready"})
	ft.releaseBlock = make(chan struct{})
	ft.releaseEntered = make(chan struct{})

	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error { return nil }}
	c := newTestDispatcher(t, runner, ft, time.Hour)

	defer c.Stop()
	defer close(ft.releaseBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driveGatedReleaseRun(t, c, ft, ctx)

	// Release is still blocked in the worker, yet the slot must read free.
	if used := c.Snapshot().Slots.GlobalUsed; used != 0 {
		t.Fatalf("Slots.GlobalUsed = %d while Release in flight, want 0 (slot accounting decoupled from the release HTTP)", used)
	}
}

// TestDiscoveryPanicPostsCandidateError proves the off-actor discovery
// goroutine keeps the actor loop's panic-containment contract for tracker
// adapters: a ListCandidates panic must be logged/reported back as a
// cmdCandidates error so the actor clears discoveryInFlight and a later poll
// can launch discovery again instead of wedging forever.
func TestDiscoveryPanicPostsCandidateError(t *testing.T) {
	ft := newFakeTracker()
	ft.panicListCandidates.Store(true)
	ft.add(tracker.Issue{ID: "fake:panic", Identifier: "fake#panic", Title: "go", WorkflowState: "ready"})

	dispatched := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		select {
		case dispatched <- struct{}{}:
		default:
		}
		return nil
	}}
	c := newTestDispatcher(t, runner, ft, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if calls := ft.listCalls.Load(); calls >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls := ft.listCalls.Load(); calls < 1 {
		t.Fatalf("initial ListCandidates call never happened")
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if snap.LastTrackerError != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if errText := c.Snapshot().LastTrackerError; !strings.Contains(errText, "simulated ListCandidates panic") {
		t.Fatalf("LastTrackerError = %q, want recovered panic", errText)
	}

	c.Refresh()

	select {
	case <-dispatched:
		// A later poll launched a fresh ListCandidates call and dispatched
		// the candidate, proving discoveryInFlight was not stranded by the
		// recovered panic and the dispatcher stayed responsive.
	case <-time.After(10 * time.Second):
		t.Fatalf("dispatcher did not recover from ListCandidates panic and dispatch on a later poll; listCalls=%d snapshot=%+v", ft.listCalls.Load(), c.Snapshot())
	}
	if calls := ft.listCalls.Load(); calls < 2 {
		t.Fatalf("ListCandidates calls = %d, want at least 2 (panic + later poll)", calls)
	}
}

func TestDispatcherRetriesOnFailure(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:2", Identifier: "fake#2", Title: "boom", WorkflowState: "ready"})

	var calls atomic.Int32
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		calls.Add(1)
		return errors.New("simulated failure")
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	// Override the backoff cap to keep the test fast.
	cfg := c.cfg.Load()
	cfg.Agent.MaxRetryBackoffMS = 100
	c.cfg.Store(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected at least 2 dispatch attempts, saw %d", calls.Load())
}

// TestDispatcherGivesUpAfterMaxAttempts is the regression guard for the
// silent-infinite-retry blocker: a deterministically-failing issue must
// stop retrying after MaxAttempts and land in a terminal FailedState
// (here "blocked") instead of rescheduling forever and burning spend.
func TestDispatcherGivesUpAfterMaxAttempts(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:ga", Identifier: "fake#ga", Title: "doomed", WorkflowState: "ready"})

	var calls atomic.Int32
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		calls.Add(1)
		return errors.New("deterministic failure")
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	cfg := c.cfg.Load()
	cfg.Agent.MaxRetryBackoffMS = 50 // keep retries fast
	cfg.Agent.MaxAttempts = 2        // initial + 1 retry, then give up
	cfg.Agent.FailedState = "blocked"
	c.cfg.Store(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	// Wait for the give-up to land the issue in the terminal failed state.
	deadline := time.Now().Add(10 * time.Second)
	state := func() string {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		if iss, ok := ft.issues["fake:ga"]; ok {
			return iss.WorkflowState
		}
		return ""
	}
	for time.Now().Before(deadline) {
		if state() == "blocked" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := state(); got != "blocked" {
		t.Fatalf("issue state = %q, want blocked after give-up", got)
	}
	// Exactly MaxAttempts dispatches — the give-up moved the issue out of
	// the eligible "ready" set so no further dispatch can pick it up.
	if got := calls.Load(); got != 2 {
		t.Fatalf("dispatch calls = %d, want 2 (initial + 1 retry, then give up)", got)
	}
	// No lingering bookkeeping: the retry entry was dropped on give-up.
	// The tracker's "blocked" write happens on the give-up WORKER before
	// the actor applies cmdDropRetry and republishes — so poll for the
	// snapshot to converge instead of asserting a single read (the -race
	// CI flake: retries=1 observed in the gap).
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if len(snap.Running) == 0 && len(snap.Retries) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap := c.Snapshot()
	t.Fatalf("after give-up: running=%d retries=%d, want 0/0", len(snap.Running), len(snap.Retries))
}

func TestDispatcherRespectsClaimConflict(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:3", Identifier: "fake#3", WorkflowState: "ready"})
	// Pre-claim under a different marker — dispatcher must not dispatch.
	if err := ft.Claim(context.Background(), "fake:3", "someone-else"); err != nil {
		t.Fatalf("pre-claim: %v", err)
	}

	var calls atomic.Int32
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		calls.Add(1)
		return nil
	}}
	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("dispatcher dispatched a pre-claimed issue (%d calls)", calls.Load())
	}
}

func TestDispatcherCancel(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{ID: "fake:4", Identifier: "fake#4", WorkflowState: "ready"})

	dispatchStarted := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		dispatchStarted <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-dispatchStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never started")
	}

	c.Cancel("fake:4")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cancel did not remove the running entry")
}

func TestBackoffMath(t *testing.T) {
	cases := []struct {
		attempt int
		cap     time.Duration
		want    time.Duration
	}{
		{0, 0, time.Second},
		{1, 10 * time.Minute, 10 * time.Second},
		{2, 10 * time.Minute, 20 * time.Second},
		{3, 10 * time.Minute, 40 * time.Second},
		{4, 10 * time.Minute, 80 * time.Second},
		{6, 60 * time.Second, 60 * time.Second}, // capped
	}
	for _, c := range cases {
		got := computeBackoff(c.attempt, c.cap)
		if got != c.want {
			t.Errorf("computeBackoff(%d, %s) = %s, want %s", c.attempt, c.cap, got, c.want)
		}
	}
}
