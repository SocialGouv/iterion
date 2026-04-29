package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// flakyExecutor fails the first `failures` Executions of `nodeID` with
// the supplied error and succeeds afterwards. Used to exercise the
// engine's retry path under a recovery dispatcher.
type flakyExecutor struct {
	target   string
	failErr  error
	failures int
	calls    int
}

func (f *flakyExecutor) Execute(_ context.Context, node ir.Node, _ map[string]interface{}) (map[string]interface{}, error) {
	if node.NodeID() != f.target {
		return map[string]interface{}{}, nil
	}
	f.calls++
	if f.calls <= f.failures {
		return nil, f.failErr
	}
	return map[string]interface{}{"ok": true}, nil
}

func newRecoveryWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "recovery_test",
		Entry: "agent_a",
		Nodes: map[string]ir.Node{
			"agent_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "agent_a", To: "done"},
		},
	}
}

func TestRecoveryDispatch_RetriesUntilSuccess(t *testing.T) {
	exec := &flakyExecutor{
		target:   "agent_a",
		failErr:  &RuntimeError{Code: ErrCodeRateLimited, Message: "throttled"},
		failures: 2,
	}

	dispatchCalls := 0
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		dispatchCalls++
		return RecoveryAction{Kind: RecoveryRetrySameNode, Delay: 0}, ErrCodeRateLimited
	})

	eng := New(newRecoveryWorkflow(), tmpStore(t), exec, WithRecoveryDispatch(dispatch))
	if err := eng.Run(context.Background(), "run-1", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.calls != 3 {
		t.Errorf("expected 3 executions (2 fails + 1 success), got %d", exec.calls)
	}
	if dispatchCalls != 2 {
		t.Errorf("expected dispatch consulted once per failure (2×), got %d", dispatchCalls)
	}
}

func TestRecoveryDispatch_PriorAttemptsResolverProgresses(t *testing.T) {
	exec := &flakyExecutor{target: "agent_a", failErr: &RuntimeError{Code: ErrCodeRateLimited}, failures: 2}

	seen := []int{}
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		seen = append(seen, prior(ErrCodeRateLimited))
		return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeRateLimited
	})

	eng := New(newRecoveryWorkflow(), tmpStore(t), exec, WithRecoveryDispatch(dispatch))
	if err := eng.Run(context.Background(), "run-2", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 2 || seen[0] != 0 || seen[1] != 1 {
		t.Errorf("expected priorAttempts to progress 0→1, got %v", seen)
	}
}

func TestRecoveryDispatch_FailTerminalCallsCheckpoint(t *testing.T) {
	exec := &flakyExecutor{
		target:   "agent_a",
		failErr:  &RuntimeError{Code: ErrCodeToolFailedPermanent, Message: "missing tool"},
		failures: 99, // never recovers
	}

	dispatch := RecoveryDispatch(func(_ context.Context, _ error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		return RecoveryAction{Kind: RecoveryFailTerminal, Reason: "permanent"}, ErrCodeToolFailedPermanent
	})

	s := tmpStore(t)
	eng := New(newRecoveryWorkflow(), s, exec, WithRecoveryDispatch(dispatch))
	err := eng.Run(context.Background(), "run-3", nil)
	if err == nil {
		t.Fatal("expected terminal error")
	}
	if exec.calls != 1 {
		t.Errorf("expected exactly 1 execution before terminal fail, got %d", exec.calls)
	}
	// Run should be persisted as failed_resumable (checkpoint preserved).
	r, _ := s.LoadRun("run-3")
	if r == nil || r.Status != store.RunStatusFailedResumable {
		t.Errorf("expected failed_resumable status, got %v", r)
	}
}

