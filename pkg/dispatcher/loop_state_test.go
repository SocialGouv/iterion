package dispatcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// stateAwareTracker extends the in-memory fake with bookkeeping for
// UpdateState calls so the in-progress transition tests can assert the
// sequence of state moves the dispatcher performs.
type stateAwareTracker struct {
	*fakeTracker

	// updateCallsMu protects updateCalls + updateRejectStates. The base
	// fakeTracker has its own mu; we use a separate lock so we don't
	// have to take both whenever we add a row.
	updateCallsMu     atomic.Value // []stateUpdate, swapped under updateMu
	updateMu          chanMu       // serialises append; matches fakeTracker.mu style
	updateRejectState string       // when non-empty, UpdateState rejects moves into this state
}

// chanMu is a single-slot channel used as a mutex. The dispatcher tests
// already use sync.Mutex on the underlying fakeTracker; the channel form
// here keeps lock-ordering explicit (Lock + Unlock are non-blocking
// against the inner tracker's mu).
type chanMu chan struct{}

func newChanMu() chanMu { ch := make(chanMu, 1); return ch }
func (m chanMu) Lock()  { m <- struct{}{} }
func (m chanMu) Unlock() {
	select {
	case <-m:
	default:
	}
}

type stateUpdate struct {
	id       string
	newState string
}

func newStateAwareTracker() *stateAwareTracker {
	t := &stateAwareTracker{
		fakeTracker: newFakeTracker(),
		updateMu:    newChanMu(),
	}
	t.updateCallsMu.Store([]stateUpdate{})
	return t
}

func (t *stateAwareTracker) UpdateState(ctx context.Context, id, newState string) error {
	t.updateMu.Lock()
	cur, _ := t.updateCallsMu.Load().([]stateUpdate)
	out := make([]stateUpdate, len(cur)+1)
	copy(out, cur)
	out[len(cur)] = stateUpdate{id: id, newState: newState}
	t.updateCallsMu.Store(out)
	reject := t.updateRejectState
	t.updateMu.Unlock()
	if reject != "" && newState == reject {
		return tracker.ErrTransitionRejected
	}
	return t.fakeTracker.UpdateState(ctx, id, newState)
}

func (t *stateAwareTracker) calls() []stateUpdate {
	cur, _ := t.updateCallsMu.Load().([]stateUpdate)
	out := make([]stateUpdate, len(cur))
	copy(out, cur)
	return out
}

func (t *stateAwareTracker) issueState(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if iss, ok := t.issues[id]; ok {
		return iss.WorkflowState
	}
	return ""
}

// newStateTestDispatcher mirrors newTestDispatcher but accepts the
// state-aware tracker so callers can assert on UpdateState calls.
func newStateTestDispatcher(t *testing.T, runner Runner, ft *stateAwareTracker, polling time.Duration, runningState string) (*Dispatcher, string) {
	t.Helper()
	dir := t.TempDir()
	wsDir := dir + "/ws"
	cfg := &Config{
		Name:     "test",
		Workflow: t.TempDir() + "/fake.iter",
		Tracker:  TrackerConfig{Kind: "fake"},
		Polling:  PollingConfig{IntervalMS: int(polling.Milliseconds())},
		Agent: AgentConfig{
			MaxConcurrent:     4,
			MaxRetryBackoffMS: 1000,
			RunningState:      runningState,
		},
		Workspace: WorkspaceConfig{Root: wsDir},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	// applyDefaults would clobber an intentionally-empty RunningState
	// back to "in_progress"; tests that exercise the disabled path
	// bypass applyDefaults entirely and rely on the explicit field
	// values above. The other defaults (polling interval floor) are
	// already non-zero so this is safe.
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
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, wsDir
}

// TestDispatch_TransitionsToInProgress verifies the dispatcher moves a
// claimed "ready" issue to "in_progress" before allocating a workspace.
func TestDispatch_TransitionsToInProgress(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:t1", Identifier: "fake#t1",
		Title: "go", WorkflowState: "ready",
	})

	// Block the runner so we can inspect the in-flight state.
	gate := make(chan struct{})
	dispatched := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		dispatched <- struct{}{}
		<-gate
		return nil
	}}

	c, _ := newStateTestDispatcher(t, runner, ft, 50*time.Millisecond, "in_progress")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer func() {
		close(gate)
		c.Stop()
	}()

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never started")
	}

	// The runner is blocked; the tracker should now report in_progress.
	if got := ft.issueState("fake:t1"); got != "in_progress" {
		t.Fatalf("issue state = %q, want in_progress", got)
	}
	calls := ft.calls()
	if len(calls) != 1 || calls[0].id != "fake:t1" || calls[0].newState != "in_progress" {
		t.Fatalf("UpdateState calls = %+v, want one move to in_progress", calls)
	}
}

