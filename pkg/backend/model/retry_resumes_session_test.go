package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// foldSameSession answers where the work RAN, not where the task asked it
// to run. Reading the task alone was right until a retry could resume a
// session the task never carried — which is exactly what an in-process
// retry now does, and where a cumulative report would be billed twice.
func TestFoldSameSessionReadsWhereTheWorkRan(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prev, next delegate.Result
		shared     bool
		want       bool
	}{
		{"both name the same session", delegate.Result{SessionID: "s1"}, delegate.Result{SessionID: "s1"}, false, true},
		{"both name DIFFERENT sessions", delegate.Result{SessionID: "s1"}, delegate.Result{SessionID: "s2"}, true, false},
		{"neither names one: the task answers", delegate.Result{}, delegate.Result{}, true, true},
		{"neither names one, and the task shares none", delegate.Result{}, delegate.Result{}, false, false},
		{"only one names one: the task still answers", delegate.Result{SessionID: "s1"}, delegate.Result{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldSameSession(tc.prev, tc.next, tc.shared); got != tc.want {
				t.Errorf("foldSameSession = %v, want %v", got, tc.want)
			}
		})
	}
}

// The measured shape, 2026-09-08: a delegate died 46 minutes into a node,
// node_recovery retried it two seconds later IN THE SAME POD (2
// node_started, 1 sandbox_started), and the node began again from zero —
// throwing away context that was still on the pod's disk.
func TestInProcessRetryResumesTheSessionTheDeadAttemptOpened(t *testing.T) {
	e := newTestExecutorForRetry(3)
	var seen []delegate.Task
	task := &delegate.Task{NodeID: "n"}
	calls := 0

	got, err := e.retryDelegateLoopChain(context.Background(), "n", "claude_code", sharesSession(task), nil,
		func() (delegate.Result, error) {
			calls++
			seen = append(seen, *task)
			// Attempt 1 opens a session and dies with it named (the
			// enabling half); attempt 2 RESUMES it. Tokens are per turn on
			// every shipped backend, but the CLI's cost figure is the
			// session's running total — CostIsSessionTotal says which.
			if calls == 1 {
				return delegate.Result{SessionID: "s-live", Tokens: 9000, CostIsSessionTotal: true,
					Output: map[string]any{"_tokens": 9000, "_cost_usd": 0.42}}, &delegate.ErrTransient{Reason: "stream closed"}
			}
			return delegate.Result{SessionID: "s-live", Tokens: 1300, CostIsSessionTotal: true,
				Output: map[string]any{"_tokens": 1300, "_cost_usd": 0.55}}, nil
		},
		func(prev delegate.Result) {
			if task.SessionID == "" && prev.SessionID != "" {
				task.SessionID = prev.SessionID
				task.SessionOptional = true
			}
		})
	if err != nil || calls != 2 {
		t.Fatalf("want one retry then success: err=%v calls=%d", err, calls)
	}
	if seen[0].SessionID != "" {
		t.Fatalf("the FIRST attempt must open its own session: %q", seen[0].SessionID)
	}
	if seen[1].SessionID != "s-live" {
		t.Fatalf("the retry started over instead of resuming: SessionID=%q", seen[1].SessionID)
	}
	if !seen[1].SessionOptional {
		t.Error("the resumed session must be OPTIONAL — if it cannot be served, the existing degrade path must take over and say so, not fail the node forever")
	}
	// And the coupling that would otherwise bill twice in silence. The TASK
	// carried no session — the carry gave it one — so sharesSession() says
	// "not shared" and the session-total cost would be SUMMED onto itself.
	// foldSameSession reads the results instead and sees one session.
	if got.Tokens != 10300 {
		t.Fatalf("tokens = %d, want 10300 — per-turn reports add up", got.Tokens)
	}
	if usd := cost.USDFromOutput(got.Output); usd != 0.55 {
		t.Fatalf("cost = %v, want 0.55 — the resumed report is the session's total, not a second bill", usd)
	}
}

// resumeScriptedBackend fails its first call naming the session it opened,
// then succeeds — and records the task it was handed each time.
type resumeScriptedBackend struct {
	name  string
	tasks []delegate.Task
}

func (b *resumeScriptedBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	res := delegate.Result{BackendName: b.name, SessionID: "s-live", Tokens: 100,
		Output: map[string]any{"served_by": b.name}}
	if len(b.tasks) == 1 {
		return res, &delegate.ErrTransient{Reason: "stream closed"}
	}
	return res, nil
}

// The WIRING, which the fold's own tests cannot show: the main dispatch must
// hand the retry loop a carry-forward that resumes. A mutant dropping it at
// the call site leaves every other test in this file green while the node
// goes on starting over from zero — the defect this exists to close.
func TestDispatchWiresTheResumingRetry(t *testing.T) {
	be := &resumeScriptedBackend{name: delegate.BackendClaudeCode}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	if _, err := e.dispatchChain(context.Background(), "review",
		[]chainElement{{Label: "primary"}}, "claude-opus-5", build); err != nil {
		t.Fatalf("the second attempt succeeds: %v", err)
	}
	if len(be.tasks) != 2 {
		t.Fatalf("want one retry, got %d attempts", len(be.tasks))
	}
	if be.tasks[0].SessionID != "" {
		t.Fatalf("the first attempt must open its own session: %q", be.tasks[0].SessionID)
	}
	if be.tasks[1].SessionID != "s-live" || !be.tasks[1].SessionOptional {
		t.Fatalf("the retry did not resume what the dead attempt opened: SessionID=%q optional=%v",
			be.tasks[1].SessionID, be.tasks[1].SessionOptional)
	}
}

