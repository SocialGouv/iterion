package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// lastRunCall captures one SetLastRun invocation so the test can
// assert on the (id, run, workdir) trio without coupling to the
// native package.
type lastRunCall struct {
	id      string
	runID   string
	workdir string
}

// lastRunTracker wraps fakeTracker and implements the optional
// SetLastRun shape the dispatcher type-asserts to. Records each call
// so the test can assert ordering + content.
type lastRunTracker struct {
	*fakeTracker

	mu            sync.Mutex
	calls         []lastRunCall
	awaitingCalls []bool
}

func newLastRunTracker() *lastRunTracker {
	return &lastRunTracker{fakeTracker: newFakeTracker()}
}

func (t *lastRunTracker) SetLastRun(id, runID, workdir string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, lastRunCall{id: id, runID: runID, workdir: workdir})
	return nil
}

func (t *lastRunTracker) lastRunCalls() []lastRunCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]lastRunCall, len(t.calls))
	copy(out, t.calls)
	return out
}

// SetAwaitingInput records the denormalized pause-hint writes so tests can
// assert the dispatcher sets true on pause and clears false on clean finish.
func (t *lastRunTracker) SetAwaitingInput(id string, v bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.awaitingCalls = append(t.awaitingCalls, v)
	return nil
}

func (t *lastRunTracker) awaitingInputCalls() []bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]bool, len(t.awaitingCalls))
	copy(out, t.awaitingCalls)
	return out
}

