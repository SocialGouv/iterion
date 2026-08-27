package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// sessionPickyBackend fails any task that carries a SessionID and
// succeeds on a fresh one — the shape of a CLI backend whose session
// files died with the sandbox container (a cloud resume replaces the
// container, `claude --resume <id>` then errors on every attempt).
type sessionPickyBackend struct {
	name  string
	fail  error
	tasks []delegate.Task
}

func (b *sessionPickyBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	res := delegate.Result{
		BackendName: b.name,
		Duration:    time.Millisecond,
		Output:      map[string]any{"ok": true},
	}
	if task.SessionID != "" {
		return delegate.Result{BackendName: b.name, Duration: time.Millisecond}, b.fail
	}
	return res, nil
}

func sessionDegradeExecutor(fail error) (*ClawExecutor, *sessionPickyBackend, []chainElement) {
	be := &sessionPickyBackend{name: delegate.BackendClaudeCode, fail: fail}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{})
	return e, be, []chainElement{{Label: "primary"}}
}

func sessionDegradeBuilder(e *ClawExecutor, optional bool) func(context.Context, string) (*delegate.Task, error) {
	return func(_ context.Context, _ string) (*delegate.Task, error) {
		return &delegate.Task{
			NodeID:          "plan_revise",
			Model:           "claude-opus-5",
			SessionID:       "dead-session",
			SessionOptional: optional,
		}, nil
	}
}

// TestOptionalSessionDegradesToFresh pins the inherit_if_available
// contract past the resume boundary: when the upstream session id
// resolves but its backing state no longer loads, the executor retries
// ONCE with the session dropped instead of failing the node forever
// (lived on branch-improve-loop's plan_revise: error_during_execution
// in ~2.6s on every resume, run wedged).
func TestOptionalSessionDegradesToFresh(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(errors.New("delegate: claude-code error: subtype=error_during_execution"))
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	out, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("optional session should degrade to fresh, got: %v", err)
	}
	if len(be.tasks) != 2 {
		t.Fatalf("want 2 attempts (resume, then fresh), got %d", len(be.tasks))
	}
	if be.tasks[0].SessionID != "dead-session" {
		t.Errorf("first attempt should carry the session id, got %q", be.tasks[0].SessionID)
	}
	if be.tasks[1].SessionID != "" || be.tasks[1].ForkSession || be.tasks[1].SessionFingerprint != "" {
		t.Errorf("fresh retry must drop every session field, got %+v", be.tasks[1])
	}
	if got := out.Result.Output["ok"]; got != true {
		t.Errorf("fresh attempt's output lost: %v", out.Result.Output)
	}
}

// TestRequiredSessionDoesNotDegrade: plain `inherit`/`fork` asked for
// continuity unconditionally — a failure keeps failing loudly.
func TestRequiredSessionDoesNotDegrade(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(errors.New("delegate: claude-code error: subtype=error_during_execution"))
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, false))

	if _, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build); err == nil {
		t.Fatal("required session must not degrade to fresh")
	}
	for _, task := range be.tasks {
		if task.SessionID == "" {
			t.Fatal("required session was retried fresh")
		}
	}
}

// TestOptionalSessionAuthFailureDoesNotDegrade: an auth/usage-window
// failure is credential-level — a fresh session hits the same wall, so
// the typed error must surface promptly instead of paying a blind retry.
func TestOptionalSessionAuthFailureDoesNotDegrade(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(&delegate.ErrAuthFailed{Provider: delegate.BackendClaudeCode, Detail: "401"})
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	_, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build)
	if err == nil {
		t.Fatal("auth failure must surface, not degrade")
	}
	var authErr *delegate.ErrAuthFailed
	if !errors.As(err, &authErr) {
		t.Fatalf("typed auth error lost: %v", err)
	}
	for _, task := range be.tasks {
		if task.SessionID == "" {
			t.Fatal("auth failure must not trigger the fresh-session retry")
		}
	}
}

