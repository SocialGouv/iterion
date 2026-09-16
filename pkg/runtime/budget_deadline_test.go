package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ctxBlockingExecutor blocks in Execute (for the node named "slow") until the
// context is cancelled, then returns ctx.Err(). Every other node returns fast.
// Simulates a single long-running / hung node — a stuck delegate subprocess, a
// runaway survey, a scanner with no internal bound — that the boundary budget
// check (which only gates NEW node starts) cannot interrupt.
type ctxBlockingExecutor struct {
	blockNode string
}

func (e *ctxBlockingExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() == e.blockNode {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]any{"ok": true}, nil
}

// TestNodeDeadlineFromDurationBudget verifies that a single node which runs
// past the run's max_duration budget is force-cancelled by the per-node
// wall-clock deadline derived from the remaining budget — rather than running
// unbounded (the boundary budget check only blocks NEW node starts) — and that
// the expiry surfaces as a resumable BUDGET_EXCEEDED(duration) failure, not a
// retry. Regression for the dogfood finding where a deepsec scanner ran 81m on
// a 90m budget and a survey node ran 100m on a 50m budget.
func TestNodeDeadlineFromDurationBudget(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "node_deadline_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			// "a" runs fast so a checkpoint exists before "slow" fails —
			// making the failure resumable rather than first-node terminal.
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"slow": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "slow"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "slow"},
			{From: "slow", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxDuration: "300ms"},
	}

	exec := &ctxBlockingExecutor{blockNode: "slow"}
	s := tmpStore(t)
	eng := New(wf, s, exec)

	start := time.Now()
	err := eng.Run(context.Background(), "run-node-deadline", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from node deadline / budget exceeded")
	}
	if !strings.Contains(err.Error(), "budget exceeded") || !strings.Contains(err.Error(), "duration") {
		t.Errorf("expected 'budget exceeded ... duration' error, got: %v", err)
	}
	// The deadline must fire near max_duration. If the per-node deadline were
	// missing, the blocking node would hang forever and this would time out.
	if elapsed > 5*time.Second {
		t.Errorf("run took %v — the slow node was not force-cancelled by the duration deadline", elapsed)
	}

	r, err := s.LoadRun(context.Background(), "run-node-deadline")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("expected failed_resumable status (checkpoint after 'a'), got %s", r.Status)
	}

	// The duration budget_exceeded event must have been emitted.
	events, err := s.LoadEvents(context.Background(), "run-node-deadline")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if !hasEventType(events, store.EventBudgetExceeded) {
		t.Error("expected a budget_exceeded event for the duration deadline")
	}
}

// internalTimeoutExecutor fails the named node with a DeadlineExceeded of its
// OWN — a shorter timeout inside the node, the shape of a scanner or an HTTP
// client that bounds itself — rather than the run's per-node budget deadline.
// It fires strictly before that deadline, which is the only thing that tells
// the two apart.
//
// The firing instant is taken from ctx's OWN deadline — the budget deadline the
// engine just installed — and set `before` ahead of it, rather than as a delay
// from when the node happens to start. That difference is the whole robustness
// of the fixture, and it was measured: a node-relative 920ms timeout under a 1s
// cap flaked 4 runs in 12 under `-race` on oversubscribed cores, because the
// node starts ~90ms into the run there and the budget deadline won the race.
// Anchored to the deadline, the gap is `before` by construction, whatever the
// scheduler does to everything upstream.
//
// firedAtNS and sawDeadline let the test tell a real regression from a fixture
// that never set up its own premise.
type internalTimeoutExecutor struct {
	blockNode string
	before    time.Duration
	// delayAfterFire holds the node between its own timeout firing and its
	// return — the gap a loaded scheduler supplies for free. The engine
	// judges when the node RETURNS, so this moves the verdict's instant
	// without moving the firing one.
	delayAfterFire time.Duration
	runStart       time.Time
	firedAtNS      atomic.Int64
	sawDeadline    atomic.Bool
}

func (e *internalTimeoutExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() == e.blockNode {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, fmt.Errorf("fixture: node %q got no budget deadline to aim at", e.blockNode)
		}
		e.sawDeadline.Store(true)
		inner, cancel := context.WithDeadline(ctx, deadline.Add(-e.before))
		defer cancel()
		<-inner.Done()
		e.firedAtNS.Store(int64(time.Since(e.runStart)))
		time.Sleep(e.delayAfterFire)
		return nil, fmt.Errorf("scanner timed out %s before the run's own deadline: %w", e.before, inner.Err())
	}
	return map[string]any{"ok": true}, nil
}

// internalTimeoutWorkflow is the shape both attribution tests need: an entry
// node, a node that will die on its own timeout, and a duration cap.
func internalTimeoutWorkflow(cap time.Duration) *ir.Workflow {
	return &ir.Workflow{
		Name:  "internal_timeout_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"slow": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "slow"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "slow"},
			{From: "slow", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxDuration: cap.String()},
	}
}