// TestFinishRun_StampsLastRunOnNativeTracker exercises the stamp path
// via the type-assertion: the fake implements SetLastRun, the
// dispatcher's finishRun must call it with the right (issue, run,
// workdir) tuple regardless of whether the run finished cleanly or
// failed (best-effort: the operator always wants the link back to
// the most recent run that processed the issue).
func TestFinishRun_StampsLastRunOnNativeTracker(t *testing.T) {
	ft := newLastRunTracker()

	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	storeDir := filepath.Join(dir, "store")
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
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
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
		StoreDir:   storeDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Plant a fake run.json so resolveRunWorkdir picks up the canonical
	// WorkDir (mirrors the worktree:auto case where Run.WorkDir is
	// swapped to the worktree path mid-run).
	runID := "run-stamp-1"
	runDir := filepath.Join(storeDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	wtPath := filepath.Join(storeDir, "worktrees", runID)
	runJSON, _ := json.Marshal(map[string]any{
		"id":       runID,
		"work_dir": wtPath,
	})
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), runJSON, 0o644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	// Seed a running entry directly and invoke finishRun on the actor
	// goroutine via the cmd channel — this exercises the same path
	// cmdRunFinished.apply uses, without spinning up the full polling
	// loop. ctx parent for the entry is background; revertTransition's
	// internal release uses its own timeout-bound ctx.
	issueID := "fake:stamp-1"
	ft.add(tracker.Issue{
		ID: issueID, Identifier: "fake#stamp-1",
		Title: "go", WorkflowState: "in_progress",
	})

	c.state.running[issueID] = &runningEntry{
		IssueID:       issueID,
		Identifier:    "fake#stamp-1",
		RunID:         runID,
		WorkflowState: "in_progress",
		WorkspacePath: filepath.Join(wsDir, "fake_stamp-1"),
		StartedAt:     time.Now(),
	}

	c.finishRun(context.Background(), issueID, nil)

	calls := ft.lastRunCalls()
	if len(calls) != 1 {
		t.Fatalf("SetLastRun calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.id != issueID {
		t.Fatalf("call id = %q, want %q", got.id, issueID)
	}
	if got.runID != runID {
		t.Fatalf("call runID = %q, want %q", got.runID, runID)
	}
	// The dispatcher must prefer the run.json work_dir over the
	// dispatcher's WorkspacePath — that's the load-bearing piece for
	// worktree:auto runs whose diff lives outside the workspace dir.
	if got.workdir != wtPath {
		t.Fatalf("call workdir = %q, want %q (from run.json)", got.workdir, wtPath)
	}
}

// TestFinishRun_StampsOnFailureToo asserts the stamp lands even when
// the run failed — the operator still wants to inspect the partial
// diff via the worktree link.
func TestFinishRun_StampsOnFailureToo(t *testing.T) {
	ft := newLastRunTracker()

	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
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
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
		// No StoreDir → resolveRunWorkdir returns "" → falls back to
		// WorkspacePath. Asserts the fallback path is wired.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	issueID := "fake:stamp-2"
	wsPath := filepath.Join(wsDir, "fake_stamp-2")
	ft.add(tracker.Issue{
		ID: issueID, Identifier: "fake#stamp-2",
		Title: "go", WorkflowState: "in_progress",
	})
	c.state.running[issueID] = &runningEntry{
		IssueID:               issueID,
		Identifier:            "fake#stamp-2",
		RunID:                 "run-fail-1",
		WorkflowState:         "in_progress",
		WorkspacePath:         wsPath,
		StartedAt:             time.Now(),
		TransitionedFromState: "ready",
	}

	// Use a non-cancellation error so we hit the failure (not soft-stop)
	// branch — both branches stamp, but explicitly exercising the
	// non-cancel path guards against a regression where stampLastRun
	// gets accidentally tucked under the err==nil arm.
	c.finishRun(context.Background(), issueID, errPermanentFailure{})

	calls := ft.lastRunCalls()
	if len(calls) != 1 {
		t.Fatalf("SetLastRun calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.workdir != wsPath {
		t.Fatalf("fallback workdir = %q, want WorkspacePath %q", got.workdir, wsPath)
	}
	if got.runID != "run-fail-1" {
		t.Fatalf("runID = %q, want run-fail-1", got.runID)
	}
}

// errPermanentFailure is a tagged error that fakeTracker doesn't
// recognise as cancellation — used to drive the failure branch of
// finishRun above.
type errPermanentFailure struct{}

func (errPermanentFailure) Error() string { return "permanent failure" }

// TestFinishRun_AwaitingInputBadge asserts the denormalized pause hint:
// the ErrRunPaused/ErrRunPausedOperator park arm sets AwaitingInput=true
// (so the board grid can badge the card without an N+1 run fetch), and a
// clean terminal finish clears it back to false.
func TestFinishRun_AwaitingInputBadge(t *testing.T) {
	newDispatcher := func(t *testing.T, ft *lastRunTracker, issueID string) *Dispatcher {
		t.Helper()
		dir := t.TempDir()
		wsDir := filepath.Join(dir, "ws")
		cfg := &Config{
			Name:      "test",
			Workflow:  t.TempDir() + "/fake.bot",
			Tracker:   TrackerConfig{Kind: "fake"},
			Polling:   PollingConfig{IntervalMS: 50},
			Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
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
			Runner:     &StubRunner{},
			Workspaces: ws,
			Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
			HostMarker: "test",
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ft.add(tracker.Issue{ID: issueID, Identifier: "fake#ai", Title: "go", WorkflowState: "in_progress"})
		c.state.running[issueID] = &runningEntry{
			IssueID:       issueID,
			Identifier:    "fake#ai",
			RunID:         "run-ai-1",
			WorkflowState: "in_progress",
			WorkspacePath: filepath.Join(wsDir, "fake_ai"),
			StartedAt:     time.Now(),
		}
		c.state.slotsByState["in_progress"] = 1
		return c
	}

	t.Run("pause sets true", func(t *testing.T) {
		const issueID = "fake:ai-pause"
		ft := newLastRunTracker()
		c := newDispatcher(t, ft, issueID)
		c.finishRun(context.Background(), issueID, runtime.ErrRunPaused)
		got := ft.awaitingInputCalls()
		if len(got) != 1 || got[0] != true {
			t.Fatalf("awaiting-input calls = %v, want [true]", got)
		}
	})

	t.Run("clean finish clears false", func(t *testing.T) {
		const issueID = "fake:ai-done"
		ft := newLastRunTracker()
		c := newDispatcher(t, ft, issueID)
		c.finishRun(context.Background(), issueID, nil)
		got := ft.awaitingInputCalls()
		if len(got) != 1 || got[0] != false {
			t.Fatalf("awaiting-input calls = %v, want [false]", got)
		}
	})
}

// applyNextCmd drains one command the off-actor finish worker posted on c.cmds
// and applies it on the test goroutine. Used by the give-up tests, which drive
// finishRun directly (no actor running) and need the worker's cmdDropRetry
// applied before asserting retry bookkeeping. Only valid when no actor
// goroutine owns c.cmds.
func applyNextCmd(t *testing.T, c *Dispatcher, ctx context.Context) {
	t.Helper()
	select {
	case command := <-c.cmds:
		command.apply(c, ctx)
	case <-time.After(10 * time.Second):
		t.Fatal("expected a command posted by the off-actor finish worker, got none")
	}
}

// TestFinishRun_GiveUpMovesToFailedState exercises the ADR-028 Step 3 give-up
// path: an exhausted failure schedules the retry optimistically (the in-memory
// re-dispatch guard) on the actor, then the off-actor finish worker moves the
// issue to FailedState and — on success — drops that guard so the give-up is
// final. End state: "blocked", no retry entry. Behaviour is unchanged from the
// former on-actor giveUpIfExhausted; only WHERE the tracker move runs changed.
func TestFinishRun_GiveUpMovesToFailedState(t *testing.T) {
	ft := newStateAwareTracker()
	ft.add(tracker.Issue{ID: "fake:gv", Identifier: "fake#gv", Title: "go", WorkflowState: "in_progress"})

	c, wsDir := newStateTestDispatcher(t, &StubRunner{}, ft, time.Hour, "in_progress")
	cfg := c.cfg.Load()
	cfg.Agent.MaxAttempts = 1 // Attempt+1 == 1 reaches the cap → exhausted
	cfg.Agent.FailedState = "blocked"
	c.cfg.Store(cfg)

	issueID := "fake:gv"
	c.state.running[issueID] = &runningEntry{
		IssueID:               issueID,
		Identifier:            "fake#gv",
		RunID:                 "run-gv",
		WorkflowState:         "in_progress",
		WorkspacePath:         filepath.Join(wsDir, "fake_gv"),
		StartedAt:             time.Now(),
		TransitionedFromState: "ready",
	}

	c.finishRun(context.Background(), issueID, errPermanentFailure{})
	// The retry guard is scheduled synchronously on the actor.
	if _, ok := c.state.retries[issueID]; !ok {
		t.Fatal("exhausted failure must schedule the optimistic retry guard before the worker runs")
	}
	c.workersWG.Wait()                       // drain the give-up worker
	applyNextCmd(t, c, context.Background()) // apply the cmdDropRetry it posted

	if got := ft.issueState(issueID); got != "blocked" {
		t.Fatalf("issue state = %q after give-up, want blocked", got)
	}
	if _, ok := c.state.retries[issueID]; ok {
		t.Fatal("retry guard not dropped after a successful give-up move")
	}
	// The move alone is ambiguous: an operator filing the ticket by hand
	// writes the very same state. The give-up must also STAMP the ticket, or
	// the pipeline board files an unattended failure as acknowledged history
	// and it never reaches the needs-attention lane (issue #494).
	stamps := ft.giveUpStamps()
	if len(stamps) != 1 || stamps[0] == nil {
		t.Fatalf("give-up stamps = %+v, want exactly one", stamps)
	}
	got := stamps[0]
	if got.RunID != "run-gv" || got.State != "blocked" || got.Attempts != 1 {
		t.Errorf("give-up stamp = %+v, want run-gv/blocked/1 attempt", got)
	}
	if got.At.IsZero() {
		t.Error("give-up stamp carries no timestamp")
	}
}

// TestFinishRun_GiveUpFallsBackToRetryWhenMoveRejected proves the deliberate
// fallback: when the board can't represent FailedState (tracker rejects the
// move), the give-up worker reverts to the source state and KEEPS the
// optimistically-scheduled retry — preserving the legacy unbounded retry
// rather than freezing the ticket. The worker posts no cmdDropRetry.
func TestFinishRun_GiveUpFallsBackToRetryWhenMoveRejected(t *testing.T) {
	ft := newStateAwareTracker()
	ft.updateRejectState = "blocked" // tracker refuses the terminal move
	ft.add(tracker.Issue{ID: "fake:gv2", Identifier: "fake#gv2", Title: "go", WorkflowState: "in_progress"})

	c, wsDir := newStateTestDispatcher(t, &StubRunner{}, ft, time.Hour, "in_progress")
	cfg := c.cfg.Load()
	cfg.Agent.MaxAttempts = 1
	cfg.Agent.FailedState = "blocked"
	c.cfg.Store(cfg)

	issueID := "fake:gv2"
	c.state.running[issueID] = &runningEntry{
		IssueID:               issueID,
		Identifier:            "fake#gv2",
		RunID:                 "run-gv2",
		WorkflowState:         "in_progress",
		WorkspacePath:         filepath.Join(wsDir, "fake_gv2"),
		StartedAt:             time.Now(),
		TransitionedFromState: "ready",
	}

	c.finishRun(context.Background(), issueID, errPermanentFailure{})
	c.workersWG.Wait() // worker: rejected give-up → revert; posts nothing back

	if got := ft.issueState(issueID); got != "ready" {
		t.Fatalf("issue state = %q after rejected give-up, want ready (reverted)", got)
	}
	if _, ok := c.state.retries[issueID]; !ok {
		t.Fatal("retry must be preserved when the give-up move is rejected (board can't represent failed)")
	}
	// A give-up that fell back to retrying has not given up. Stamping here
	// would put a still-retrying ticket in the needs-attention lane and tell
	// the operator a decision was made that was not.
	if stamps := ft.giveUpStamps(); len(stamps) != 0 {
		t.Errorf("give-up stamps = %+v, want none — the terminal move was rejected", stamps)
	}
}

// TestFinishRun_CompletedStateCapturedBeforeReload is the focused regression
// for the Step-3 offload race: a clean finish must capture CompletedState in
// the actor-built finishPlan. A Reload applied after finishRun returns but
// before the off-actor worker performs the completed-state transition must NOT
// retarget this just-finished run.
func TestFinishRun_CompletedStateCapturedBeforeReload(t *testing.T) {
	ft := newStateAwareTracker()
	issueID := "fake:captured-completed"
	ft.add(tracker.Issue{
		ID: issueID, Identifier: "fake#captured-completed",
		Title: "go", WorkflowState: "in_progress",
	})

	c, wsDir := newStateTestDispatcherWithCompleted(t, &StubRunner{}, ft, time.Hour, "in_progress", "review")
	c.state.running[issueID] = &runningEntry{
		IssueID:       issueID,
		Identifier:    "fake#captured-completed",
		RunID:         "run-captured-completed",
		WorkflowState: "in_progress",
		WorkspacePath: filepath.Join(wsDir, "fake_captured-completed"),
		StartedAt:     time.Now(),
	}

	entered := make(chan finishPlan, 1)
	proceed := make(chan struct{})
	defer func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
		c.workersWG.Wait()
	}()
	c.beforeFinishWorker = func(plan finishPlan) {
		entered <- plan
		<-proceed
	}

	c.finishRun(context.Background(), issueID, nil)

	var plan finishPlan
	select {
	case plan = <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("finish worker never reached the pre-transition gate")
	}
	if plan.kind != finishCompleted || plan.completedState != "review" {
		t.Fatalf("finishPlan captured kind/completedState = %v/%q, want finishCompleted/review", plan.kind, plan.completedState)
	}

	// Simulate the actor having processed a Reload after finishRun built the
	// plan but before the worker performs maybeTransitionToCompleted. The old
	// worker path read c.cfg.Load().Agent.CompletedState here and would have
	// moved this just-finished run to "done".
	reloaded := *c.cfg.Load()
	reloaded.Agent.CompletedState = "done"
	c.cfg.Store(&reloaded)

	close(proceed)
	c.workersWG.Wait()

	if got := ft.issueState(issueID); got != "review" {
		t.Fatalf("issue state = %q after post-finish reload, want originally captured CompletedState %q", got, "review")
	}
	calls := ft.calls()
	if len(calls) != 1 || calls[0].newState != "review" {
		t.Fatalf("UpdateState calls = %+v, want exactly one move to captured CompletedState review", calls)
	}
}

// TestDispatch_SkipsUnresolvableExplicitBot asserts the honest-fail
// guard: a ticket naming a bot the registry can't resolve must NOT be
// dispatched against the default workflow (which would run an unrelated
// no-op and report a misleading success). The dispatch is skipped — no
// running entry, no runner call — so the issue stays eligible for retry.
func TestDispatch_SkipsUnresolvableExplicitBot(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{
		ID: "fake:ghost", Identifier: "fake#ghost",
		Title: "go", WorkflowState: "ready", Bot: "no-such-bot-xyz",
	})

	var dispatched atomic.Int64
	runner := &StubRunner{Handler: func(context.Context, DispatchSpec) error {
		dispatched.Add(1)
		return nil
	}}

	dir := t.TempDir()
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	cfg.applyDefaults()
	// Bots.Paths points at an empty dir → "no-such-bot-xyz" is unresolvable.
	cfg.Bots.Paths = []string{filepath.Join(dir, "empty-bots")}
	ws, err := NewWorkspaces(filepath.Join(dir, "ws"))
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
		StoreDir:   filepath.Join(dir, "store"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.dispatch(context.Background(), tracker.Issue{
		ID: "fake:ghost", Identifier: "fake#ghost",
		Title: "go", WorkflowState: "ready", Bot: "no-such-bot-xyz",
	})

	if n := len(c.state.running); n != 0 {
		t.Fatalf("running entries = %d, want 0 (dispatch must be skipped)", n)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("runner.Dispatch called %d times, want 0", dispatched.Load())
	}

	// The skip must be reconcilable from the UI, not just a log line:
	// dispatch() records it in state and buildSnapshot surfaces it so the
	// board / dispatcher dashboard can show WHY the ticket sits idle.
	skip, ok := c.state.dispatchSkips["fake:ghost"]
	if !ok {
		t.Fatalf("dispatch-skip not recorded for the unresolvable bot")
	}
	if skip.Bot != "no-such-bot-xyz" || skip.Reason == "" {
		t.Errorf("skip entry incomplete: %+v", skip)
	}
	if snap := c.buildSnapshot(); len(snap.DispatchSkips) != 1 || snap.DispatchSkips[0].IssueID != "fake:ghost" {
		t.Fatalf("snapshot DispatchSkips = %+v, want one fake:ghost entry", c.buildSnapshot().DispatchSkips)
	}

	// Warn-dedup state is candidate-scoped too: a deleted or terminal ticket
	// must not retain an entry for the daemon lifetime.
	c.state.lastRunHoldWarned["fake:gone"] = "held"
	cmdCandidates{issues: nil}.apply(c, context.Background())
	if _, ok := c.state.lastRunHoldWarned["fake:gone"]; ok {
		t.Fatal("lastRunHoldWarned entry survived after issue left candidates")
	}
}

// pumpCandidates drains the cmdCandidates that tick()'s off-actor discovery
// goroutine posts on c.cmds and applies it on the test goroutine. Since
// ADR-028 Step 2 the candidate sort / dispatch-skip prune / dispatch loop
// runs in cmdCandidates.apply rather than inline in tick(), so a direct
// c.tick(ctx) drive must pump the resulting command to complete the cycle.
// Only valid when no actor goroutine is running (the test owns c.cmds).
func pumpCandidates(t *testing.T, c *Dispatcher, ctx context.Context) {
	t.Helper()
	select {
	case command := <-c.cmds:
		command.apply(c, ctx)
	case <-time.After(10 * time.Second):
		t.Fatal("no cmdCandidates posted by the off-actor discovery goroutine")
	}
}

// TestDispatchSkip_SurfacedThenPruned drives the actor's tick directly:
// an eligible ticket naming an unrouteable bot must surface a
// dispatch-skip entry (so the operator can reconcile the silent stall),
// and that entry must be pruned once the ticket leaves the eligible lane
// so the UI doesn't carry a stale "won't dispatch" badge forever.
func TestDispatchSkip_SurfacedThenPruned(t *testing.T) {
	ft := newFakeTracker()
	ft.add(tracker.Issue{
		ID: "fake:ghost", Identifier: "fake#ghost",
		Title: "go", WorkflowState: "ready", Bot: "no-such-bot-xyz",
	})

	dir := t.TempDir()
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	cfg.applyDefaults()
	cfg.Bots.Paths = []string{filepath.Join(dir, "empty-bots")} // unresolvable
	ws, err := NewWorkspaces(filepath.Join(dir, "ws"))
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
		StoreDir:   filepath.Join(dir, "store"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// Tick 1: the unrouteable ticket is an eligible candidate → skip recorded.
	// Discovery runs off-actor (ADR-028 Step 2), so pump the cmdCandidates it
	// posts to complete the dispatch decision on this goroutine.
	c.tick(ctx)
	pumpCandidates(t, c, ctx)
	snap := c.buildSnapshot()
	if len(snap.DispatchSkips) != 1 || snap.DispatchSkips[0].IssueID != "fake:ghost" {
		t.Fatalf("after tick 1: DispatchSkips = %+v, want one fake:ghost entry", snap.DispatchSkips)
	}
	if snap.DispatchSkips[0].Bot != "no-such-bot-xyz" || snap.DispatchSkips[0].Reason == "" {
		t.Errorf("skip entry lacks bot/reason: %+v", snap.DispatchSkips[0])
	}

	// Operator moves the ticket out of the eligible lane (or fixes it):
	// it's no longer a candidate, so the stale skip must be pruned.
	if err := ft.UpdateState(ctx, "fake:ghost", "blocked"); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	c.tick(ctx)
	pumpCandidates(t, c, ctx)
	if snap := c.buildSnapshot(); len(snap.DispatchSkips) != 0 {
		t.Fatalf("after tick 2: DispatchSkips = %+v, want pruned (empty)", snap.DispatchSkips)
	}
}

// TestDispatch_StampsLastRunAtStart asserts the studio "run ↗" link is
// live for the whole run: the dispatcher must stamp last_run when it
// dispatches, not only when the run finishes. A blocking runner keeps
// the run in flight so the only SetLastRun observed is the dispatch-time
// one (the finish-time stamp can't have fired yet).
func TestDispatch_StampsLastRunAtStart(t *testing.T) {
	ft := newLastRunTracker()
	ft.add(tracker.Issue{
		ID: "fake:dispatch-stamp", Identifier: "fake#dispatch-stamp",
		Title: "go", WorkflowState: "ready",
	})

	started := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(ctx context.Context, _ DispatchSpec) error {
		started <- struct{}{}
		<-ctx.Done() // keep the run "in flight" until the test cancels
		return ctx.Err()
	}}

	dir := t.TempDir()
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 30},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: filepath.Join(dir, "ws")},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(filepath.Join(dir, "ws"))
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
		StoreDir:   filepath.Join(dir, "store"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never started")
	}

	// The run is now blocked in the handler — finishRun cannot have run,
	// so any SetLastRun call must be the dispatch-time stamp.
	calls := ft.lastRunCalls()
	if len(calls) == 0 {
		t.Fatal("expected last_run stamped at dispatch, got none")
	}
	got := calls[0]
	if got.id != "fake:dispatch-stamp" {
		t.Fatalf("stamped issue = %q, want fake:dispatch-stamp", got.id)
	}
	if got.runID == "" {
		t.Fatalf("dispatch-time stamp must carry a runID, got empty")
	}
}

