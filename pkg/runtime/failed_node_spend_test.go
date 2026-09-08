package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/clock"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// checkpointCapturingStore keeps the last checkpoint the engine wrote, which
// is where a resume reads the budget carry from.
type checkpointCapturingStore struct {
	store.RunStore
	last *store.Checkpoint
}

func (s *checkpointCapturingStore) SaveCheckpoint(ctx context.Context, runID string, cp *store.Checkpoint) error {
	s.last = cp
	return s.RunStore.SaveCheckpoint(ctx, runID, cp)
}

func (s *checkpointCapturingStore) FailRunResumable(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	if cp != nil {
		s.last = cp
	}
	return s.RunStore.FailRunResumable(ctx, id, cp, runErr, code)
}

func (s *checkpointCapturingStore) FailRunTerminal(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	if cp != nil {
		s.last = cp
	}
	return s.RunStore.FailRunTerminal(ctx, id, cp, runErr, code)
}

// A node that FAILED still spent. The delegate stamps the pass's cost on the
// result it returns beside the error; until this landed, nothing read it —
// `recordBudget` runs on the success path only, so the run's totals, the
// daily cap and a lending donor's ledger all missed whatever the failing node
// burned. On a long agent node that is a whole session.
func TestFailedNodeSpendIsRecorded(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Budget = &ir.Budget{MaxTokens: 10_000}
	engine := New(wf, tmpStore(t), newStubExecutor())
	shared := newSharedBudget(wf.Budget, engine.logger)
	rs := &runState{budget: shared, loopBudgetMarks: make(map[string]loopBudgetMark)}

	engine.recordFailedNodeSpend(rs, "agent", map[string]any{"_tokens": 4_000, "_cost_usd": 1.25})
	tokens, cost, _, _, _, _ := shared.Snapshot()
	if tokens != 4_000 {
		t.Fatalf("the failed node's tokens never reached the run: %d", tokens)
	}
	if cost != 1.25 {
		t.Fatalf("the failed node's cost never reached the run: %v", cost)
	}

	// Over the cap is not this function's verdict: the node's own failure is
	// the run's, and raising a budget error here would replace a named cause
	// with a generic one on a run that is already ending. It must still book.
	engine.recordFailedNodeSpend(rs, "agent", map[string]any{"_tokens": 20_000})
	if tokens, _, _, _, _, _ := shared.Snapshot(); tokens != 24_000 {
		t.Fatalf("an over-budget failure was not booked: %d", tokens)
	}

	// A node that spent nothing books nothing — no phantom zero rows.
	before, _, _, _, _, _ := shared.Snapshot()
	engine.recordFailedNodeSpend(rs, "tool", map[string]any{"ok": true})
	if after, _, _, _, _, _ := shared.Snapshot(); after != before {
		t.Fatalf("a spendless failure moved the totals: %d -> %d", before, after)
	}
	engine.recordFailedNodeSpend(rs, "tool", nil)
}

