package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// convergeAt runs the join of results at a node awaiting in mode, on a run
// state the caller can read the merged outputs from.
func convergeAt(t *testing.T, mode ir.AwaitMode, results []*branchResult) (*runState, error) {
	t.Helper()
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"join": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: mode}}}
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("converge", nil)
	rs.ctx = context.Background()
	_, err := eng.processConvergence(rs, "join", results, nil, nil)
	return rs, err
}

// Every item of a `fan_out_each` replays one template, so its branches
// publish under ONE node id. Which of them a later `outputs.<node>` reads is
// decided by branch id, never by which goroutine finished last — built here
// with the results in both orders.
func TestAJoinMergesCollidingBranchOutputsInBranchOrder(t *testing.T) {
	branches := func() (first, last *branchResult) {
		return &branchResult{branchID: "branch_each_0", outputs: map[string]map[string]any{"work": {"a": 1, "s": "one"}}},
			&branchResult{branchID: "branch_each_1", outputs: map[string]map[string]any{"work": {"s": "two"}}}
	}
	want := map[string]any{"s": "two"}
	for _, finishedLast := range []string{"branch_each_0", "branch_each_1"} {
		first, last := branches()
		results := []*branchResult{first, last}
		if finishedLast == first.branchID {
			results = []*branchResult{last, first}
		}
		rs, err := convergeAt(t, ir.AwaitWaitAll, results)
		if err != nil {
			t.Fatalf("%s finished last: %v", finishedLast, err)
		}
		if got := rs.outputs["work"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s finished last: outputs.work = %v, want the last branch by id %v", finishedLast, got, want)
		}
	}
}

// A wait_all join one branch failed on the tool-node contract says THAT:
// the refusal and its remedy are what the run's error carries, whatever the
// other branches failed on and in whichever order they are handed over, and
// the typed refusal is still in the chain.
func TestAJoinRefusedByTheToolContractQuotesThatBranchAndWrapsIt(t *testing.T) {
	refused := func() error {
		return fmt.Errorf("node %q in branch %s: %w", "publish", "branch_z", &model.ToolExecutionError{
			Cause: fmt.Errorf("model: tool node %q: %w", "publish", &model.ToolStdoutNotJSONError{Schema: "publish_out", Bytes: 92}),
		})
	}
	lint := func() error {
		return fmt.Errorf("node %q in branch %s: %w", "lint", "branch_a", &model.ToolExecutionError{
			Cause: errors.New(`model: tool node "lint": shell command failed: exit status 1`),
		})
	}
	for _, order := range []string{"the refused branch last", "the refused branch first"} {
		results := []*branchResult{{branchID: "branch_a", err: lint()}, {branchID: "branch_z", err: refused()}}
		if order == "the refused branch first" {
			results[0], results[1] = results[1], results[0]
		}
		_, err := convergeAt(t, ir.AwaitWaitAll, results)
		var rerr *RuntimeError
		if !errors.As(err, &rerr) || rerr.Code != ErrCodeToolFailedPermanent {
			t.Fatalf("%s: err = %v, want %s", order, err, ErrCodeToolFailedPermanent)
		}
		for _, want := range []string{"stdout is not a JSON object", "publish_out", "branch_z", "2 branch(es) failed"} {
			if !strings.Contains(rerr.Message, want) {
				t.Errorf("%s: the message does not say %q: %s", order, want, rerr.Message)
			}
		}
		if strings.Contains(rerr.Message, "exit status 1") {
			t.Errorf("%s: the message quotes the sibling's failure instead: %s", order, rerr.Message)
		}
		var violation model.ToolContractViolation
		if !errors.As(err, &violation) {
			t.Errorf("%s: the typed refusal is not in the chain of %v", order, err)
		}
	}
}