// TestDispatch_SkipsTransitionWhenRunningStateEmpty verifies that
// setting cfg.Agent.RunningState = "" disables the transition entirely.
func TestDispatch_SkipsTransitionWhenRunningStateEmpty(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:t2", Identifier: "fake#t2",
		Title: "go", WorkflowState: "ready",
	})

	gate := make(chan struct{})
	dispatched := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		dispatched <- struct{}{}
		<-gate
		return nil
	}}

	c, _ := newStateTestDispatcher(t, runner, ft, 50*time.Millisecond, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer func() {
		close(gate)
		c.Stop()
	}()

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never started")
	}

	if got := ft.issueState("fake:t2"); got != "ready" {
		t.Fatalf("issue state = %q, want ready (no transition)", got)
	}
	if calls := ft.calls(); len(calls) != 0 {
		t.Fatalf("UpdateState calls = %+v, want none when RunningState empty", calls)
	}
}

// TestDispatch_SkipsTransitionWhenAlreadyInTargetState verifies the
// idempotency check: an issue already in the target state shouldn't
// trigger an UpdateState call.
func TestDispatch_SkipsTransitionWhenAlreadyInTargetState(t *testing.T) {
	ft := newStateAwareTracker()
	// Pre-position the issue in in_progress (e.g. operator dragged it
	// before the dispatcher claimed it).
	ft.add(tracker.Issue{
		ID: "fake:t3", Identifier: "fake#t3",
		Title: "go", WorkflowState: "in_progress",
	})

	// The runner returns immediately so the worker drains cleanly.
	// We don't need a gate — the dispatch happens synchronously up to
	// the goroutine spawn, and that's the only point where the
	// transition would have been emitted.
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		return nil
	}}

	c, _ := newStateTestDispatcher(t, runner, ft, 50*time.Millisecond, "in_progress")

	// fakeTracker.ListCandidates only returns "ready" — call dispatch
	// directly to exercise the in_progress source-state path.
	iss := tracker.Issue{
		ID: "fake:t3", Identifier: "fake#t3",
		Title: "go", WorkflowState: "in_progress",
	}
	c.dispatch(context.Background(), iss)

	// The transition check is synchronous in dispatch(); the worker
	// goroutine spawn happens after. So immediately after dispatch
	// returns, ft.calls() reflects the (lack of) transition.
	if calls := ft.calls(); len(calls) != 0 {
		t.Fatalf("UpdateState calls = %+v, want none when already in target state", calls)
	}

	// Drain the worker goroutine so the test exits cleanly.
	c.workersWG.Wait()
}

// TestDispatch_RevertsOnWorkspaceCreateFailure exercises the
// dispatch-rollback path. We deliberately seed the workspace target as
// a regular file so Workspaces.Create reports "exists and is not a
// directory" — the dispatcher must revert in_progress back to ready
// before releasing the claim.
func TestDispatch_RevertsOnWorkspaceCreateFailure(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:t4", Identifier: "fake#t4",
		Title: "go", WorkflowState: "ready",
	})

	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	// Seed a file where the workspace directory should land. The
	// sanitized key for "fake:t4" replaces ':' with '_'.
	collidingFile := filepath.Join(wsDir, "fake_t4")
	if err := os.WriteFile(collidingFile, []byte("collision"), 0o644); err != nil {
		t.Fatalf("seed colliding file: %v", err)
	}

	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.iter",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: wsDir},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	ws, err := NewWorkspaces(wsDir)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    ft,
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Invoke dispatch directly (don't run the actor — we want to
	// observe the synchronous rollback without a tick interleaving).
	iss := tracker.Issue{
		ID: "fake:t4", Identifier: "fake#t4",
		Title: "go", WorkflowState: "ready",
	}
	c.dispatch(context.Background(), iss)

	// Workspace-create-fail path: claim → transition → workspace fails
	// → revert → release. The issue should be back in ready and not
	// running.
	if got := ft.issueState("fake:t4"); got != "ready" {
		t.Fatalf("issue state = %q, want ready after revert", got)
	}
	if _, running := c.state.running["fake:t4"]; running {
		t.Fatal("running entry should not be recorded after workspace failure")
	}
	calls := ft.calls()
	// Expect exactly two UpdateState calls: forward to in_progress,
	// then revert back to ready.
	if len(calls) != 2 ||
		calls[0].newState != "in_progress" ||
		calls[1].newState != "ready" {
		t.Fatalf("UpdateState calls = %+v, want [in_progress, ready]", calls)
	}
	// And the claim should be released.
	ft.mu.Lock()
	_, stillClaimed := ft.claims["fake:t4"]
	ft.mu.Unlock()
	if stillClaimed {
		t.Fatal("claim not released after workspace failure")
	}
}

