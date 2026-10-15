package runtime

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// requireBudgetDeadline returns the per-node budget deadline the engine put on
// a node's context, or an error naming the premise that failed. Both fixtures
// of this file aim at that deadline, and a fixture asked to aim at one it never
// got reports that instead of blocking until the harness timeout — so the
// regression "the engine stopped arming a per-node deadline" fails as itself,
// in milliseconds, in whichever witness runs first.
func requireBudgetDeadline(ctx context.Context, nodeID string) (time.Time, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Time{}, fmt.Errorf("fixture: node %q got no budget deadline to aim at", nodeID)
	}
	return deadline, nil
}

// ctxBlockingExecutor blocks in Execute (for the node named "slow") until the
// context is cancelled, then returns ctx.Err() — carrying `usage` when the
// caller sets one, which is what the node burned before it was cut off. Every
// other node returns fast. Simulates a single long-running / hung node — a
// stuck delegate subprocess, a runaway survey, a scanner with no internal
// bound — that the boundary budget check (which only gates NEW node starts)
// cannot interrupt.
//
// started and armed are the premise every caller judges itself by: the node
// reached the executor AND carried the engine's per-node budget deadline. A
// node that got NO deadline returns that fact as its error instead of blocking
// forever, so the regression TestNodeDeadlineFromDurationBudget guards fails on
// its own assertion in milliseconds instead of on the harness timeout
// (measured: 45s of `-timeout 45s` before, 0.03s now).
//
// expire, when set, is called once the node has started and found its
// deadline: it spends the run's duration budget on the clock the test owns
// (expireDurationBudget builds the pair). The engine's verdict then rests on
// an ORDER the test staged — the node started, THEN the cap ran out — never on
// a race between a loaded scheduler and a few hundred milliseconds.
type ctxBlockingExecutor struct {
	blockNode string
	usage     map[string]any
	expire    func()
	started   atomic.Bool
	armed     atomic.Bool
}

func (e *ctxBlockingExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() != e.blockNode {
		return map[string]any{"ok": true}, nil
	}
	// Recorded BEFORE the deadline is read: a node the engine never
	// dispatched and a node dispatched without a deadline are different
	// facts, and the tests below name them differently.
	e.started.Store(true)
	if _, err := requireBudgetDeadline(ctx, e.blockNode); err != nil {
		return nil, err
	}
	e.armed.Store(true)
	if e.expire != nil {
		e.expire()
	}
	<-ctx.Done()
	// A copy, not the field: the engine stamps `_duration_ms` onto whatever
	// map a node hands back (stampNodeDuration, on every exit), so returning
	// `usage` itself would let two executions of this fixture write one map.
	// maps.Clone(nil) is nil, so a caller that declared no usage is unchanged.
	return maps.Clone(e.usage), ctx.Err()
}