// poisonedSessionBackend fails TRANSIENTLY every time it is handed a
// session — the shape that matters, because `transient (network)` is
// exactly what the measured failure classified as, and exactly the
// category the executor's degrade path excludes by construction.
type poisonedSessionBackend struct {
	name  string
	tasks []delegate.Task
}

func (b *poisonedSessionBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	res := delegate.Result{BackendName: b.name, SessionID: "s-poison", Tokens: 10,
		Output: map[string]any{"served_by": b.name}}
	if task.SessionID != "" {
		return res, &delegate.ErrTransient{Reason: "claude session ended without result message: connection reset by peer"}
	}
	if len(b.tasks) == 1 {
		return res, &delegate.ErrTransient{Reason: "stream closed: connection reset by peer"}
	}
	return delegate.Result{BackendName: b.name, Tokens: 10, Output: map[string]any{"served_by": b.name}}, nil
}

// A CARRIED session is opportunistic: its status quo ante is a fresh start,
// so it gets ONE chance. If the attempt that resumed it dies too, the
// session is a suspect and the remaining budget goes to a clean attempt.
//
// The executor's degrade path cannot do this job — it is gated on
// UNCLASSIFIED, and the failure this whole change was measured on
// classifies as `transient (network)`. Without the one-chance rule a
// poisoned carried session is re-loaded on EVERY attempt, silently,
// burning the retry budget with the only guard that would drop it switched
// off for its category.
func TestACarriedSessionGetsOneChanceThenTheBudgetGoesToACleanAttempt(t *testing.T) {
	be := &poisonedSessionBackend{name: delegate.BackendClaudeCode}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{})
	// Three attempts, so the budget outlives the one chance the carried
	// session gets — with two, the carry consumes the LAST attempt and the
	// rule this test exists for is never reached.
	e.retry.MaxAttempts = 3

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	_, err := e.dispatchChain(context.Background(), "review",
		[]chainElement{{Label: "primary"}}, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("the clean third attempt must succeed: %v (attempts=%d)", err, len(be.tasks))
	}
	if len(be.tasks) < 3 {
		t.Fatalf("want at least three attempts, got %d", len(be.tasks))
	}
	if be.tasks[0].SessionID != "" {
		t.Fatalf("attempt 1 opens its own session: %q", be.tasks[0].SessionID)
	}
	if be.tasks[1].SessionID != "s-poison" {
		t.Fatalf("attempt 2 must take the one chance: %q", be.tasks[1].SessionID)
	}
	if be.tasks[2].SessionID != "" {
		t.Fatalf("attempt 3 re-loaded a session that had just killed attempt 2: %q — the budget burns on it and session_degraded never fires for this category", be.tasks[2].SessionID)
	}
	if be.tasks[2].SessionOptional {
		t.Error("the optional flag outlived the id it qualifies")
	}
}

// A fall-through starts a fresh conversation on ANOTHER backend, and the
// builder clears the session id for that reason. `SessionOptional`
// qualifies that id and must go with it: while only the declared modes set
// the flag it was unreachable, but a retry that carries a session forward
// is a SECOND writer, so a stale `true` can now ride into an element that
// was handed no session at all.
func TestFallThroughClearsTheOptionalFlagWithTheSessionItQualifies(t *testing.T) {
	// SAME backend on both elements — a provider chain, the common shape —
	// because the builder CACHES one task per backend NAME
	// (executor_retry.go: `tasks := map[string]*delegate.Task{}`). Fall
	// through to a DIFFERENT backend and `assemble` hands out a fresh
	// task, where the reset is invisible: the first version of this test
	// did exactly that and a mutant deleting the reset stayed green.
	be := &poisonedSessionBackend{name: delegate.BackendClaudeCode}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review"}, nil
		})
	_, _ = e.dispatchChain(context.Background(), "review", []chainElement{
		{Label: "anthropic", Provider: "anthropic"},
		{Label: "zai", Provider: "zai"},
	}, "claude-opus-5", build)

	if len(be.tasks) < 3 {
		t.Fatalf("want the head to retry then fall through: %d attempts", len(be.tasks))
	}
	if be.tasks[1].SessionID != "s-poison" {
		t.Fatalf("the head must have carried its session before falling through: %+v", be.tasks[1])
	}
	last := be.tasks[len(be.tasks)-1]
	if last.SessionID != "" {
		t.Fatalf("a session survived the fall-through: %q", last.SessionID)
	}
	if last.SessionOptional {
		t.Error("the optional flag survived the fall-through without the id it qualifies — a stale true rides into an element handed no session at all")
	}
}