// And the WIRING, which is the half a helper test cannot show: a node that
// fails terminally books its spend, while one the engine retries IN PLACE
// does not — the retry continues a session whose usage is cumulative, so
// counting both would bill the same tokens twice.
func TestFailedNodeSpendReachesTheRunOnlyOnce(t *testing.T) {
	build := func() *ir.Workflow {
		return &ir.Workflow{
			Name:  "spend_test",
			Entry: "agent",
			Nodes: map[string]ir.Node{
				"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
				"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			},
			Edges:   []*ir.Edge{{From: "agent", To: "done"}},
			Schemas: map[string]*ir.Schema{},
			Prompts: map[string]*ir.Prompt{},
			Vars:    map[string]*ir.Var{},
			Loops:   map[string]*ir.Loop{},
			Budget:  &ir.Budget{MaxTokens: 1_000_000},
		}
	}

	t.Run("a terminal failure books what it burned", func(t *testing.T) {
		exec := newStubExecutor()
		exec.on("agent", func(_ map[string]any) (map[string]any, error) {
			// The shape the delegate returns on a failure: the pass's spend
			// stamped on the result that travels with the error.
			return map[string]any{"_tokens": 7_000, "_cost_usd": 2.10}, errors.New("stream closed")
		})
		st := &checkpointCapturingStore{RunStore: tmpStore(t)}
		eng := New(build(), st, exec)
		if err := eng.Run(context.Background(), "run-spend-1", nil); err == nil {
			t.Fatal("the run was supposed to fail")
		}
		// Read where a RESUME reads it: the checkpoint's budget carry, which
		// is what stops a resumed run from re-granting the whole allowance.
		cp := st.last
		if cp == nil {
			t.Fatal("no checkpoint to read the budget from")
		}
		if cp.BudgetTokensUsed != 7_000 {
			t.Fatalf("a failed node's session was not booked: %d", cp.BudgetTokensUsed)
		}
		if cp.BudgetCostUSD != 2.10 {
			t.Fatalf("a failed node's cost was not booked: %v", cp.BudgetCostUSD)
		}
	})

	// The other half, and the one a booking call added at the retry exit would
	// break silently: an attempt the engine retries IN PLACE is NOT booked,
	// because the retry continues the same session and reports that session's
	// running total. Booking the abandoned attempt as well bills its tokens a
	// second time. Driven through the production ClawExecutor and a real
	// delegate.Backend, so the numbers under test are the ones a session-
	// cumulative backend actually reports — a runtime stub would only be
	// re-stating the assumption.
	t.Run("an in-place retry books the session once, not once per attempt", func(t *testing.T) {
		backend := &sessionCumulativeBackend{}
		reg := delegate.NewRegistry()
		reg.Register("cumulative_stub", backend)
		wf := build()
		wf.Nodes["agent"] = &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "agent"},
			LLMFields: ir.LLMFields{Backend: "cumulative_stub", Model: "anthropic/claude-opus-5"},
		}
		exec := model.NewClawExecutor(model.NewRegistry(), wf,
			model.WithBackendRegistry(reg),
			// The engine's recovery dispatcher drives the retry; the
			// executor's own ladder would hide it inside one Execute.
			model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
		)
		// Retry the first failure, then let the second attempt stand.
		dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
			if prior(ErrCodeRateLimited) > 0 {
				return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeRateLimited
			}
			return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeRateLimited
		})

		st := &checkpointCapturingStore{RunStore: tmpStore(t)}
		eng := New(wf, st, exec, WithRecoveryDispatch(dispatch))
		if err := eng.Run(context.Background(), "run-spend-retry", nil); err != nil {
			t.Fatalf("the retry was supposed to carry the run through: %v", err)
		}
		if backend.calls != 2 {
			t.Fatalf("expected a failed attempt and its retry, got %d delegation(s)", backend.calls)
		}
		cp := st.last
		if cp == nil {
			t.Fatal("no checkpoint to read the budget from")
		}
		// 8_000 is the SESSION's total as the successful retry reports it.
		// 13_000 (5_000 + 8_000) is what booking the abandoned attempt too
		// would produce — the same tokens billed twice.
		if cp.BudgetTokensUsed != 8_000 {
			t.Fatalf("the retried session was not booked exactly once: %d tokens (5_000 + 8_000 = double-billed)", cp.BudgetTokensUsed)
		}
		if cp.BudgetCostUSD != 1.60 {
			t.Fatalf("the retried session's cost was not booked exactly once: %v", cp.BudgetCostUSD)
		}
	})
}

// sessionCumulativeBackend is the shape the engine's no-book-on-retry rule
// rests on: the first delegation dies after burning 5_000 tokens, and the
// retry — continuing the SAME session — reports the session's running total
// (8_000), not the 3_000 it added. claude_code accounts this way by design
// (`annotateCost` takes the max across result messages rather than the sum).
type sessionCumulativeBackend struct {
	calls int
}

func (b *sessionCumulativeBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.calls++
	if b.calls == 1 {
		return delegate.Result{
			Output:      map[string]any{"_tokens": 5_000, "_cost_usd": 1.00},
			Tokens:      5_000,
			BackendName: "cumulative_stub",
		}, errors.New("stream closed mid-session")
	}
	return delegate.Result{
		Output:      map[string]any{"ok": true, "_tokens": 8_000, "_cost_usd": 1.60},
		Tokens:      8_000,
		BackendName: "cumulative_stub",
	}, nil
}