// expireDurationBudget puts a run's duration budget on a clock the test
// advances by hand, and returns the two halves a per-node deadline witness
// needs: the clock to hand the engine, and the spend that exhausts the cap.
//
// Frozen until the spend, the cap cannot be consumed by anything upstream of
// the node under observation — the engine's own construction, the store, the
// entry node — so the pre-exec gate always sees the full allowance and the
// node is always reached and always armed. That is the #2447 flake at its
// root: a 300ms cap that a merge-queue runner spent before `slow` started,
// leaving the booking its witness asserts with nothing to book (measured, run
// 38611812306: `the timed-out node's tokens never reached the run: 0`). Spent
// from inside the node, the other half of the premise holds too — the engine's
// post-exec verdict reads a budget that is genuinely out of time.
//
// The spend is the cap plus the sliver a real clock adds between the cut and
// the verdict, so `RemainingDuration() <= 0` is read past the cap rather than
// exactly on it. The per-node deadline itself is untouched: the engine still
// arms it from the remaining budget, and the node still dies on it, on the
// real clock.
func expireDurationBudget(t *testing.T, wf *ir.Workflow) (now func() time.Time, spend func()) {
	t.Helper()
	budgetCap := capOf(t, wf)
	clock := newBudgetTestClock()
	return clock.Now, func() { clock.Advance(budgetCap + time.Millisecond) }
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

	// The budget's clock is the test's, frozen until `slow` starts: the cap
	// measures how long the NODE is held, never how long the engine took to
	// reach it. Without that, everything upstream raced the 300ms — and a
	// runner that won the race took the pre-exec refusal instead, which emits
	// the same budget_exceeded(duration) failure this test reads, so the
	// witness passed with `slow` never dispatched. Measured on this tree: with
	// 400ms of upstream work, the test passes while `context.WithDeadline` is
	// REMOVED from engine_exec.go — the regression it guards, green.
	clockNow, spendTheCap := expireDurationBudget(t, wf)
	exec := &ctxBlockingExecutor{blockNode: "slow", expire: spendTheCap}
	s := tmpStore(t)
	eng := New(wf, s, exec)
	eng.budgetClock = clockNow

	start := time.Now()
	err := eng.Run(context.Background(), "run-node-deadline", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from node deadline / budget exceeded")
	}
	// The premise, before any verdict: this run is about a node the deadline
	// CUT, so a run that never dispatched `slow` — or dispatched it with no
	// deadline on its context — says nothing about that and must not pass.
	if !exec.started.Load() {
		t.Fatalf("node %q never ran (%v): the duration deadline had nothing to cancel", exec.blockNode, err)
	}
	if !exec.armed.Load() {
		t.Fatalf("node %q ran with no budget deadline on its context (%v): the per-node duration deadline is gone", exec.blockNode, err)
	}
	if !strings.Contains(err.Error(), "budget exceeded") || !strings.Contains(err.Error(), "duration") {
		t.Errorf("expected 'budget exceeded ... duration' error, got: %v", err)
	}
	// A hang guard, and calibrated as one. The denominator #1471's 10x applies
	// to is the whole eng.Run — the 300ms the node is HELD plus the engine's
	// own setup, the store and the entry node — and it is the LOADED figure:
	// an idle denominator calibrates a bound against the case it never fails
	// in. Measured on this tree, two runs of each shape: 0.32-0.39s over
	// `-race -count=20`, and 0.33-0.46s inside a full `-race` run of this
	// package; this family's own tail under a whole-repo `go test ./...` adds
	// 443ms on top (override_test.go measured it, a heavier machine than
	// saturated cores), so ~0.9s loaded. That puts the old 5s at ~6x, BELOW
	// the rule's 10x, where 30s is ~33x. The
	// forbidden alternative is a node held with no deadline at all, not a late
	// answer — and a node left unbounded no longer reaches this bound anyway:
	// it has no deadline to report, so the armed check above fails first, in
	// milliseconds.
	const deadlineHangGuard = 30 * time.Second
	if elapsed > deadlineHangGuard {
		t.Errorf("run took %v against a %s guard — the slow node was not force-cancelled by the duration deadline",
			elapsed, deadlineHangGuard)
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
// started and sawDeadline let the test tell a real regression from a fixture
// that never set up its own premise — and, among the fixtures that did not, the
// node that never ran from the node that ran with no deadline.
type internalTimeoutExecutor struct {
	blockNode string
	before    time.Duration
	// delayAfterFire holds the node between its own timeout firing and its
	// return — the gap a loaded scheduler supplies for free. The engine
	// judges when the node RETURNS, so this moves the verdict's instant
	// without moving the firing one.
	delayAfterFire time.Duration
	// upstreamHold is held by every node BUT blockNode, which is how the
	// run's cap gets spent before blockNode is ever reached — what a loaded
	// runner does for free, on demand.
	upstreamHold time.Duration
	runStart     time.Time
	// deadlineAtNS and firedAtNS are ABSOLUTE instants (UnixNano): the
	// per-node budget deadline the engine installed, and when the node's own
	// timeout went off. The budget's clock starts at deadlineAtNS minus the
	// cap, which is how the test reads its quantities against the RUN's clock
	// rather than against its own first line.
	deadlineAtNS atomic.Int64
	firedAtNS    atomic.Int64
	started      atomic.Bool
	sawDeadline  atomic.Bool
}

func (e *internalTimeoutExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() == e.blockNode {
		// Recorded BEFORE the deadline is read: a node that reached the
		// executor and found no deadline is a different fact from a node the
		// engine never dispatched, and armInternalTimeout answers them
		// differently.
		e.started.Store(true)
		deadline, err := requireBudgetDeadline(ctx, e.blockNode)
		if err != nil {
			return nil, err
		}
		e.sawDeadline.Store(true)
		e.deadlineAtNS.Store(deadline.UnixNano())
		inner, cancel := context.WithDeadline(ctx, deadline.Add(-e.before))
		defer cancel()
		<-inner.Done()
		e.firedAtNS.Store(time.Now().UnixNano())
		time.Sleep(e.delayAfterFire)
		return nil, fmt.Errorf("scanner timed out %s before the run's own deadline: %w", e.before, inner.Err())
	}
	time.Sleep(e.upstreamHold)
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

// internalTimeoutWorkflowWalledCap is internalTimeoutWorkflow whose cap is an
// absolute wall — ir.Budget.CapImposed, what a platform ceiling or a pool
// donor's allowance puts on a run: no overspend is afforded at all.
//
// A fixture that spends the cap UPSTREAM needs that. The 90% line
// checkBudgetBeforeExec refuses a new node from is not a wall on its own: from
// 100% up to the graced ceiling (cap × the exit grace ratio, 10% by default)
// the budget exit grace ADMITS the node and bounds it by
// GracedRemainingDuration — a real deadline, so the node both starts and arms.
// A hold set just under the cap to close the gate therefore REOPENS it as soon
// as a loaded scheduler adds a few tens of milliseconds. Against a walled cap
// the refusal is monotonic: every overspend is refused.
func internalTimeoutWorkflowWalledCap(cap time.Duration) *ir.Workflow {
	wf := internalTimeoutWorkflow(cap)
	wf.Budget.CapImposed = true
	return wf
}

// internalTimeoutAttempt is one run of internalTimeoutWorkflow, with the facts
// its test judges it by.
type internalTimeoutAttempt struct {
	attempt int
	runID   string
	store   store.RunStore
	err     error
	// judgedAt and firedAt are offsets into the CAP by the BUDGET's clock:
	// when the verdict was computed (an upper bound — read once Run returned)
	// and when the node's own timeout went off. Neither stands in for the
	// other: the engine judges when the node returns, and scheduling between
	// the two is enough to make a budget stop the correct answer while firedAt
	// is still under the cap. See TestAVerdictJudgedPastTheCapIsABudgetStop.
	judgedAt time.Duration
	firedAt  time.Duration
	started  bool
	armed    bool
}

// internalTimeoutAttempts bounds how many times the scenario below is set up
// before the test gives up on this machine.
const internalTimeoutAttempts = 4

// capOf reports the duration cap a fixture workflow declares. It is the length
// of the budget's clock, and every quantity these tests compare to it is read
// against that clock.
func capOf(t *testing.T, wf *ir.Workflow) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(wf.Budget.MaxDuration)
	if err != nil {
		t.Fatalf("the fixture's cap %q does not parse: %v", wf.Budget.MaxDuration, err)
	}
	return d
}

// armInternalTimeout runs the internal-timeout scenario until an attempt is
// ARMED and `usable` accepts it, and returns that attempt. needAttempts is how
// many attempts the caller's premise needs to have been GIVEN: a caller whose
// first attempt is refused on purpose needs 2, and getting fewer than that is
// the re-attempt being gone, not this machine being unable.
//
// Armed is a fact about the NODE, not about the clock: `slow` reached the
// executor AND carried the engine's per-node budget deadline on its context.
// The two ways that premise can be missing are not the same thing, and
// reading them both off one flag is what reddened this file on loaded
// runners (#2160):
//
//   - the node never ran — everything upstream of it, the entry node and the
//     engine's own work, spent past the 90% line checkBudgetBeforeExec
//     refuses a new node from, so the run ended before `slow`. That attempt
//     measured nothing about telling one deadline from another, and the
//     setup is attempted again.
//   - the node ran with NO deadline on its context — the per-node budget
//     deadline is gone, which is the regression TestNodeDeadlineFromDuration
//     Budget exists for. It fails here and now, rather than being
//     re-attempted into a skip that would hide it.
//
// Every quantity the caller compares to the cap is read against the BUDGET's
// clock: the per-node deadline the engine installs is the budget's start plus
// the cap (RemainingDuration is cap minus elapsed), so the deadline the node
// reports dates the instant the run's clock started. Read against this
// function's own start instead, those quantities also carry engine and store
// construction the budget never counted — 120-220ms under `-race`, enough on
// its own to put both attribution tests past a 1s cap they were inside, ten
// runs out of ten under load (#2160).
//
// Attempts exhausted is a Skip: the scenario is the only thing these tests
// can speak about, and a machine that cannot set it up gives no verdict
// rather than a flaky one. Exhausted having made FEWER attempts than the
// caller needs is a failure instead: a skip and a pass leave the same exit
// code, so the test that witnesses the re-attempt would report nothing at all
// on the day the re-attempt is deleted.
func armInternalTimeout(t *testing.T, wf *ir.Workflow, needAttempts int, newExec func(attempt int) *internalTimeoutExecutor, usable func(internalTimeoutAttempt) bool) internalTimeoutAttempt {
	t.Helper()
	budgetCap := capOf(t, wf)
	var last internalTimeoutAttempt
	for attempt := 1; attempt <= internalTimeoutAttempts; attempt++ {
		exec := newExec(attempt)
		a := internalTimeoutAttempt{
			attempt: attempt,
			runID:   fmt.Sprintf("run-internal-timeout-%d", attempt),
			store:   tmpStore(t),
		}
		exec.runStart = time.Now()
		a.err = New(wf, a.store, exec).Run(context.Background(), a.runID, nil)
		judged := time.Now()
		a.started, a.armed = exec.started.Load(), exec.sawDeadline.Load()
		// The budget's clock where the node reported its deadline; this
		// function's where no node ever armed, and the two figures only
		// carry the diagnosis below.
		origin := exec.runStart
		if dl := exec.deadlineAtNS.Load(); dl != 0 {
			origin = time.Unix(0, dl).Add(-budgetCap)
		}
		a.judgedAt = judged.Sub(origin)
		if fired := exec.firedAtNS.Load(); fired != 0 {
			a.firedAt = time.Unix(0, fired).Sub(origin)
		}
		last = a
		if a.started && !a.armed {
			t.Fatalf("node %q ran with no budget deadline on its context (attempt %d, run: %v): "+
				"the per-node duration deadline is gone", exec.blockNode, attempt, a.err)
		}
		if a.started && usable(a) {
			return a
		}
	}
	if last.attempt < needAttempts {
		t.Fatalf("the fixture gave up after %d attempt(s) where its premise needs %d: the re-attempt a cap "+
			"spent upstream is answered with is gone (last: started=%v armed=%v, fired %v, judged %v, run: %v)",
			last.attempt, needAttempts, last.started, last.armed, last.firedAt, last.judgedAt, last.err)
	}
	t.Skipf("could not set the scenario up in %d attempts (last: started=%v armed=%v, fired %v, judged %v, run: %v): "+
		"this machine neither reached the node under its cap nor held two deadlines apart",
		last.attempt, last.started, last.armed, last.firedAt, last.judgedAt, last.err)
	return internalTimeoutAttempt{}
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
	a := armInternalTimeout(t, internalTimeoutWorkflow(cap1s), 1,
		func(int) *internalTimeoutExecutor {
			return &internalTimeoutExecutor{blockNode: "slow", before: 80 * time.Millisecond, delayAfterFire: 400 * time.Millisecond}
		},
		// The node's own timeout has to land UNDER the cap: past it the run
		// genuinely was out of time, and the two instants this test holds
		// apart are the same one.
		func(a internalTimeoutAttempt) bool { return a.firedAt < cap1s })

	if a.judgedAt < cap1s {
		t.Fatalf("the verdict was judged at %v, still inside the %v cap: the delay did not open the gap this test exists to describe", a.judgedAt, cap1s)
	}
	if a.err == nil || !strings.Contains(a.err.Error(), "budget exceeded") {
		t.Fatalf("a run judged %v into a %v cap reported %v, want a budget stop — the sibling test's retry rule rests on this being the correct verdict", a.judgedAt, cap1s, a.err)
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
// timer goroutine late enough pushes the firing past the budget deadline —
// where the run genuinely IS out of time and a budget stop is the correct
// verdict, so there is nothing to assert. Measured: 3 runs in 15 miss that way
// under `-race` on 8 cores oversubscribed 2x (and a node-relative timeout,
// before this was anchored to ctx's own deadline, missed 4 in 12).
//
// What the margin may NOT be spent on is measuring it: armInternalTimeout
// reads the budget's own clock, so the 80ms covers the engine's work between
// the node's return and the verdict and nothing else. Widening the gap is
// bounded on the other side — it has to stay inside the cap's last 10%, or the
// run is no longer near enough to its cap to pin the proximity guard at all,
// which is why the premise is asserted below rather than argued here.
//
// So a miss re-attempts the SETUP; it never softens the verdict. A run that arms
// and then reports a budget stop fails immediately, on the first attempt.
//
// The same load has a second way to miss, and armInternalTimeout owns both: the
// entry node plus the engine's own work can spend the cap before `slow` is
// dispatched at all, which is a setup that never happened — not the per-node
// deadline going missing, which still fails on the spot.
func TestNodeDeadlineDoesNotClaimAnInternalTimeout(t *testing.T) {
	const cap1s = time.Second
	a := armInternalTimeout(t, internalTimeoutWorkflow(cap1s), 1,
		func(int) *internalTimeoutExecutor {
			return &internalTimeoutExecutor{blockNode: "slow", before: 80 * time.Millisecond}
		},
		// The verdict has to be read while the run still had time on its
		// clock: judged past the cap the budget deadline won, a budget stop
		// is RIGHT, and the attempt proves nothing.
		func(a internalTimeoutAttempt) bool { return a.judgedAt < cap1s })

	// And the other side of the same premise: the verdict landed INSIDE the
	// cap's last 10%, where `used >= limit*0.9` holds and a guard reading it
	// would claim the budget. Short of that line the run is not near its cap,
	// and this test would pass with the proximity guard still wrong.
	if a.judgedAt < cap1s*9/10 {
		t.Fatalf("the verdict landed %v into a %v cap, short of the last 10%% where `used >= limit*0.9` holds: "+
			"the proximity guard was never engaged, so this run says nothing about attribution", a.judgedAt, cap1s)
	}
	if a.err == nil {
		t.Fatal("expected the node's own timeout to fail the run")
	}
	if strings.Contains(a.err.Error(), "budget exceeded") {
		t.Fatalf("a timeout that fired %v into a %v cap — with the run's own clock still "+
			"running — was sentenced as a budget stop: %v", a.firedAt, cap1s, a.err)
	}

	events, lerr := a.store.LoadEvents(context.Background(), a.runID)
	if lerr != nil {
		t.Fatalf("load events: %v", lerr)
	}
	if hasEventType(events, store.EventBudgetExceeded) {
		t.Error("budget_exceeded emitted for a node that died on its own timeout with time still on the run's clock")
	}

	r, lerr := a.store.LoadRun(context.Background(), a.runID)
	if lerr != nil {
		t.Fatalf("load run: %v", lerr)
	}
	// Resumable, and NOT finalized as a spent budget: the run still has
	// time, so resuming it is a live option rather than a re-death.
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable", r.Status)
	}
}

// TestTheInternalTimeoutFixtureReAttemptsACapSpentUpstream: a run whose cap was
// spent before `slow` ever started measured nothing about attribution, and the
// setup is attempted again rather than failed. That is the loaded runner of
// #2160, made deterministic by holding the entry node past the 90% line
// checkBudgetBeforeExec refuses a new node from — against a cap that is a WALL,
// since that line alone is only the near edge of a band the exit grace reopens
// (internalTimeoutWorkflowWalledCap, pinned by the test below it).
func TestTheInternalTimeoutFixtureReAttemptsACapSpentUpstream(t *testing.T) {
	const cap1s = time.Second
	a := armInternalTimeout(t, internalTimeoutWorkflowWalledCap(cap1s), 2,
		func(attempt int) *internalTimeoutExecutor {
			exec := &internalTimeoutExecutor{blockNode: "slow", before: 80 * time.Millisecond}
			if attempt == 1 {
				// 95% of the cap on the entry node alone, so the gate refuses
				// `slow` whatever else the machine is doing: against a walled
				// cap the hold only ever pushes `used` further past the line.
				exec.upstreamHold = cap1s * 95 / 100
			}
			return exec
		},
		// Any armed attempt proves the point: where the verdict's clock fell
		// is the attribution tests' business, not this one's.
		func(internalTimeoutAttempt) bool { return true })

	if a.attempt < 2 {
		t.Fatalf("the attempt that armed is #%d: the run that never reached %q was read as an armed one", a.attempt, "slow")
	}
}

// TestAWalledCapRefusesEveryOverspend pins the premise the re-attempt witness
// above rests on: the gate it closes by spending the cap upstream refuses the
// next node at EVERY overspend, not only between 90% and 100% of the cap.
//
// Read off the policy predicate rather than off engine runs, because the
// quantities are a budget's elapsed time and the grace's answer to it: a
// restored elapsed says in microseconds what a sweep of real runs would spend
// seconds saying, and it says it at the band's edges rather than wherever the
// scheduler happened to land. Both environment layers the grace reads are
// pinned to their defaults, so what refuses below is the imposed cap and
// nothing else.
func TestAWalledCapRefusesEveryOverspend(t *testing.T) {
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "")
	t.Setenv("ITERION_LOOP_BUDGET_GUARD", "on")
	const cap1s = time.Second
	wf := internalTimeoutWorkflowWalledCap(cap1s)
	eng := New(wf, tmpStore(t), &internalTimeoutExecutor{blockNode: "slow"})
	for _, spent := range []time.Duration{cap1s * 95 / 100, cap1s, cap1s * 105 / 100, cap1s * 2} {
		b := newSharedBudget(wf.Budget, nil)
		b.Restore(0, 0, 0, spent, 0, 0, 0)
		checks := b.Check()
		if findExceeded(checks) == nil && findHardLimited(checks) == nil {
			t.Fatalf("%v of a %v cap is neither exceeded nor hard-limited: the gate has nothing to refuse a new node on", spent, cap1s)
		}
		if dim, ok := eng.withinSharedBudgetGrace(b); ok {
			t.Fatalf("%v of a %v cap: the %s exit grace admits a node anyway, so a hold spending the cap upstream does not keep %q from starting", spent, cap1s, dim, "slow")
		}
	}
}
