package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// residualFanExecutor starts both branches before triggering a stop. The
// undecided command either reports its failed sweep or stays alive past grace.
type residualFanExecutor struct {
	*stubExecutor
	started chan struct{}
	release chan struct{}
	stop    func()
	trigger error
	late    bool
	once    sync.Once
}

func (x *residualFanExecutor) Execute(ctx context.Context, n ir.Node, in map[string]any) (map[string]any, error) {
	id := n.NodeID()
	if id == "handle" {
		if in["id"] == "A" {
			id = "agent_a"
		} else {
			id = "agent_b"
		}
	}
	switch id {
	case "agent_a":
		close(x.started)
		<-ctx.Done()
		if x.late {
			<-x.release
		}
		return nil, errors.Join(ctx.Err(), &sandbox.ExecStatusUnknownError{Detail: "the sweep does not settle the command"})
	case "agent_b":
		<-x.started
		x.once.Do(x.stop)
		if x.trigger != nil {
			return nil, x.trigger
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return x.stubExecutor.Execute(ctx, n, in)
}

// Mutation: remove collectBranches' ambiguous-result classification, or its
// abandoned-result construction. Each topology must persist AMBIGUOUS_EFFECT
// before it returns, even on a context-sensitive store; nothing reaches join.
func TestFanOutStopsOnUndecidedEffects(t *testing.T) {
	for _, topology := range []string{"all", "each", "multi"} {
		for _, trigger := range []string{"cancel", "drain", "deadline", "sibling", "budget", "pause"} {
			for _, late := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/late=%v", topology, trigger, late), func(t *testing.T) {
					old := branchCancelGracePeriod
					branchCancelGracePeriod = 25 * time.Millisecond
					defer func() { branchCancelGracePeriod = old }()
					wf := fanOutWorkflow(ir.AwaitBestEffort)
					if trigger == "sibling" {
						wf = fanOutWorkflow(ir.AwaitWaitAll)
					}
					branchID := "branch_router_agent_a"
					if topology == "each" {
						await := ir.AwaitBestEffort
						if trigger == "sibling" {
							await = ir.AwaitWaitAll
						}
						wf = fanOutEachWorkflow(false, await, 2)
						branchID = "branch_dispatch_0"
					}
					if topology == "multi" {
						r := wf.Nodes["router"].(*ir.RouterNode)
						r.RouterMode, r.RouterMulti, r.Model = ir.RouterLLM, true, "test-model"
					}
					ctx, cancel := context.WithCancelCause(context.Background())
					defer cancel(nil)
					stop := func() { cancel(context.Canceled) }
					var triggerErr error
					switch trigger {
					case "drain":
						stop = func() { cancel(ErrRunInterrupted) }
					case "deadline":
						// The budget has to cover the entry node, the router
						// and the branch start before it may expire, since
						// what this case measures is a deadline reached while
						// a branch is UNDECIDED. At 200 ms a loaded CI shard
						// lost that race twice in one hour (2026-10-29,
						// race-shard (0) on #2751 and #2764: "[TIMEOUT]
						// timeout: context deadline exceeded (node: entry)",
						// i.e. the run died before any fan-out). Two seconds
						// is a tenfold margin over the worst measured
						// pre-branch stretch, and the guard below names the
						// environment if even that is ever lost — a faked
						// context is NOT the alternative: the engine derives
						// branchCtx with context.WithCancel, Go propagates the
						// parent's error verbatim, so a stub would hand the
						// branches Canceled and quietly turn this case into a
						// duplicate of "cancel".
						var deadlineCancel context.CancelFunc
						ctx, deadlineCancel = context.WithTimeout(ctx, 2*time.Second)
						defer deadlineCancel()
						stop = func() {}
					case "sibling":
						stop = func() {}
						triggerErr = errors.New("sibling fails")
					case "budget":
						stop = func() {}
						triggerErr = ErrBudgetExceeded
					case "pause":
						stop = func() {}
						triggerErr = ErrRunPaused
					}
					x := &residualFanExecutor{stubExecutor: newStubExecutor(), started: make(chan struct{}), release: make(chan struct{}), stop: stop, trigger: triggerErr, late: late}
					x.on("entry", func(map[string]any) (map[string]any, error) {
						return map[string]any{"summary": "input", "items": []any{item("A"), item("B")}}, nil
					})
					x.on("router", func(map[string]any) (map[string]any, error) {
						return map[string]any{"selected_routes": []any{"agent_a", "agent_b"}}, nil
					})
					s := doneRefusingResumableStore{doneRefusingStore{tmpStore(t)}}
					// Join the late branch before its store's owned scratch is removed.
					defer func() {
						close(x.release)
						if late {
							waitBranchFinished(t, s, "r", branchID)
						}
					}()
					err := New(wf, s, x).Run(ctx, "r", nil)
					if trigger == "deadline" {
						// A deadline that fired before the branches were up
						// measured the budget, not the classification: say so
						// instead of failing on a verdict this run never had
						// the chance to reach.
						select {
						case <-x.started:
						default:
							t.Fatalf("the deadline fired before the fan-out started (err = %v) — this run measured the 2s budget, not the undecided-effect classification; the machine, not the code, is what to look at", err)
						}
					}
					var rt *RuntimeError
					if !errors.As(err, &rt) || rt.Code != ErrCodeAmbiguousEffect {
						t.Errorf("returned %v, want AMBIGUOUS_EFFECT", err)
					}
					r, loadErr := s.LoadRun(context.Background(), "r")
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					if r.Status != store.RunStatusFailedResumable || r.FailureCode != ErrCodeAmbiguousEffect {
						t.Errorf("recorded %s/%s, want failed_resumable/AMBIGUOUS_EFFECT", r.Status, r.FailureCode)
					}
					events, loadErr := s.LoadEvents(context.Background(), "r")
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					found := false
					for _, ev := range events {
						if ev.Type == store.EventRunFailed && ev.Data["code"] == string(ErrCodeAmbiguousEffect) {
							found = true
						}
						if ev.Type == store.EventNodeStarted && (ev.NodeID == "finalize" || ev.NodeID == "collect") {
							t.Error("join executes beside an undecided effect")
						}
					}
					if !found {
						t.Error("no run_failed event carries AMBIGUOUS_EFFECT")
					}
				})
			}
		}
	}
}

// The first settled error in a join does not hide a later undecided one.
// Mutation: replace the tree walk with one errors.As + AmbiguousEffect call.
func TestAmbiguousEffectReadsEveryJoinedCause(t *testing.T) {
	ordinary := &RuntimeError{Code: ErrCodeExecutionFailed, Message: "ordinary"}
	undecided := &RuntimeError{Code: ErrCodeAmbiguousEffect, Message: "recorded child"}
	for _, err := range []error{
		errors.Join(ordinary, undecided),
		errors.Join(&sandbox.ExecStatusUnknownError{Swept: true}, undecided),
		&RuntimeError{Code: ErrCodeExecutionFailed, Cause: undecided},
	} {
		if !IsAmbiguousEffect(err) {
			t.Errorf("undecided effect is hidden: %v", err)
		}
	}
	if IsAmbiguousEffect(ordinary) || IsAmbiguousEffect(&sandbox.ExecStatusUnknownError{Swept: true}) {
		t.Fatal("a settled failure becomes undecided")
	}
}