// Booking is ACCOUNTING, never a verdict. A failing node whose spend also
// crosses the cap must not have the booking speak for the run: the immediate
// budget path does not merely return an error, it emits budget_exceeded and
// WRITES the run failed_resumable(BUDGET_EXCEEDED) — so on the recovery-pause
// exit it would bury a just-parked paused_waiting_human (and the operator's
// pending question with it), and on the terminal exit it would replace the
// node's named cause with a generic one.
func TestFailedNodeSpendDoesNotSpeakForTheRun(t *testing.T) {
	// One token of headroom, and a failing node that burns far past it.
	build := func() *ir.Workflow {
		return &ir.Workflow{
			Name:    "spend_verdict_test",
			Entry:   "agent",
			Nodes:   map[string]ir.Node{"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}}, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
			Edges:   []*ir.Edge{{From: "agent", To: "done"}},
			Schemas: map[string]*ir.Schema{},
			Prompts: map[string]*ir.Prompt{},
			Vars:    map[string]*ir.Var{},
			Loops:   map[string]*ir.Loop{},
			Budget:  &ir.Budget{MaxTokens: 1_000},
		}
	}
	overBudgetFailure := func() *stubExecutor {
		exec := newStubExecutor()
		exec.on("agent", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"_tokens": 50_000, "_cost_usd": 9.99}, errors.New("stream closed")
		})
		return exec
	}

	t.Run("a recovery pause survives the booking", func(t *testing.T) {
		dispatch := RecoveryDispatch(func(_ context.Context, _ error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
			return RecoveryAction{Kind: RecoveryPauseForHuman, Reason: "ask the operator"}, ErrCodeExecutionFailed
		})
		st := tmpStore(t)
		eng := New(build(), st, overBudgetFailure(), WithRecoveryDispatch(dispatch))
		if err := eng.Run(context.Background(), "run-verdict-pause", nil); err != ErrRunPaused {
			t.Fatalf("expected the run to park on the recovery question, got %v", err)
		}
		r, err := st.LoadRun(context.Background(), "run-verdict-pause")
		if err != nil {
			t.Fatalf("load run: %v", err)
		}
		if r.Status != store.RunStatusPausedWaitingHuman {
			t.Fatalf("the booking overwrote the parked run: status %v, failure %q", r.Status, r.FailureCode)
		}
		if r.Checkpoint == nil || r.Checkpoint.InteractionID == "" {
			t.Fatal("the operator's pending recovery question was lost")
		}
		// …and the spend rode the checkpoint the resume reads its carry
		// from. Booked on the way OUT of handleNodeFailure it would be
		// zero here: pauseForRecovery has already written by then.
		if r.Checkpoint.BudgetTokensUsed != 50_000 {
			t.Fatalf("the parked attempt's spend never reached the checkpoint: %d", r.Checkpoint.BudgetTokensUsed)
		}
	})

	t.Run("a terminal failure keeps its own cause", func(t *testing.T) {
		st := tmpStore(t)
		eng := New(build(), st, overBudgetFailure())
		err := eng.Run(context.Background(), "run-verdict-fail", nil)
		if err == nil {
			t.Fatal("the run was supposed to fail")
		}
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) {
			t.Fatalf("expected a RuntimeError, got %T: %v", err, err)
		}
		if rtErr.Code != ErrCodeExecutionFailed {
			t.Fatalf("the booking replaced the node's cause: %s", rtErr.Code)
		}
		r, loadErr := st.LoadRun(context.Background(), "run-verdict-fail")
		if loadErr != nil {
			t.Fatalf("load run: %v", loadErr)
		}
		if r.FailureCode != store.FailureExecutionFailed {
			t.Fatalf("the persisted verdict is not the node's own: %s", r.FailureCode)
		}
		if r.Checkpoint == nil || r.Checkpoint.BudgetTokensUsed != 50_000 {
			t.Fatalf("the failed node's spend was not booked: %+v", r.Checkpoint)
		}
	})
}

// meteredFailingBackend is the shape a real delegate returns when a
// delegation dies: the error, and BESIDE it the result carrying what the
// pass burned — `typedFailure` allocates the output map and annotates the
// cost precisely so the figure survives the failure.
type meteredFailingBackend struct {
	calls int
}

