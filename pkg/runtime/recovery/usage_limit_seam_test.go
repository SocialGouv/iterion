package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// This file guards the delegate → Classify → engine-failure SEAM, which no
// other test crosses. Each half was covered in isolation and the join was
// not, which is how the engine came to flatten every terminal failure into
// an EXECUTION_FAILED string: `Classify` correctly returned
// USAGE_LIMIT_BLOCKED, and the code plus the typed cause were both dropped
// on the way out. That made the reset-aware wait in the run-level
// auto-resume loop unreachable — it matches on the typed
// *delegate.ErrRateLimited and on the runtime code, and saw neither.

// usageWindowExecutor fails the target node with the supplied error, always.
type usageWindowExecutor struct {
	target  string
	failErr error
	calls   int
}

func (u *usageWindowExecutor) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() != u.target {
		return map[string]any{}, nil
	}
	u.calls++
	return nil, u.failErr
}

func seamWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "usage_limit_seam",
		Entry: "synthesize",
		Nodes: map[string]ir.Node{
			"synthesize": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "synthesize"}},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "synthesize", To: "done"}},
	}
}

func seamStore(t *testing.T) store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return s
}

// weeklyLimitErr reproduces what the claude_code delegate returns when the
// forfait weekly window is exhausted (the shape that killed seven scheduled
// prod runs on 2026-07-27).
func weeklyLimitErr(resetAt time.Time) error {
	return &delegate.ErrRateLimited{
		Provider: delegate.BackendClaudeCode,
		Detail:   "You've hit your weekly limit · resets Jul 28, 9pm (UTC)",
		Kind:     delegate.RateLimitKindUsageWindow,
		ResetAt:  resetAt,
	}
}

// TestUsageWindowFailure_PreservesCodeAndCause is the seam test: with a
// dispatcher wired, a usage-window failure must reach the host carrying BOTH
// the classified code and the typed cause (with its ResetAt intact), and the
// run must be resumable.
func TestUsageWindowFailure_PreservesCodeAndCause(t *testing.T) {
	resetAt := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	exec := &usageWindowExecutor{target: "synthesize", failErr: weeklyLimitErr(resetAt)}

	s := seamStore(t)
	eng := runtime.New(seamWorkflow(), s, exec,
		runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))

	err := eng.Run(context.Background(), "run-usage-window", nil)
	if err == nil {
		t.Fatal("expected a terminal error from the exhausted usage window")
	}

	// A usage window cannot clear inside a node's retry budget, so the
	// recipe must fail terminally on the first attempt rather than burn
	// retries against the wall.
	if exec.calls != 1 {
		t.Errorf("executor calls = %d, want 1 (no in-node retry against a usage window)", exec.calls)
	}

	var rtErr *runtime.RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected a *runtime.RuntimeError, got %T: %v", err, err)
	}
	if rtErr.Code != runtime.ErrCodeUsageLimitBlocked {
		t.Errorf("code = %s, want %s", rtErr.Code, runtime.ErrCodeUsageLimitBlocked)
	}

	// The load-bearing assertion: the typed cause survives to the host, so
	// a reset-aware retry can read ResetAt instead of guessing.
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("errors.As did not recover *delegate.ErrRateLimited from %v", err)
	}
	if !rl.ResetAt.Equal(resetAt) {
		t.Errorf("ResetAt = %v, want %v", rl.ResetAt, resetAt)
	}
	if rl.Kind != delegate.RateLimitKindUsageWindow {
		t.Errorf("Kind = %q, want %q", rl.Kind, delegate.RateLimitKindUsageWindow)
	}

	// The run must be resumable, and the emitted run_failed event must carry
	// the real code — that event is what an operator and the studio read.
	r, loadErr := s.LoadRun(context.Background(), "run-usage-window")
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %v, want %v", r.Status, store.RunStatusFailedResumable)
	}
	assertRunFailedCode(t, s, "run-usage-window", string(runtime.ErrCodeUsageLimitBlocked))

	// The persisted message keeps the historical wording: the DLQ text, the
	// run doc's error field and operator greps all key on it.
	if want := `node "synthesize" execution failed:`; len(r.Error) < len(want) || r.Error[:len(want)] != want {
		t.Errorf("run error = %q, want it to start with %q", r.Error, want)
	}
}

// TestUsageWindowFailure_NoDispatcherKeepsExecutionFailed pins the other
// half of the contract: a host that wires no dispatcher (as the cloud runner
// did) classifies nothing, and the failure must stay EXECUTION_FAILED rather
// than acquire a code out of thin air.
func TestUsageWindowFailure_NoDispatcherKeepsExecutionFailed(t *testing.T) {
	exec := &usageWindowExecutor{
		target:  "synthesize",
		failErr: weeklyLimitErr(time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)),
	}

	s := seamStore(t)
	eng := runtime.New(seamWorkflow(), s, exec) // no WithRecoveryDispatch

	err := eng.Run(context.Background(), "run-no-dispatch", nil)
	if err == nil {
		t.Fatal("expected a terminal error")
	}
	var rtErr *runtime.RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected a *runtime.RuntimeError, got %T: %v", err, err)
	}
	if rtErr.Code != runtime.ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s", rtErr.Code, runtime.ErrCodeExecutionFailed)
	}
}

// assertRunFailedCode asserts the run_failed event carries the given code.
func assertRunFailedCode(t *testing.T, s store.RunStore, runID, wantCode string) {
	t.Helper()
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, ev := range events {
		if ev.Type != store.EventRunFailed {
			continue
		}
		if got, _ := ev.Data["code"].(string); got != wantCode {
			t.Errorf("run_failed code = %q, want %q", got, wantCode)
		}
		if resumable, _ := ev.Data["resumable"].(bool); !resumable {
			t.Error("run_failed resumable = false, want true")
		}
		return
	}
	t.Fatal("no run_failed event found")
}