// TestOptionalSessionDegradeEvictsNodeSession: dropping task.SessionID
// only makes the retry fresh for a backend that keys continuity on the
// id. A claw node's conversation lives in the (runID, nodeID) session
// store and is replayed regardless — including the FAILED attempt's
// messages — so the degrade must evict it, exactly as the route-change
// path does, or the "FRESH session" it logs is not fresh at all.
func TestOptionalSessionDegradeEvictsNodeSession(t *testing.T) {
	const runID, nodeID = "run-1", "plan_revise"
	sessions := newNodeSessionStore()
	if err := sessions.SaveSnapshot(runID, nodeID, []byte(`[{"role":"assistant"}]`)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if sessions.LoadSnapshot(runID, nodeID) == nil {
		t.Fatal("precondition: snapshot not stored")
	}
	ctx := withRuntimeContext(context.Background(), runID, sessions)

	e, _, chain := sessionDegradeExecutor(errors.New("delegate: claude-code error: subtype=error_during_execution"))
	build := e.newElementBuilder(nodeID, delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	if _, err := e.dispatchChain(ctx, nodeID, chain, "claude-opus-5", build); err != nil {
		t.Fatalf("optional session should degrade to fresh, got: %v", err)
	}
	if snap := sessions.LoadSnapshot(runID, nodeID); snap != nil {
		t.Errorf("the dead session's conversation survived the degrade: %s", snap)
	}
}

// TestOptionalSessionTransientFailureDoesNotDegrade: a throttle, a 5xx
// or a TCP blip that outlived the retry budget classifies as
// transient_exhausted — a provider-side cause the session id had no
// part in. Degrading there would buy a SECOND full retry budget under a
// provider outage AND discard the node's continuity for nothing
// (R1486ff), so the failure must surface with the session intact.
func TestOptionalSessionTransientFailureDoesNotDegrade(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(&delegate.ErrTransient{Reason: "upstream 503"})
	e.retry.MaxAttempts = 1 // exhaust the budget on the first call
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	if _, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build); err == nil {
		t.Fatal("a transient provider failure must surface, not degrade the session")
	}
	for i, task := range be.tasks {
		if task.SessionID == "" {
			t.Fatalf("attempt %d ran fresh: a provider-side transient must not drop the session", i)
		}
	}
}

// TestOptionalSessionDegradeIsRunVisible: the node SUCCEEDS having lost
// the conversation its `session:` field asked for. A process log line is
// not a run record — without a stamp on the output a downstream
// deterministic gate cannot fail closed on the amnesiac input, and
// without an event nothing in the timeline says the node started blank
// (R051957).
func TestOptionalSessionDegradeIsRunVisible(t *testing.T) {
	var seen []SessionDegradedInfo
	be := &sessionPickyBackend{
		name: delegate.BackendClaudeCode,
		fail: errors.New("delegate: claude-code error: subtype=error_during_execution"),
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{
		OnSessionDegraded: func(_ string, info SessionDegradedInfo) { seen = append(seen, info) },
	})
	chain := []chainElement{{Label: "primary"}}
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	out, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("optional session should degrade to fresh, got: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("want exactly 1 session-degrade event, got %d", len(seen))
	}
	if seen[0].SessionID != "dead-session" || seen[0].BackendName != delegate.BackendClaudeCode {
		t.Errorf("event does not name the session that failed: %+v", seen[0])
	}
	if seen[0].Reason != string(delegate.FallbackUnclassified) || seen[0].Err == nil {
		t.Errorf("event lost the reason/cause: %+v", seen[0])
	}
	if !out.SessionDegraded {
		t.Fatal("chainOutcome does not report the degrade — the output stamp is derived from it")
	}
	stamped := map[string]any{"ok": true}
	stampFallbackMeta(stamped, out)
	if stamped["_session_degraded"] != true {
		t.Errorf("output not stamped _session_degraded: %v", stamped)
	}
	if _, ok := stamped["_fallback_used"]; ok {
		t.Errorf("a session degrade is not a route change — _fallback_used must stay clear: %v", stamped)
	}
}

// A clean pass must carry neither key: an output that stamps
// _session_degraded always means something actually happened.
func TestCleanPassStampsNoSessionDegrade(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(errors.New("unused"))
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "plan_revise", Model: "claude-opus-5"}, nil
		})
	out, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("clean pass failed: %v", err)
	}
	if len(be.tasks) != 1 || out.SessionDegraded {
		t.Fatalf("clean pass reported a degrade: attempts=%d degraded=%v", len(be.tasks), out.SessionDegraded)
	}
	stamped := map[string]any{"ok": true}
	stampFallbackMeta(stamped, out)
	if _, ok := stamped["_session_degraded"]; ok {
		t.Errorf("clean pass stamped _session_degraded: %v", stamped)
	}
}

// cancellingDegradeBackend fails the session-carrying attempt after
// spending real tokens, then cancels the run during the fresh retry.
type cancellingDegradeBackend struct {
	cancel context.CancelFunc
	fail   error
	calls  int
}

func (b *cancellingDegradeBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	b.calls++
	if task.SessionID != "" {
		return delegate.Result{
			BackendName: delegate.BackendClaudeCode,
			Tokens:      500,
			Output:      map[string]any{"_tokens": 500, "_cost_usd": 1.25},
		}, b.fail
	}
	b.cancel()
	return delegate.Result{BackendName: delegate.BackendClaudeCode}, ctx.Err()
}

// TestOptionalSessionDegradeKeepsSpendOnCancel: cancellation is terminal
// for the node but does not un-spend what the abandoned attempt burned.
// The degrade made this reachable on a SINGLE-route node — where the
// accumulator used to be provably empty at the cancel return — so a
// cancel during the fresh retry silently dropped the first attempt's
// tokens and cost from max_cost_usd, the org monthly cap and a lending
// donor's ledger.
func TestOptionalSessionDegradeKeepsSpendOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := &cancellingDegradeBackend{
		cancel: cancel,
		fail:   errors.New("delegate: claude-code error: subtype=error_during_execution"),
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{})
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	out, err := e.dispatchChain(ctx, "plan_revise", []chainElement{{Label: "primary"}}, "claude-opus-5", build)
	if err == nil {
		t.Fatal("a cancelled fresh retry must surface the cancellation")
	}
	if be.calls != 2 {
		t.Fatalf("want 2 attempts (dead session, then fresh), got %d", be.calls)
	}
	if out.Result.Tokens != 500 {
		t.Errorf("the abandoned attempt's tokens were dropped on cancel: %d", out.Result.Tokens)
	}
}