// TestFinishRun_WarnsOnZeroCommit asserts the honesty guard: a clean
// finish whose run produced no commit is logged at WARN (not the
// silent INFO "finished cleanly") so an empty run doesn't masquerade
// as completed work — while still transitioning the issue to
// CompletedState (reverting would loop the dispatcher). A run that
// produced a commit keeps the quiet INFO path.
func TestFinishRun_WarnsOnZeroCommit(t *testing.T) {
	run := func(t *testing.T, finalCommit string) (logs string, finalState string) {
		ft := newFakeTracker()
		dir := t.TempDir()
		wsDir := filepath.Join(dir, "ws")
		storeDir := filepath.Join(dir, "store")
		cfg := &Config{
			Name:      "test",
			Workflow:  t.TempDir() + "/fake.bot",
			Tracker:   TrackerConfig{Kind: "fake"},
			Polling:   PollingConfig{IntervalMS: 50},
			Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
			Workspace: WorkspaceConfig{Root: wsDir},
			Stall:     StallConfig{TimeoutMS: 0},
		}
		cfg.applyDefaults() // CompletedState defaults to "review"
		ws, err := NewWorkspaces(wsDir)
		if err != nil {
			t.Fatalf("NewWorkspaces: %v", err)
		}
		var buf bytes.Buffer
		c, err := New(Options{
			Config:     cfg,
			Tracker:    ft,
			Runner:     &StubRunner{},
			Workspaces: ws,
			Logger:     iterlog.New(iterlog.LevelInfo, &buf),
			HostMarker: "test",
			StoreDir:   storeDir,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		runID := "run-empty-check"
		runDir := filepath.Join(storeDir, "runs", runID)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatalf("mkdir run dir: %v", err)
		}
		payload := map[string]any{"id": runID, "work_dir": filepath.Join(storeDir, "worktrees", runID)}
		if finalCommit != "" {
			payload["final_commit"] = finalCommit
		}
		runJSON, _ := json.Marshal(payload)
		if err := os.WriteFile(filepath.Join(runDir, "run.json"), runJSON, 0o644); err != nil {
			t.Fatalf("write run.json: %v", err)
		}

		issueID := "fake:empty-1"
		ft.add(tracker.Issue{
			ID: issueID, Identifier: "fake#empty-1",
			Title: "go", WorkflowState: "in_progress",
		})
		c.state.running[issueID] = &runningEntry{
			IssueID:       issueID,
			Identifier:    "fake#empty-1",
			RunID:         runID,
			WorkflowState: "in_progress",
			WorkspacePath: filepath.Join(wsDir, "fake_empty-1"),
			StartedAt:     time.Now(),
		}

		c.finishRun(context.Background(), issueID, nil)
		// ADR-028 Step 3: finishRun hands the CompletedState transition to an
		// off-actor finish worker. The honesty-guard WARN/INFO is logged
		// synchronously on the actor (above), but the tracker move is async —
		// drain the finish worker before asserting the issue advanced. The
		// clean path posts nothing back, so Wait() can't deadlock (no actor is
		// draining c.cmds here, and the buffer would absorb a post anyway).
		c.workersWG.Wait()

		states, _ := ft.RefreshStates(context.Background(), []string{issueID})
		return buf.String(), states[issueID]
	}

	t.Run("zero commit warns", func(t *testing.T) {
		logs, state := run(t, "")
		if !strings.Contains(logs, "produced NO commit") {
			t.Fatalf("expected zero-commit WARN, got logs:\n%s", logs)
		}
		// Non-breaking: the issue still advances to CompletedState.
		if state != "review" {
			t.Fatalf("issue state = %q, want %q (transition must still happen)", state, "review")
		}
	})

	t.Run("with commit stays quiet", func(t *testing.T) {
		logs, state := run(t, "deadbeefcafe")
		if strings.Contains(logs, "produced NO commit") {
			t.Fatalf("did not expect zero-commit WARN for a committed run, got logs:\n%s", logs)
		}
		if !strings.Contains(logs, "finished cleanly") {
			t.Fatalf("expected quiet INFO 'finished cleanly', got logs:\n%s", logs)
		}
		if state != "review" {
			t.Fatalf("issue state = %q, want %q", state, "review")
		}
	})
}
