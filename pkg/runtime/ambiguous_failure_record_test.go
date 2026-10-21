package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// doneRefusingStore refuses every status write on a done context, as a store
// that honours cancellation (Mongo) does.
type doneRefusingStore struct{ store.RunStore }

func (s doneRefusingStore) UpdateRunStatusCoded(ctx context.Context, id string, status store.RunStatus, runErr string, code store.FailureCode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.RunStore.UpdateRunStatusCoded(ctx, id, status, runErr, code)
}

// A failure that lands with a cancel is recorded all the same, under the code
// its chain carries: an undecided effect behind a subbot boundary's catch-all
// is recorded as one, and a run left `running` would be redelivered.
func TestAFailureOnADoneContextIsRecordedUnderItsChainsCode(t *testing.T) {
	s := doneRefusingStore{tmpStore(t)}
	if _, err := s.CreateRun(context.Background(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	eng := New(&ir.Workflow{}, s, newStubExecutor())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	behindASubbot := &RuntimeError{
		Code:    ErrCodeExecutionFailed,
		Message: "subbot child failed",
		Cause:   &sandbox.ExecStatusUnknownError{Swept: false, Detail: "the sweep could not finish"},
	}
	err := eng.failRunErr(ctx, "r", "gate", behindASubbot)
	if strings.Contains(err.Error(), "could not persist") {
		t.Fatalf("a failure on a done context was not recorded: %v", err)
	}
	r, lerr := s.LoadRun(context.Background(), "r")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if r.Status != store.RunStatusFailed || r.FailureCode != ErrCodeAmbiguousEffect {
		t.Fatalf("recorded %s / %q, want failed / %q", r.Status, r.FailureCode, ErrCodeAmbiguousEffect)
	}
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeAmbiguousEffect {
		t.Fatalf("the returned failure does not carry the recorded code: %v", err)
	}
}

// A best_effort join stops on a branch whose effect is undecided, as wait_all
// does: what follows the join would rely on it.
func TestBestEffortConvergenceStopsOnAnAmbiguousBranch(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"join": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort}}}
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("best-effort", nil)
	rs.ctx = context.Background()
	results := []*branchResult{
		{branchID: "a", err: ambiguousBranchErr{msg: "a's command may still be running"}},
		{branchID: "b", outputs: map[string]map[string]any{"b": {"ok": true}}},
	}
	_, err := eng.processConvergence(rs, "join", results, nil, nil)
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeAmbiguousEffect {
		t.Fatalf("a best_effort join over an ambiguous branch: %v, want %s", err, ErrCodeAmbiguousEffect)
	}
}
