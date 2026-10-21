package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// Any other failure that lands with a cancel keeps the cancel's own handling:
// it is not written on a detached context over the status the publisher
// wrote, nor over a drained run its next runner adopts.
func TestAnOrdinaryFailureOnADoneContextIsNotDetached(t *testing.T) {
	s := doneRefusingStore{tmpStore(t)}
	if _, err := s.CreateRun(context.Background(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	eng := New(&ir.Workflow{}, s, newStubExecutor())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := eng.failRunErr(ctx, "r", "n", &RuntimeError{Code: ErrCodeExecutionFailed, Message: "fan-out stopped"})
	if err == nil || !strings.Contains(err.Error(), "could not persist") {
		t.Fatalf("an ordinary failure on a done context was written: %v", err)
	}
	var rt *RuntimeError
	if errors.As(err, &rt) {
		t.Fatalf("an unrecorded ordinary failure returned its runtime error, which reads as recorded: %v", err)
	}
	r, lerr := s.LoadRun(context.Background(), "r")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if r.Status == store.RunStatusFailed {
		t.Fatalf("an ordinary failure on a done context overwrote the run's status: %s", r.Status)
	}
}

// oneNodeWorkflow is first, then done.
func oneNodeWorkflow(first ir.Node) *ir.Workflow {
	return &ir.Workflow{
		Name:    "one_node",
		Entry:   first.NodeID(),
		Nodes:   map[string]ir.Node{first.NodeID(): first, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Edges:   []*ir.Edge{{From: first.NodeID(), To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// ctxNodeExecutor runs node "a" with its context; the others are stubs.
type ctxNodeExecutor struct {
	*stubExecutor
	a func(ctx context.Context) (map[string]any, error)
}

func (x ctxNodeExecutor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	if node.NodeID() == "a" {
		return x.a(ctx)
	}
	return x.stubExecutor.Execute(ctx, node, input)
}

// An undecided end that lands with the run's budget deadline is recorded as
// an undecided effect, not as a budget wall a raised cap would resume.
func TestAnUndecidedEndAtTheBudgetDeadlineIsRecordedAsOne(t *testing.T) {
	wf := oneNodeWorkflow(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}})
	wf.Budget = &ir.Budget{MaxDuration: "300ms"}
	x := ctxNodeExecutor{stubExecutor: newStubExecutor(), a: func(ctx context.Context) (map[string]any, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("model: tool node %q: shell command failed: %w", "a", errors.Join(
			fmt.Errorf("the node ended before its command's end was proven: %w", ctx.Err()),
			&sandbox.ExecStatusUnknownError{Swept: false, Detail: "the sweep could not finish"}))
	}}
	s := tmpStore(t)
	_ = New(wf, s, x).Run(context.Background(), "r", nil)
	r, err := s.LoadRun(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if r.FailureCode != ErrCodeAmbiguousEffect {
		t.Fatalf("an undecided end at the budget deadline was recorded %s / %q, want %q", r.Status, r.FailureCode, ErrCodeAmbiguousEffect)
	}
}

// A wait_all join whose other branch hit the budget does not read an undecided
// branch as one that merely stopped: the join is an undecided effect.
func TestWaitAllBudgetPrecedenceDoesNotMaskAnUndecidedBranch(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"join": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll}}}
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("wait-all", nil)
	rs.ctx = context.Background()
	undecided := fmt.Errorf("model: tool node %q: shell command failed: %w", "a", errors.Join(
		fmt.Errorf("the node ended before its command's end was proven: %w", context.Canceled),
		&sandbox.ExecStatusUnknownError{Swept: false}))
	results := []*branchResult{
		{branchID: "a", err: undecided},
		{branchID: "b", err: fmt.Errorf("%w: cost_usd (12/10)", ErrBudgetExceeded)},
	}
	_, err := eng.processConvergence(rs, "join", results, nil, nil)
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeAmbiguousEffect {
		t.Fatalf("a wait_all join over an undecided branch and a budget refusal: %v, want %s", err, ErrCodeAmbiguousEffect)
	}
}

// statusRefusingStore refuses every status write, a store that is down.
type statusRefusingStore struct{ store.RunStore }

func (s statusRefusingStore) UpdateRunStatusCoded(context.Context, string, store.RunStatus, string, store.FailureCode) error {
	return errors.New("store down")
}

// A failure the store could not record returns the store error alone — the
// caller must not read it as recorded — except an undecided effect, whose
// code is returned beside it so the caller does not re-drive the run.
func TestAnUnrecordedFailureCarriesItsCodeOnlyWhenUndecided(t *testing.T) {
	eng := New(&ir.Workflow{}, statusRefusingStore{tmpStore(t)}, newStubExecutor())
	var rt *RuntimeError
	if err := eng.failRun(context.Background(), "r", "n", "exit status 1"); errors.As(err, &rt) || !strings.Contains(err.Error(), "could not persist") {
		t.Fatalf("an unrecorded failRun: %v, want the store error alone", err)
	}
	ordinary := &RuntimeError{Code: ErrCodeExecutionFailed, Message: "exit status 1"}
	if err := eng.failRunErr(context.Background(), "r", "n", ordinary); errors.As(err, &rt) || !strings.Contains(err.Error(), "could not persist") {
		t.Fatalf("an unrecorded ordinary failRunErr: %v, want the store error alone", err)
	}
	undecided := &RuntimeError{Code: ErrCodeExecutionFailed, Message: "lost stream", Cause: &sandbox.ExecStatusUnknownError{Swept: false}}
	err := eng.failRunErr(context.Background(), "r", "n", undecided)
	if !errors.As(err, &rt) || rt.Code != ErrCodeAmbiguousEffect || !strings.Contains(err.Error(), "could not persist") {
		t.Fatalf("an unrecorded undecided effect: %v, want its code beside the store error", err)
	}
}

// doneRefusingResumableStore refuses a resumable failure's write on a done
// context too.
type doneRefusingResumableStore struct{ doneRefusingStore }

func (s doneRefusingResumableStore) FailRunResumable(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.doneRefusingStore.FailRunResumable(ctx, id, cp, runErr, code)
}

// The checkpointed writer records an undecided effect that lands with a
// cancel as well, with its checkpoint and under its own code.
func TestAnUndecidedCheckpointedFailureOnADoneContextIsRecorded(t *testing.T) {
	s := doneRefusingResumableStore{doneRefusingStore{tmpStore(t)}}
	if _, err := s.CreateRun(context.Background(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	eng := New(&ir.Workflow{}, s, newStubExecutor())
	rs := eng.newRunState("r", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rs.ctx = ctx
	undecided := &RuntimeError{Code: ErrCodeExecutionFailed, Message: "lost stream", Cause: &sandbox.ExecStatusUnknownError{Swept: false}}
	if err := eng.failRunErrWithCheckpoint(rs, "gate", undecided); strings.Contains(err.Error(), "could not persist") {
		t.Fatalf("an undecided checkpointed failure on a done context was not recorded: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != ErrCodeAmbiguousEffect {
		t.Fatalf("recorded %s / %q, want failed_resumable / %q with its checkpoint", r.Status, r.FailureCode, ErrCodeAmbiguousEffect)
	}
}

// pauseSiblingExecutor runs a fan-out whose branch "tool" ends undecided once
// the fan-out cancels it, and whose branch "barrier" lets the sibling gate
// pause only after "tool" started.
type pauseSiblingExecutor struct {
	*stubExecutor
	started chan struct{}
	once    sync.Once
}

func (x *pauseSiblingExecutor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	switch node.NodeID() {
	case "tool":
		x.once.Do(func() { close(x.started) })
		<-ctx.Done()
		return nil, fmt.Errorf("model: tool node %q: shell command failed: %w", "tool", errors.Join(
			fmt.Errorf("the node ended before its command's end was proven: %w", ctx.Err()),
			&sandbox.ExecStatusUnknownError{Swept: false, Detail: "the sweep could not finish"}))
	case "barrier":
		select {
		case <-x.started:
		case <-time.After(10 * time.Second):
			return nil, errors.New("the tool branch never started")
		}
	}
	return x.stubExecutor.Execute(ctx, node, input)
}

// A sibling's human pause does not drop an undecided branch: the run stops on
// it, rather than pause for an answer that would run the branch again.
func TestASiblingsPauseDoesNotDropAnUndecidedBranch(t *testing.T) {
	for _, await := range []ir.AwaitMode{ir.AwaitWaitAll, ir.AwaitBestEffort} {
		t.Run(await.String(), func(t *testing.T) {
			wf := &ir.Workflow{
				Name:  "pause_sibling",
				Entry: "entry",
				Nodes: map[string]ir.Node{
					"entry":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
					"fan":     &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll},
					"tool":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tool"}},
					"barrier": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "barrier"}},
					"gate":    &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
					"b":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
					"merge":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "merge"}, AwaitMode: await},
					"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
				},
				Edges: []*ir.Edge{
					{From: "entry", To: "fan"},
					{From: "fan", To: "tool"},
					{From: "fan", To: "barrier"},
					{From: "barrier", To: "gate"},
					{From: "gate", To: "b", Condition: "approved"},
					{From: "tool", To: "merge"},
					{From: "b", To: "merge"},
					{From: "merge", To: "done"},
				},
				Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{},
				Loops: map[string]*ir.Loop{}, Foreaches: map[string]*ir.Foreach{},
			}
			x := &pauseSiblingExecutor{stubExecutor: newStubExecutor(), started: make(chan struct{})}
			s := tmpStore(t)
			_ = New(wf, s, x).Run(context.Background(), "r", nil)
			r, err := s.LoadRun(context.Background(), "r")
			if err != nil {
				t.Fatal(err)
			}
			if r.Status == store.RunStatusPausedWaitingHuman || r.FailureCode != ErrCodeAmbiguousEffect {
				t.Fatalf("a sibling's pause over an undecided branch: recorded %s / %q, want the run stopped on %q", r.Status, r.FailureCode, ErrCodeAmbiguousEffect)
			}
		})
	}
}

// A raise posted while a node ran is applied, recorded and acknowledged when
// the node ends undecided at the budget deadline, as on any end there.
func TestARaiseIsDrainedWhenAnUndecidedEndMeetsTheBudgetDeadline(t *testing.T) {
	wf := oneNodeWorkflow(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}})
	wf.Budget = &ir.Budget{MaxDuration: "300ms"}
	ch := make(chan *OverrideMsg, 1)
	raise := NewRaiseBudgetOverride(ir.BudgetOverrides{MaxDuration: "1h"}, "operator")
	x := ctxNodeExecutor{stubExecutor: newStubExecutor(), a: func(ctx context.Context) (map[string]any, error) {
		ch <- raise
		<-ctx.Done()
		return nil, fmt.Errorf("model: tool node %q: shell command failed: %w", "a", errors.Join(
			fmt.Errorf("the node ended before its command's end was proven: %w", ctx.Err()),
			&sandbox.ExecStatusUnknownError{Swept: false, Detail: "the sweep could not finish"}))
	}}
	s := tmpStore(t)
	_ = New(wf, s, x, WithOverrideChannel(ch)).Run(context.Background(), "r", nil)
	if _, err := raise.Await(context.Background(), 200*time.Millisecond); err != nil {
		t.Fatalf("the raise posted during the node was never applied: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if r.BudgetRaises == nil || r.FailureCode != ErrCodeAmbiguousEffect {
		t.Fatalf("recorded %s / %q with raises %v, want %q and the raise recorded", r.Status, r.FailureCode, r.BudgetRaises, ErrCodeAmbiguousEffect)
	}
}

// A branch's event carries the code the run records for its end: an
// undecided effect, whatever wraps it — a stop, a wrapper's code.
func TestABranchEventCarriesTheUndecidedCode(t *testing.T) {
	undecided := &sandbox.ExecStatusUnknownError{Swept: false}
	for name, err := range map[string]error{
		"wrapping a cancel":    errors.Join(fmt.Errorf("ended: %w", context.Canceled), undecided),
		"behind a typed error": &RuntimeError{Code: ErrCodeExecutionFailed, Message: "subbot child failed", Cause: undecided},
	} {
		if got := errorCode(err); got != string(store.FailureAmbiguousEffect) {
			t.Errorf("%s: branch code %q, want %q", name, got, store.FailureAmbiguousEffect)
		}
	}
	if got := errorCode(fmt.Errorf("ended: %w", context.Canceled)); got != string(store.FailureCancelled) {
		t.Errorf("a stopped branch: code %q, want %q", got, store.FailureCancelled)
	}
}