func (b *meteredFailingBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.calls++
	return delegate.Result{
		Output:      map[string]any{"_tokens": 31_000, "_cost_usd": 4.75},
		Tokens:      31_000,
		BackendName: "metered_stub",
	}, errors.New("stream closed mid-session")
}

// The END of the chain, through the production executor rather than a
// runtime stub: a real ClawExecutor dispatching to a real delegate.Backend
// that fails after spending. Every layer below went to trouble to preserve
// the figure — typedFailure allocates and annotates, dispatchChain folds the
// abandoned routes' spend into the terminal result — and it only counts if
// it survives the last frame into the engine, which is the only place that
// books it against max_cost_usd, the org cap and a donor's ledger.
//
// A runtime-side stub cannot show this: it substitutes the very executor
// whose failure return is under test.
func TestFailedNodeSpendSurvivesTheProductionExecutor(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "metered_failure",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "agent"},
				LLMFields: ir.LLMFields{Backend: "metered_stub", Model: "anthropic/claude-opus-5"},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "agent", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}

	backend := &meteredFailingBackend{}
	reg := delegate.NewRegistry()
	reg.Register("metered_stub", backend)
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg),
		// One attempt: the assertion is about what ONE failed delegation
		// reports, not about what a retry ladder accumulates.
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
	)

	st := tmpStore(t)
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-metered-failure", nil); err == nil {
		t.Fatal("the run was supposed to fail")
	}
	if backend.calls != 1 {
		t.Fatalf("expected exactly one delegation, got %d", backend.calls)
	}

	r, err := st.LoadRun(context.Background(), "run-metered-failure")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if r.Checkpoint.BudgetTokensUsed != 31_000 {
		t.Fatalf("the failed delegation's tokens never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
	}
	if r.Checkpoint.BudgetCostUSD != 4.75 {
		t.Fatalf("the failed delegation's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
	}
}

// The call that raised ErrNeedsInteraction is deliberately NOT booked — "its
// spend is the resumed call's to report". That deferral is only honest if the
// resumed call books at its own terminal exit: otherwise a re-invocation that
// dies loses both sessions, the parked one and its own.
func TestFailedReInvocationBooksTheWholeSession(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Budget = &ir.Budget{MaxTokens: 1_000_000}
	calls := 0
	exec := newStubExecutor()
	exec.on("worker", func(map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{
				NodeID:    "worker",
				Questions: map[string]any{delegate.AskUserQuestionKey: "ok?"},
				SessionID: "sess-ask",
				Backend:   "claude_code",
			}
		}
		// The resumed call reports the SESSION — the parked call's spend
		// included — and then dies.
		return map[string]any{"_tokens": 9_000, "_cost_usd": 2.75}, errors.New("stream closed")
	})

	st := tmpStore(t)
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-reinvoke-spend", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("want the run parked on the question, got %v", err)
	}
	if err := eng.Resume(context.Background(), "run-reinvoke-spend",
		map[string]any{delegate.AskUserQuestionKey: "yes"}); err == nil {
		t.Fatal("the re-invocation was supposed to fail")
	}
	if calls != 2 {
		t.Fatalf("expected the parked call and its re-invocation, got %d", calls)
	}

	r, err := st.LoadRun(context.Background(), "run-reinvoke-spend")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if r.Checkpoint.BudgetTokensUsed != 9_000 {
		t.Fatalf("the re-invocation's session never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
	}
	if r.Checkpoint.BudgetCostUSD != 2.75 {
		t.Fatalf("the re-invocation's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
	}
}

// The llm half of llm_or_human degrades to a human pause rather than killing
// the run — but it can fail AFTER spending (a stream that dies mid-answer),
// and the human who answers next reports no tokens. Nothing downstream will
// ever re-report that call, so the pause's own checkpoint is the last place
// the figure can land.
func TestFailedHumanLLMHalfBooksItsSpendBeforeThePause(t *testing.T) {
	wf := humanModeWorkflow(ir.InteractionLLMOrHuman)
	wf.Budget = &ir.Budget{MaxTokens: 1_000_000}
	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "complex change"}, nil
	})
	exec.on("review", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"_tokens": 6_000, "_cost_usd": 1.80}, errors.New("stream closed mid-answer")
	})

	st := tmpStore(t)
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-human-llm-spend", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected the human half to take over, got %v", err)
	}
	r, err := st.LoadRun(context.Background(), "run-human-llm-spend")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("the booking disturbed the fallback: status %v", r.Status)
	}
	if r.Checkpoint == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if r.Checkpoint.BudgetTokensUsed != 6_000 {
		t.Fatalf("the failed llm half's spend never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
	}
	if r.Checkpoint.BudgetCostUSD != 1.80 {
		t.Fatalf("the failed llm half's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
	}
}

// ctxRefusingSpendStore is the shape a real remote ledger has: a write on a
// done context is refused, the way a Mongo write is. The filesystem store
// ignores ctx entirely, which is exactly what hides this class of bug
// locally.
type ctxRefusingSpendStore struct {
	*memSpendStore
	refused int
}

func (s *ctxRefusingSpendStore) AddSpend(ctx context.Context, date, runID string, cum float64) (*store.DailySpend, error) {
	if err := ctx.Err(); err != nil {
		s.refused++
		return nil, err
	}
	return s.memSpendStore.AddSpend(ctx, date, runID, cum)
}

// A teardown mid-node is the exit where the booked figure is most certainly
// final — the run is over, nothing will re-report it. It is also the one exit
// reached BECAUSE the run's context is done, so booking through that context
// hands the daily ledger a write it must refuse. The spend has to land anyway.
func TestFailedNodeSpendLandsInTheLedgerOnATornDownRun(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cancelled_spend",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "agent", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec := newStubExecutor()
	exec.on("agent", func(_ map[string]any) (map[string]any, error) {
		// The drain/operator cancel arrives while the node is executing;
		// the delegate returns the session's spend beside its error.
		cancel()
		return map[string]any{"_tokens": 12_000, "_cost_usd": 3.50}, errors.New("stream closed")
	})

	ledger := &ctxRefusingSpendStore{memSpendStore: newMemSpendStore()}
	guard := NewDailyCapGuard(ledger, clock.Default, DailyCapConfig{MaxCostPerDayUSD: 100})
	eng := New(wf, tmpStore(t), exec, WithDailyCap(guard))
	if err := eng.Run(ctx, "run-cancelled-spend", nil); err == nil {
		t.Fatal("the run was supposed to stop on the teardown")
	}

	if ledger.refused > 0 {
		t.Fatalf("the booking wrote through the run's own dead context: %d refused ledger write(s)", ledger.refused)
	}
	day := ledger.get(clock.DayKey(clock.Default.Now()))
	if got := day.RunsContributed["run-cancelled-spend"]; got != 3.50 {
		t.Fatalf("the torn-down run's spend never reached the daily ledger: %v", got)
	}
}

// The OTHER node kind that spends: an LLM router is a model call, and a
// failing one is special-dispatched — it never reaches execLoopRunNode, so
// the standard path's booking cannot cover it. Both frames had to be fixed
// for the figure to survive: the executor returned a bare nil beside the
// error, and the engine's router path dropped whatever it was handed.
func TestFailedLLMRouterSpendReachesTheRun(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "metered_router_failure",
		Entry: "route",
		Nodes: map[string]ir.Node{
			"route": &ir.RouterNode{
				BaseNode:   ir.BaseNode{ID: "route"},
				LLMFields:  ir.LLMFields{Backend: "metered_stub", Model: "anthropic/claude-opus-5"},
				RouterMode: ir.RouterLLM,
			},
			"a":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "route", To: "a"}, {From: "route", To: "b"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}

	backend := &meteredFailingBackend{}
	reg := delegate.NewRegistry()
	reg.Register("metered_stub", backend)
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
	)

	st := tmpStore(t)
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-metered-router", nil); err == nil {
		t.Fatal("the run was supposed to fail on the router")
	}
	if backend.calls != 1 {
		t.Fatalf("expected exactly one delegation, got %d", backend.calls)
	}

	r, err := st.LoadRun(context.Background(), "run-metered-router")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if r.Checkpoint.BudgetTokensUsed != 31_000 {
		t.Fatalf("the failed router's tokens never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
	}
	if r.Checkpoint.BudgetCostUSD != 4.75 {
		t.Fatalf("the failed router's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
	}
}
