package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ambiguousBranchErr is a branch failure whose remote effect is undecided.
type ambiguousBranchErr struct{ msg string }

func (e ambiguousBranchErr) Error() string         { return e.msg }
func (e ambiguousBranchErr) AmbiguousEffect() bool { return true }

// convergeWaitAll runs a wait_all convergence over results and returns its
// error.
func convergeWaitAll(t *testing.T, results []*branchResult) error {
	t.Helper()
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"join": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll}}}
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("convergence-order", nil)
	rs.ctx = context.Background()
	_, err := eng.processConvergence(rs, "join", results, nil, nil)
	if err == nil {
		t.Fatal("a wait_all convergence over failed branches did not fail")
	}
	return err
}

// bothOrders is results as given and reversed: the order the branches'
// goroutines finished in.
func bothOrders(results ...*branchResult) [][]*branchResult {
	reversed := make([]*branchResult, len(results))
	for i, r := range results {
		reversed[len(results)-1-i] = r
	}
	return [][]*branchResult{results, reversed}
}

// TestConvergence_quotesTheBranchThatFailedByItself: a branch whose failure
// cancels its siblings is what a wait_all failure quotes, whichever branch
// sorts first and whichever finished first; the cancelled siblings carry no
// verdict of their own. The same branch is the aggregate's CAUSE — errors.Is
// reaches it in both orders, and no sibling cancellation does — so a chain
// walker and a message reader learn the same root cause (#1669). When every
// branch was stopped, a branch that ran out its own deadline is quoted
// before the siblings it cancelled, and the first by id among equals.
func TestConvergence_quotesTheBranchThatFailedByItself(t *testing.T) {
	own := errors.New("runtime: pause branch: pause store unavailable")
	cancelled := fmt.Errorf("%w: %v", ErrRunCancelled, context.Canceled)
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_dispatch_0", err: cancelled},
		&branchResult{branchID: "branch_dispatch_1", err: own},
	) {
		err := convergeWaitAll(t, results)
		if !strings.Contains(err.Error(), own.Error()) {
			t.Fatalf("finished %s first: the failure quotes %q, want the branch that failed by itself", results[0].branchID, err)
		}
		if !errors.Is(err, own) {
			t.Fatalf("finished %s first: errors.Is(%v, root cause) = false, want the aggregate's chain to reach the branch that failed by itself", results[0].branchID, err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrRunCancelled) {
			t.Fatalf("finished %s first: the aggregate's chain reaches the sibling's cancellation (%v), want it subordinated", results[0].branchID, err)
		}
		if cause, canceled := strings.Index(err.Error(), own.Error()), strings.Index(err.Error(), "context canceled"); canceled >= 0 && cause > canceled {
			t.Fatalf("finished %s first: %q names a context canceled before the root cause", results[0].branchID, err)
		}
	}
	timedOut := fmt.Errorf("branch 1: node review: %w", context.DeadlineExceeded)
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_dispatch_0", err: fmt.Errorf("branch 0: %w", context.Canceled)},
		&branchResult{branchID: "branch_dispatch_1", err: timedOut},
	) {
		if err := convergeWaitAll(t, results); !strings.Contains(err.Error(), timedOut.Error()) {
			t.Fatalf("finished %s first, a timeout cancelling its sibling: the failure quotes %q, want the timeout", results[0].branchID, err)
		}
	}
	first := fmt.Errorf("branch 0: %w", context.Canceled)
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_dispatch_0", err: first},
		&branchResult{branchID: "branch_dispatch_1", err: fmt.Errorf("branch 1: %w", ErrRunCancelled)},
	) {
		if err := convergeWaitAll(t, results); !strings.Contains(err.Error(), first.Error()) {
			t.Fatalf("finished %s first, every branch cancelled: the failure quotes %q, want the first by id", results[0].branchID, err)
		}
	}
}