func TestRecoveryDispatch_PauseForHumanProducesInteraction(t *testing.T) {
	exec := &flakyExecutor{
		target:   "agent_a",
		failErr:  &RuntimeError{Code: ErrCodeBudgetExceeded},
		failures: 99,
	}

	dispatch := RecoveryDispatch(func(_ context.Context, _ error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		return RecoveryAction{Kind: RecoveryPauseForHuman, Reason: "budget exhausted"}, ErrCodeBudgetExceeded
	})

	s := tmpStore(t)
	eng := New(newRecoveryWorkflow(), s, exec, WithRecoveryDispatch(dispatch))
	err := eng.Run(context.Background(), "run-4", nil)
	if err != ErrRunPaused {
		t.Fatalf("expected ErrRunPaused, got %v", err)
	}
	r, _ := s.LoadRun("run-4")
	if r == nil || r.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("expected paused_waiting_human, got %v", r)
	}
	if r.Checkpoint == nil || r.Checkpoint.InteractionID == "" {
		t.Fatal("expected synthetic recovery interaction in checkpoint")
	}
	interaction, err := s.LoadInteraction("run-4", r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("load interaction: %v", err)
	}
	if _, ok := interaction.Questions["acknowledge_recovery"]; !ok {
		t.Errorf("expected synthetic question 'acknowledge_recovery', got %+v", interaction.Questions)
	}
}

// TestRecoveryDispatch_PreservesNodeAttemptsAcrossResume verifies that
// the per-(node, code) attempt bucket survives a fail/resume cycle.
// Without checkpoint persistence the bucket would reset to 0 on resume
// and the dispatcher would see prior=0 again, defeating the
// retry-budget contract.
func TestRecoveryDispatch_PreservesNodeAttemptsAcrossResume(t *testing.T) {
	exec := &flakyExecutor{
		target:   "agent_a",
		failErr:  &RuntimeError{Code: ErrCodeRateLimited, Message: "throttled"},
		failures: 99,
	}

	priorSeen := []int{}
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		priorSeen = append(priorSeen, prior(ErrCodeRateLimited))
		return RecoveryAction{Kind: RecoveryFailTerminal, Reason: "stop"}, ErrCodeRateLimited
	})

	s := tmpStore(t)
	eng := New(newRecoveryWorkflow(), s, exec, WithRecoveryDispatch(dispatch))

	// Run 1: first failure → prior=0, then FailTerminal → failed_resumable
	// with checkpoint preserved.
	if err := eng.Run(context.Background(), "run-resume", nil); err == nil {
		t.Fatal("run 1 expected terminal error, got nil")
	}
	if len(priorSeen) != 1 || priorSeen[0] != 0 {
		t.Fatalf("run 1 prior view: want [0], got %v", priorSeen)
	}
	r, _ := s.LoadRun("run-resume")
	if r == nil || r.Status != store.RunStatusFailedResumable {
		t.Fatalf("expected failed_resumable, got %+v", r)
	}
	if got := r.Checkpoint.NodeAttempts["agent_a"][string(ErrCodeRateLimited)]; got != 1 {
		t.Fatalf("after run 1 want bucket=1, got %d", got)
	}

	// Resume: re-executes agent_a; a second failure should now show
	// prior=1, proving the bucket was restored from the checkpoint.
	// Without H1's fix the dispatcher would observe prior=0 again.
	if err := eng.Resume(context.Background(), "run-resume", nil); err == nil {
		t.Fatal("resume expected terminal error, got nil")
	}
	if len(priorSeen) != 2 || priorSeen[1] != 1 {
		t.Fatalf("resume prior view: want [0 1], got %v", priorSeen)
	}
	r, _ = s.LoadRun("run-resume")
	if got := r.Checkpoint.NodeAttempts["agent_a"][string(ErrCodeRateLimited)]; got != 2 {
		t.Errorf("after resume want bucket=2, got %d", got)
	}
}

func TestRecoveryDispatch_RespectsContextCancelDuringDelay(t *testing.T) {
	exec := &flakyExecutor{target: "agent_a", failErr: &RuntimeError{Code: ErrCodeRateLimited}, failures: 99}
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		return RecoveryAction{Kind: RecoveryRetrySameNode, Delay: 5 * time.Second}, ErrCodeRateLimited
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	eng := New(newRecoveryWorkflow(), tmpStore(t), exec, WithRecoveryDispatch(dispatch))
	err := eng.Run(ctx, "run-5", nil)
	if err == nil {
		t.Fatal("expected cancellation-related error")
	}
}
