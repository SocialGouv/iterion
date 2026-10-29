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

// armedDeadline is a context whose deadline starts counting only when the
// test arms it — the moment the undecided branch is running — so a slow
// runner cannot spend the budget before that branch exists. A budget
// measured from the run's start expired inside the router on a loaded runner
// under the race detector (CI race-shard(0) on fdf9d9761, #2840) and the run
// recorded TIMEOUT while no effect was undecided yet. Once fired, Err reads
// as context.DeadlineExceeded exactly as a real deadline does, so the
// engine's TIMEOUT classification stays the path under test; a cancelled
// parent fires it with the parent's error instead.
type armedDeadline struct {
	parent context.Context
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	at     time.Time
	err    error
}

func newArmedDeadline(parent context.Context) *armedDeadline {
	c := &armedDeadline{parent: parent, done: make(chan struct{})}
	go func() {
		select {
		case <-parent.Done():
			c.fire(parent.Err())
		case <-c.done:
		}
	}()
	return c
}

// arm starts the budget now: the deadline fires after d.
func (c *armedDeadline) arm(d time.Duration) {
	c.mu.Lock()
	c.at = time.Now().Add(d)
	c.mu.Unlock()
	time.AfterFunc(d, func() { c.fire(context.DeadlineExceeded) })
}

func (c *armedDeadline) fire(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *armedDeadline) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at, !c.at.IsZero()
}

func (c *armedDeadline) Done() <-chan struct{} { return c.done }

func (c *armedDeadline) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *armedDeadline) Value(key any) any { return c.parent.Value(key) }

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
						// The budget starts when the undecided branch is running (agent_b
						// arms it once agent_a has started), never from the run's start: a
						// fixed 200ms measured from Run expired inside the router on a loaded
						// runner under the race detector and recorded TIMEOUT with no effect
						// in flight (CI race-shard(0) on fdf9d9761, #2840).
						armed := newArmedDeadline(ctx)
						ctx = armed
						stop = func() { armed.arm(50 * time.Millisecond) }
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