// TestAVerdictJudgedPastTheCapIsABudgetStop.
//
// The engine judges when the node RETURNS, not when its own timeout fired.
// Hold the executor past the cap after firing early and "budget exceeded"
// becomes the RIGHT answer, with the firing instant still under the cap.
//
// That is why the guard in TestNodeDeadlineDoesNotClaimAnInternalTimeout must
// re-attempt on the JUDGED clock: it used to read the firing one, so a loaded
// runner supplying this same gap reddened a test about attribution — on pull
// requests touching nothing in this package.
//
// The direction is the safe one: sleeping longer only puts the verdict further
// past the cap.
func TestAVerdictJudgedPastTheCapIsABudgetStop(t *testing.T) {
	const cap1s = time.Second
	wf := internalTimeoutWorkflow(cap1s)
	exec := &internalTimeoutExecutor{blockNode: "slow", before: 80 * time.Millisecond, delayAfterFire: 400 * time.Millisecond}
	s := tmpStore(t)

	exec.runStart = time.Now()
	err := New(wf, s, exec).Run(context.Background(), "run-judged-late", nil)
	judgedAt := time.Since(exec.runStart)
	firedAt := time.Duration(exec.firedAtNS.Load())

	if !exec.sawDeadline.Load() {
		t.Fatal("fixture never armed: the node got no budget deadline")
	}
	if firedAt >= cap1s {
		t.Skipf("the node's own timeout landed past the %v cap (%v): this machine cannot hold the two instants apart", cap1s, firedAt)
	}
	if judgedAt < cap1s {
		t.Fatalf("the verdict was judged at %v, still inside the %v cap: the delay did not open the gap this test exists to describe", judgedAt, cap1s)
	}
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("a run judged %v into a %v cap reported %v, want a budget stop — the sibling test's retry rule rests on this being the correct verdict", judgedAt, cap1s, err)
	}
}

// TestNodeDeadlineDoesNotClaimAnInternalTimeout is the true-negative twin of
// TestNodeDeadlineFromDurationBudget: a node killed by its own shorter timeout,
// while the run sits deep in the last 10% of its duration cap, is NOT the run
// running out of time. It must reach ordinary recovery dispatch — which can
// retry it — instead of being sentenced as BUDGET_EXCEEDED with a "raise
// budget.duration and resume" hint that would not have helped.
//
// This is the half of the attribution the proximity guard got wrong in the
// direction nothing else covers. The raise-side tests in override_test.go pin
// the other half; without this one, `used >= limit*0.9` could come back for
// every expiry that arrives with no raise queued and no test would notice.
//
// The node must START under the 90% hard limit (checkBudgetBeforeExec refuses a
// new node from there) and FAIL above it, so its own timeout is what carries the
// run across the line: firing 80ms before the 1s budget deadline puts `used` at
// ~920ms against a 900ms line while leaving 80ms for the executor to return and
// the guard to read the clock.
//
// That 80ms is a real margin, and it is why the fixture ARMS in a loop. The
// scenario is two deadlines 80ms apart, so a scheduler that wakes the node's
// timer goroutine late enough pushes the firing past the budget deadline — where
// the run genuinely IS out of time and a budget stop is the correct verdict, so
// there is nothing to assert. Measured: 3 runs in 15 miss that way under `-race`
// on 8 cores oversubscribed 2x (and a node-relative timeout, before this was
// anchored to ctx's own deadline, missed 4 in 12). Widening the gap is not
// available — it has to stay inside the cap's last 10% or the run is no longer
// near enough to its cap to pin the proximity guard at all, and scaling the cap
// buys the margin at seconds of wall clock in a required check.
//
// So a miss re-attempts the SETUP; it never softens the verdict. A run that arms
// and then reports a budget stop fails immediately, on the first attempt.
func TestNodeDeadlineDoesNotClaimAnInternalTimeout(t *testing.T) {
	const cap1s = time.Second
	wf := internalTimeoutWorkflow(cap1s)

	const attempts = 4
	for attempt := 1; ; attempt++ {
		runID := fmt.Sprintf("run-internal-timeout-%d", attempt)
		exec := &internalTimeoutExecutor{blockNode: "slow", before: 80 * time.Millisecond}
		s := tmpStore(t)
		eng := New(wf, s, exec)

		exec.runStart = time.Now()
		err := eng.Run(context.Background(), runID, nil)
		// The clock the VERDICT was computed against, read after it was
		// computed — so an upper bound on it. The firing instant below is a
		// different quantity and cannot stand in for it: the engine judges
		// when the node returns, and scheduling between the two is enough to
		// make a budget stop the correct answer while `firedAt` is still
		// under the cap. See TestAVerdictJudgedPastTheCapIsABudgetStop.
		judgedAt := time.Since(exec.runStart)
		firedAt := time.Duration(exec.firedAtNS.Load())

		if err == nil {
			t.Fatal("expected the node's own timeout to fail the run")
		}
		if !exec.sawDeadline.Load() {
			t.Fatal("fixture never armed: the node got no budget deadline, so nothing here " +
				"is about telling one deadline from another")
		}
		if judgedAt >= cap1s {
			// The budget deadline won: the run really had run out of time by
			// the instant it was judged, so a budget stop would be RIGHT and
			// this attempt proves nothing.
			if attempt == attempts {
				t.Skipf("could not set the scenario up in %d attempts: the run kept being "+
					"judged past the %v cap (last: fired %v, judged %v), so the machine is "+
					"too loaded to hold two deadlines 80ms apart", attempts, cap1s, firedAt, judgedAt)
			}
			continue
		}

		if strings.Contains(err.Error(), "budget exceeded") {
			t.Fatalf("a timeout that fired %v into a %v cap — with the run's own clock still "+
				"running — was sentenced as a budget stop: %v", firedAt, cap1s, err)
		}

		events, lerr := s.LoadEvents(context.Background(), runID)
		if lerr != nil {
			t.Fatalf("load events: %v", lerr)
		}
		if hasEventType(events, store.EventBudgetExceeded) {
			t.Error("budget_exceeded emitted for a node that died on its own timeout with time still on the run's clock")
		}

		r, lerr := s.LoadRun(context.Background(), runID)
		if lerr != nil {
			t.Fatalf("load run: %v", lerr)
		}
		// Resumable, and NOT finalized as a spent budget: the run still has
		// time, so resuming it is a live option rather than a re-death.
		if r.Status != store.RunStatusFailedResumable {
			t.Errorf("status = %s, want failed_resumable", r.Status)
		}
		return
	}
}