// TestConvergence_doesNotWrapABranchsOwnClassification: a branch failure
// that carries a classification of its own — a typed code, a loop decline,
// a run-level sentinel — never becomes the aggregate's cause: wrapping it
// would let the branch reclassify the run's end through the chain (a death
// beside a ceiling would read as the ceiling). The message still quotes
// that branch; the chain reaches only a failure with nothing to say.
func TestConvergence_doesNotWrapABranchsOwnClassification(t *testing.T) {
	typed := &RuntimeError{
		Code:    ErrCodeNoOutgoingEdge,
		NodeID:  "b2",
		Message: `no outgoing edge from node "b2"`,
		Cause:   &LoopDeclined{Loop: "retry", Reason: "liveness_stall"},
	}
	plain := errors.New("runtime: pause branch: pause store unavailable")
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_split_b1", err: typed},
		&branchResult{branchID: "branch_split_c1", err: plain},
	) {
		err := convergeWaitAll(t, results)
		if !errors.Is(err, plain) {
			t.Fatalf("finished %s first: the chain must reach the plain root cause, got %v", results[0].branchID, err)
		}
		var d *LoopDeclined
		if errors.As(err, &d) {
			t.Fatalf("finished %s first: the chain reaches the sibling's loop decline (%v) — the aggregate would read as the ceiling", results[0].branchID, err)
		}
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeExecutionFailed {
			t.Fatalf("finished %s first: the aggregate's own code = %v, want EXECUTION_FAILED", results[0].branchID, err)
		}
	}
	// When every own-failure is classified, nothing is wrapped: the
	// aggregate keeps its flattened message and no branch's chain answers
	// for it.
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_split_b1", err: typed},
		&branchResult{branchID: "branch_split_c1", err: &RuntimeError{Code: ErrCodeLoopExhausted, NodeID: "c2", Message: `node "c2": loop "fix" exhausted`, Cause: &LoopDeclined{Loop: "fix", Reason: "loop_cap"}}},
	) {
		err := convergeWaitAll(t, results)
		var d *LoopDeclined
		if errors.As(err, &d) {
			t.Fatalf("finished %s first: the chain reaches a branch's loop decline (%v) — a death beside a ceiling reads as the ceiling", results[0].branchID, err)
		}
	}
}

// TestConvergence_runLevelSentinelsStayOffTheChain: a branch failure
// wrapping a sentinel the run's end is classified on — pause detections,
// drain/quota refusals — never becomes the aggregate's cause, so
// errors.Is on the run's end cannot read a branch's stop as the run's
// own. No current code path lets a branch carry one of these; the test is
// what reddens the day a future one does, before a reader (dispatcher,
// runner, launch) misclassifies a fan-out death as a pause or a refusal.
func TestConvergence_runLevelSentinelsStayOffTheChain(t *testing.T) {
	plain := errors.New("runtime: pause branch: pause store unavailable")
	for _, sentinel := range []error{ErrUsageCapped, ErrServerDraining, ErrRunPausedOperator} {
		for _, results := range bothOrders(
			&branchResult{branchID: "branch_a", err: fmt.Errorf("branch a: %w", sentinel)},
			&branchResult{branchID: "branch_b", err: plain},
		) {
			err := convergeWaitAll(t, results)
			if errors.Is(err, sentinel) {
				t.Fatalf("%s via %s first: the aggregate's chain reaches the run-level sentinel — the run's end would classify on a branch's stop", sentinel, results[0].branchID)
			}
			if !errors.Is(err, plain) {
				t.Fatalf("%s via %s first: the plain root cause must be the cause the chain reaches, got %v", sentinel, results[0].branchID, err)
			}
		}
	}
}

// TestConvergence_carriesTheFirstByIDWhateverFinishedFirst: the budget
// refusal a spent-budget failure quotes, and the undecided effect it
// carries, are the first by branch id, not by completion.
func TestConvergence_carriesTheFirstByIDWhateverFinishedFirst(t *testing.T) {
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_a", err: fmt.Errorf("branch a: %w", ErrBudgetExceeded)},
		&branchResult{branchID: "branch_b", err: fmt.Errorf("branch b: %w", ErrBudgetExceeded)},
	) {
		if err := convergeWaitAll(t, results); !strings.Contains(err.Error(), "branch a:") {
			t.Fatalf("finished %s first: the budget failure quotes %q, want branch_a's", results[0].branchID, err)
		}
	}
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_a", err: ambiguousBranchErr{"branch a: POST /deploy timed out"}},
		&branchResult{branchID: "branch_b", err: ambiguousBranchErr{"branch b: POST /deploy timed out"}},
	) {
		var rt *RuntimeError
		if err := convergeWaitAll(t, results); !errors.As(err, &rt) || rt.Code != ErrCodeAmbiguousEffect || rt.Cause == nil || !strings.Contains(rt.Cause.Error(), "branch a:") {
			t.Fatalf("finished %s first: the undecided effect carried is %v, want branch_a's", results[0].branchID, err)
		}
	}
}