// TestFinishRun_RevertsOnCancel exercises the cancel-then-finish flow:
// the dispatcher must revert the in_progress transition so the next
// tick can re-pick the issue from "ready".
func TestFinishRun_RevertsOnCancel(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:t5", Identifier: "fake#t5",
		Title: "go", WorkflowState: "ready",
	})

	dispatchStarted := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		dispatchStarted <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}

	c, _ := newStateTestDispatcher(t, runner, ft, 50*time.Millisecond, "in_progress")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-dispatchStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never started")
	}

	// At this point the issue should already be in_progress.
	if got := ft.issueState("fake:t5"); got != "in_progress" {
		t.Fatalf("issue state = %q after dispatch, want in_progress", got)
	}

	c.Cancel("fake:t5")

	// Wait for the cancel to propagate through finishRun.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ft.issueState("fake:t5") == "ready" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := ft.issueState("fake:t5"); got != "ready" {
		t.Fatalf("issue state = %q after cancel, want ready (reverted)", got)
	}

	calls := ft.calls()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 UpdateState calls (forward + revert), got %+v", calls)
	}
	last := calls[len(calls)-1]
	if last.newState != "ready" {
		t.Fatalf("final UpdateState = %q, want ready", last.newState)
	}
}

// TestFinishRun_DoesNotRevertOnCleanFinish verifies the clean-finish
// path: when the workflow returns nil and the operator hasn't moved
// the state, the issue stays in in_progress (the operator inspects it
// there until they explicitly close or re-queue).
func TestFinishRun_DoesNotRevertOnCleanFinish(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:t6", Identifier: "fake#t6",
		Title: "go", WorkflowState: "ready",
	})

	finished := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		finished <- struct{}{}
		return nil
	}}

	c, _ := newStateTestDispatcher(t, runner, ft, 50*time.Millisecond, "in_progress")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never finished")
	}

	// Give the actor a moment to process cmdRunFinished.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if len(snap.Running) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The issue should remain in in_progress — clean finishes don't
	// revert. The workflow itself would be expected to call
	// UpdateState if it wanted to move on (e.g. doc-align → review).
	if got := ft.issueState("fake:t6"); got != "in_progress" {
		t.Fatalf("issue state = %q after clean finish, want in_progress", got)
	}

	calls := ft.calls()
	// Only the forward move; no revert.
	if len(calls) != 1 || calls[0].newState != "in_progress" {
		t.Fatalf("UpdateState calls = %+v, want only [in_progress]", calls)
	}
}

// TestFinishRun_AutoTransitionsToCompletedState is the regression
// guard for the dispatch-loop bug: a workflow that finishes cleanly
// without moving the issue state (typically because it lacks
// board.move capability — dispatcher_default is the prime offender)
// used to leave the issue in `in_progress`; with `in_progress`
// marked eligible:true on the default board (needed for crash
// recovery), the next poll re-dispatched the SAME issue, again, and
// again — burning model spend in a tight loop. The fix moves the
// issue from RunningState into CompletedState ("review" by default)
// on clean finish when the workflow itself didn't change the state.
func TestFinishRun_AutoTransitionsToCompletedState(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:auto1", Identifier: "fake#auto1",
		Title: "no board.move workflow", WorkflowState: "ready",
	})

	finished := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		finished <- struct{}{}
		return nil
	}}

	c, _ := newStateTestDispatcherWithCompleted(t, runner, ft, 50*time.Millisecond, "in_progress", "review")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never finished")
	}

	// Wait for cmdRunFinished + the auto-transition's RefreshStates +
	// UpdateState to drain through the actor.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ft.issueState("fake:auto1") == "review" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := ft.issueState("fake:auto1"); got != "review" {
		t.Fatalf("issue state = %q after clean finish, want review (auto-transition)", got)
	}
	// Two forward moves: ready → in_progress (dispatch) then
	// in_progress → review (clean-finish auto-transition).
	calls := ft.calls()
	if len(calls) != 2 || calls[0].newState != "in_progress" || calls[1].newState != "review" {
		t.Fatalf("UpdateState calls = %+v, want [in_progress, review]", calls)
	}
}

