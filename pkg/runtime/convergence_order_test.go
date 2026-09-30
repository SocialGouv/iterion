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
// verdict of their own. When every branch was stopped, the first by id is.
func TestConvergence_quotesTheBranchThatFailedByItself(t *testing.T) {
	own := errors.New("runtime: pause branch: pause store unavailable")
	cancelled := fmt.Errorf("%w: %v", ErrRunCancelled, context.Canceled)
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_dispatch_0", err: cancelled},
		&branchResult{branchID: "branch_dispatch_1", err: own},
	) {
		if err := convergeWaitAll(t, results); !strings.Contains(err.Error(), own.Error()) {
			t.Fatalf("finished %s first: the failure quotes %q, want the branch that failed by itself", results[0].branchID, err)
		}
	}
	first := fmt.Errorf("branch 0: %w", context.Canceled)
	for _, results := range bothOrders(
		&branchResult{branchID: "branch_dispatch_0", err: first},
		&branchResult{branchID: "branch_dispatch_1", err: fmt.Errorf("branch 1: %w", context.DeadlineExceeded)},
	) {
		if err := convergeWaitAll(t, results); !strings.Contains(err.Error(), first.Error()) {
			t.Fatalf("finished %s first, every branch stopped: the failure quotes %q, want the first by id", results[0].branchID, err)
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