// TestFinishRun_SkipsAutoTransitionWhenWorkflowMovedState verifies
// the safety check: a workflow that already moved the issue out of
// RunningState (e.g. doc-align → "review", or a board-aware bot
// picking "done") keeps its terminal state — we don't second-guess
// it by re-applying the default CompletedState.
func TestFinishRun_SkipsAutoTransitionWhenWorkflowMovedState(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:auto2", Identifier: "fake#auto2",
		Title: "board-aware workflow", WorkflowState: "ready",
	})

	finished := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		// Simulate a board-aware workflow that moved the issue to
		// "done" mid-run, before signalling clean exit.
		_ = ft.fakeTracker.UpdateState(ctx, "fake:auto2", "done")
		finished <- struct{}{}
		return nil
	}}

	c, _ := newStateTestDispatcherWithCompleted(t, runner, ft, 50*time.Millisecond, "in_progress", "review")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never finished")
	}

	// Give the actor a moment to process cmdRunFinished. The
	// auto-transition's RefreshStates probe should see "done" and
	// leave the issue alone.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if len(snap.Running) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := ft.issueState("fake:auto2"); got != "done" {
		t.Fatalf("issue state = %q, want done (workflow's choice preserved)", got)
	}
}

// newStateTestDispatcherWithCompleted is newStateTestDispatcher with
// an explicit CompletedState override — kept as a separate helper so
// the existing tests don't grow another positional argument.
func newStateTestDispatcherWithCompleted(t *testing.T, runner Runner, ft *stateAwareTracker, polling time.Duration, runningState, completedState string) (*Dispatcher, string) {
	t.Helper()
	c, wsDir := newStateTestDispatcher(t, runner, ft, polling, runningState)
	cfg := c.cfg.Load()
	cfg.Agent.CompletedState = completedState
	c.cfg.Store(cfg)
	return c, wsDir
}

// TestRevertTransition_SkipsWhenStateMovedExternally asserts the
// safety check: if the workflow or operator moved the issue out of
// the running state mid-run, the revert leaves the new state alone.
func TestRevertTransition_SkipsWhenStateMovedExternally(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{
		ID: "fake:t7", Identifier: "fake#t7", WorkflowState: "in_progress",
	})

	c, _ := newStateTestDispatcher(t, &StubRunner{}, ft, 50*time.Millisecond, "in_progress")

	// Pretend the workflow moved the state to "review" before the
	// revert ran.
	if err := ft.fakeTracker.UpdateState(context.Background(), "fake:t7", "review"); err != nil {
		t.Fatalf("seed review state: %v", err)
	}
	// Drop the UpdateState that the seed produced so the assertion
	// below only counts the dispatcher's calls. (We added one
	// UpdateState via the wrapping stateAwareTracker.UpdateState.)
	ft.updateMu.Lock()
	ft.updateCallsMu.Store([]stateUpdate{})
	ft.updateMu.Unlock()

	c.revertTransition(context.Background(), "fake:t7", "fake#t7", "ready", "in_progress")

	if got := ft.issueState("fake:t7"); got != "review" {
		t.Fatalf("issue state = %q after revert attempt, want review (untouched)", got)
	}
	if calls := ft.calls(); len(calls) != 0 {
		t.Fatalf("revertTransition shouldn't have called UpdateState, got %+v", calls)
	}
}

// TestDispatch_TolerateTransitionRejection asserts that a tracker
// returning ErrTransitionRejected on the forward move doesn't abort
// dispatch — the claim is already taken and the workflow should still
// run.
func TestDispatch_TolerateTransitionRejection(t *testing.T) {
	ft := newStateAwareTracker()
	ft.updateRejectState = "in_progress"
	ft.add(tracker.Issue{
		ID: "fake:t8", Identifier: "fake#t8",
		Title: "go", WorkflowState: "ready",
	})

	dispatched := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		dispatched <- struct{}{}
		return nil
	}}

	c, _ := newStateTestDispatcher(t, runner, ft, 50*time.Millisecond, "in_progress")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never started despite rejected transition")
	}

	// The issue state should still be "ready" because the transition
	// was rejected — and the dispatcher must NOT have recorded
	// TransitionedFromState, so finishRun won't try to revert a move
	// that never happened.
	if got := ft.issueState("fake:t8"); got != "ready" {
		t.Fatalf("issue state = %q, want ready (transition rejected)", got)
	}
}

// TestApplyDefaults_RunningStateDefault verifies the YAML default
// behaviour: an unset running_state lands as "in_progress", and the
// "none" sentinel disables the transition (mapped back to "").
func TestApplyDefaults_RunningStateDefault(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unset", "", "in_progress"},
		{"explicit", "review", "review"},
		{"none-disables", "none", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Agent: AgentConfig{RunningState: tc.in}}
			cfg.applyDefaults()
			if cfg.Agent.RunningState != tc.want {
				t.Fatalf("RunningState = %q, want %q", cfg.Agent.RunningState, tc.want)
			}
		})
	}
}
