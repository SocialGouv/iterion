package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/clock"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// checkpointCapturingStore keeps the last checkpoint the engine wrote, which
// is where a resume reads the budget carry from. Guarded: under a fan-out the
// writers are branch goroutines. onSave, when set, runs on every checkpoint
// write and lets a test order itself against the engine's own progress.
type checkpointCapturingStore struct {
	store.RunStore
	mu     sync.Mutex
	last   *store.Checkpoint
	onSave func(*store.Checkpoint)
}

func (s *checkpointCapturingStore) capture(cp *store.Checkpoint) {
	s.mu.Lock()
	s.last = cp
	hook := s.onSave
	s.mu.Unlock()
	if hook != nil {
		hook(cp)
	}
}

func (s *checkpointCapturingStore) lastCheckpoint() *store.Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *checkpointCapturingStore) SaveCheckpoint(ctx context.Context, runID string, cp *store.Checkpoint) error {
	s.capture(cp)
	return s.RunStore.SaveCheckpoint(ctx, runID, cp)
}

func (s *checkpointCapturingStore) PauseRun(ctx context.Context, id string, cp *store.Checkpoint) error {
	if cp != nil {
		s.capture(cp)
	}
	return s.RunStore.PauseRun(ctx, id, cp)
}

func (s *checkpointCapturingStore) FailRunResumable(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	if cp != nil {
		s.capture(cp)
	}
	return s.RunStore.FailRunResumable(ctx, id, cp, runErr, code)
}

func (s *checkpointCapturingStore) FailRunTerminal(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	if cp != nil {
		s.capture(cp)
	}
	return s.RunStore.FailRunTerminal(ctx, id, cp, runErr, code)
}

// A node that FAILED still spent. The delegate stamps the pass's cost on the
// result it returns beside the error; until this landed, nothing read it —
// `recordBudget` runs on the success path only, so the run's budget and the
// daily cap both missed whatever the failing node burned. On a long agent
// node that is a whole session. (recordFailedNodeSpend's doc states the
// booking's reach and where it stops.)
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

	// A node that spent nothing books nothing — no phantom zero rows. The
	// ITERATIONS axis is the oracle here, not the tokens: a zero booking
	// cannot move a total by construction, so asserting on tokens alone
	// would pass with the guard deleted. RecordUsage advances iterationsUsed
	// unconditionally, so a phantom row shows up there and only there — and
	// that row is a max_iterations slot a spendless tool failure must not
	// consume.
	beforeTokens, _, beforeIters, _, _, _ := shared.Snapshot()
	engine.recordFailedNodeSpend(rs, "tool", map[string]any{"ok": true})
	engine.recordFailedNodeSpend(rs, "tool", nil)
	afterTokens, _, afterIters, _, _, _ := shared.Snapshot()
	if afterTokens != beforeTokens {
		t.Fatalf("a spendless failure moved the totals: %d -> %d", beforeTokens, afterTokens)
	}
	if afterIters != beforeIters {
		t.Fatalf("a spendless failure burned %d max_iterations slot(s)", afterIters-beforeIters)
	}
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
		cp := st.lastCheckpoint()
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
		cp := st.lastCheckpoint()
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

// meteredFailingBackend is the shape a delegate returns when a delegation
// dies: the error, and BESIDE it the result carrying what the pass burned.
// Each shipped backend reaches that shape its own way — claude_code through
// `typedFailure`, which allocates the output map and annotates the cost on a
// typed refusal; claw through `meteredFailure` over the partial result its
// generation layer returns beside the error. That the SHIPPED ones actually
// fill it is pinned where they live
// (model.TestClawBackendKeepsWhatAnAbandonedGenerationBurned); what this stub
// pins is the frame above them.
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
// runtime stub: a real ClawExecutor dispatching a registered delegate that
// fails after spending. Every layer below went to trouble to preserve the
// figure — the backends stamp it on their failure result, dispatchChain folds
// the abandoned routes' spend into the terminal one — and it only counts if
// it survives the last frame into the engine, which is the only place that
// books it against max_cost_usd and the daily-cap ledger.
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

// meteredInvalidBackend SUCCEEDS — a served, paid-for delegation — and hands
// back output whose `verdict` is a string where the schema declares a bool. A
// type mismatch is deliberately NOT retry-eligible (the model returns it in a
// stable shape), so the node dies on its schema after exactly one session.
type meteredInvalidBackend struct {
	calls int
}

func (b *meteredInvalidBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.calls++
	return delegate.Result{
		Output:      map[string]any{"verdict": "not-a-bool", "_tokens": 22_000, "_cost_usd": 3.30},
		Tokens:      22_000,
		BackendName: "metered_stub",
	}, nil
}

// The dispatch-failure seam above is only half the surface: a delegation can
// SUCCEED and the node still fail, on the schema check that runs after it. The
// spend is identical — a whole served session, and on the after-retry return
// two of them — but the executor returned a bare nil there, one frame further
// in than the return the rest of this file is about, so every booking
// downstream was inert on it.
//
// This is the failure mode that costs the most: a node that dies on its schema
// has paid for every token the model emitted, and a weak or degraded model is
// both the likeliest to emit unusable shape and the likeliest to have been
// reached through a fallback chain that burned other routes first.
func TestPostDispatchValidationFailureStillBooksItsSpend(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "metered_invalid_output",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{
				BaseNode:     ir.BaseNode{ID: "agent"},
				LLMFields:    ir.LLMFields{Backend: "metered_stub", Model: "anthropic/claude-opus-5"},
				SchemaFields: ir.SchemaFields{OutputSchema: "verdict_output"},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "agent", To: "done"}},
		Schemas: map[string]*ir.Schema{
			"verdict_output": {Name: "verdict_output", Fields: []*ir.SchemaField{
				{Name: "verdict", Type: ir.FieldTypeBool},
			}},
		},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}

	backend := &meteredInvalidBackend{}
	reg := delegate.NewRegistry()
	reg.Register("metered_stub", backend)
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
	)

	st := tmpStore(t)
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-metered-invalid", nil); err == nil {
		t.Fatal("the run was supposed to fail on the schema")
	}
	if backend.calls != 1 {
		t.Fatalf("a type mismatch is not retry-eligible; expected one delegation, got %d", backend.calls)
	}

	r, err := st.LoadRun(context.Background(), "run-metered-invalid")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if r.Checkpoint.BudgetTokensUsed != 22_000 {
		t.Fatalf("the served session's tokens never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
	}
	if r.Checkpoint.BudgetCostUSD != 3.30 {
		t.Fatalf("the served session's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
	}
}

// meteredUnparseableBackend takes the router's OTHER post-dispatch exit: the
// backend could not emit JSON at all, the executor wrapped its prose in a
// `text` field, and the extraction attempt fails too. The prose was still
// generated and still billed.
type meteredUnparseableBackend struct {
	calls int
}

func (b *meteredUnparseableBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.calls++
	return delegate.Result{
		Output:        map[string]any{"text": "I think we should go left.", "_tokens": 22_000, "_cost_usd": 3.30},
		Tokens:        22_000,
		ParseFallback: true,
		BackendName:   "metered_stub",
	}, nil
}

// The same post-dispatch hole on the router seam: `{"verdict": …}` carries no
// `route`, so the router's own schema rejects it after a served, metered
// delegation. Both of that function's post-dispatch returns were bare nils, so
// the sub-test below drives the other one — unparseable prose after the text
// wrapper — through the same assertion.
func TestPostDispatchRouterValidationFailureStillBooksItsSpend(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "metered_invalid_router",
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

	for _, tc := range []struct {
		name    string
		backend delegate.Backend
	}{
		{"schema invalid", &meteredInvalidBackend{}},
		{"unparseable after the text wrapper", &meteredUnparseableBackend{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := delegate.NewRegistry()
			reg.Register("metered_stub", tc.backend)
			exec := model.NewClawExecutor(model.NewRegistry(), wf,
				model.WithBackendRegistry(reg),
				model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
			)

			st := tmpStore(t)
			eng := New(wf, st, exec)
			runID := "run-metered-invalid-router"
			if err := eng.Run(context.Background(), runID, nil); err == nil {
				t.Fatal("the run was supposed to fail on the router's output")
			}

			r, err := st.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if r.Checkpoint == nil {
				t.Fatal("no checkpoint to read the budget from")
			}
			if r.Checkpoint.BudgetTokensUsed != 22_000 {
				t.Fatalf("the served router session's tokens never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
			}
			if r.Checkpoint.BudgetCostUSD != 3.30 {
				t.Fatalf("the served router session's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
			}
		})
	}
}

// A fan-out branch is where the most expensive agent work in a run tends to
// happen, and it never reaches execLoopRunNode — so the trunk's booking
// cannot cover it. A branch node that fails is terminal for its branch:
// nothing retries it in place, and the trunk aggregates the failure without
// ever seeing the output, so this is the last frame that can book what it
// burned.
// Booking it in memory is only half: a resume reads the carry from the LAST
// CHECKPOINT, and the failing branch writes none — execBranch returns the
// moment the node fails. The order below is imposed rather than raced: the
// sibling's completion checkpoint lands first, taken before the failure books
// anything, and the failing branch then has to make its own spend durable. Left
// to the scheduler this passes most runs — the sibling usually checkpoints
// after the booking — and the same run silently loses a whole failed session's
// spend on the interleavings where it does not.
func TestFailedBranchNodeSpendReachesTheRun(t *testing.T) {
	wf := budgetFanOutWorkflow(&ir.Budget{MaxTokens: 1_000_000, MaxParallelBranches: 2})
	st := &checkpointCapturingStore{RunStore: tmpStore(t)}

	siblingBooked := make(chan struct{})
	var once sync.Once
	st.onSave = func(cp *store.Checkpoint) {
		if cp != nil && cp.BudgetTokensUsed == 1_000 {
			once.Do(func() { close(siblingBooked) })
		}
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 1_000, "_cost_usd": 0.10}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		select {
		case <-siblingBooked:
		case <-time.After(30 * time.Second):
			t.Error("the sibling never checkpointed its own spend; the ordering this test rests on is gone")
		}
		return map[string]any{"_tokens": 15_000, "_cost_usd": 4.20}, errors.New("stream closed mid-branch")
	})

	eng := New(wf, st, exec)
	// best_effort convergence: the sibling carries the run, so the failing
	// branch's spend has to land WITHOUT the run failing to make it visible.
	if err := eng.Run(context.Background(), "run-branch-spend", nil); err != nil {
		t.Fatalf("the best-effort join was supposed to carry the run: %v", err)
	}

	cp := st.lastCheckpoint()
	if cp == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	// 15_000 (the failed branch) + 1_000 (its sibling); the entry node spent
	// nothing. Without the booking the run reads 1_000 and the operator is
	// billed for a session the totals never saw.
	if cp.BudgetTokensUsed != 16_000 {
		t.Fatalf("the failed branch's session never reached the run: %d tokens", cp.BudgetTokensUsed)
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

// proseOnlyClient answers a STRUCTURED request with plain prose and no
// tool_use block — the shape a model produces when it ignores the schema, and
// one of the three post-aggregate failures of GenerateObjectDirect. The stream
// completes and reports its usage first, so the tokens below are billed.
type proseOnlyClient struct {
	mu    sync.Mutex
	calls int
}

func (c *proseOnlyClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	ch := make(chan api.StreamEvent, 8)
	go func() {
		defer close(ch)
		ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 4_000}
		ch <- api.StreamEvent{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}}
		ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "I'd rather not answer in JSON."}}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 2_000}}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()
	return ch, nil
}

// The test above drives a runtime stub, and a stub can return whatever shape
// the assertion wants. Production could not return that shape: executeHumanLLM
// went through GenerateObjectDirect, which answers `nil, err` on every failure
// path of its own — so the booking was handed nil, extractUsage(nil) gave
// (0, 0), and the call returned at its own zero guard. The seam the branch
// claimed to close was inert (prior review R1c7030).
//
// This drives the REAL executor against a client that streams a complete,
// billed answer and then fails the structured parse, and reads the figure back
// off the checkpoint. `_cost_usd` is asserted as well as `_tokens`: this path
// annotates through cost.Annotate with the in/out split still in hand, which
// is more than the delegate seams' `_tokens`-only stamp can manage.
func TestFailedHumanLLMHalfBooksItsSpendInProduction(t *testing.T) {
	wf := humanModeWorkflow(ir.InteractionLLMOrHuman)
	wf.Budget = &ir.Budget{MaxTokens: 1_000_000}
	// The stub executor cannot serve the entry agent here (a real executor is
	// under test), so the human node is the entry.
	wf.Entry = "review"

	client := &proseOnlyClient{}
	reg := model.NewRegistry()
	// A PRICED spec, standing in for the provider's client: `_cost_usd` can
	// only be asserted on a model the cost table knows, and pricing it is
	// half of what this path does.
	reg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })
	if n, ok := wf.Nodes["review"].(*ir.HumanNode); ok {
		n.Model = "anthropic/claude-opus-5"
	}
	exec := model.NewClawExecutor(reg, wf, model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))

	st := tmpStore(t)
	eng := New(wf, st, exec)
	// A non-empty input: the node is the entry here, and a generation with no
	// user message is refused before the client is ever reached.
	if err := eng.Run(context.Background(), "run-human-llm-production",
		map[string]any{"summary": "complex change"}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected the human half to take over, got %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("expected exactly one generation, got %d", client.calls)
	}

	r, err := st.LoadRun(context.Background(), "run-human-llm-production")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Checkpoint == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if r.Checkpoint.BudgetTokensUsed != 6_000 {
		t.Fatalf("the failed generation's tokens never reached the run: %d (0 means the seam is still inert)", r.Checkpoint.BudgetTokensUsed)
	}
	if r.Checkpoint.BudgetCostUSD <= 0 {
		t.Fatalf("the failed generation's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
	}
}

// meteredCtxBlockingExecutor is main's ctxBlockingExecutor with the one thing
// this file is about: the hung node reports what it burned before the deadline
// cut it off.
type meteredCtxBlockingExecutor struct {
	blockNode string
}

func (e *meteredCtxBlockingExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() == e.blockNode {
		<-ctx.Done()
		return map[string]any{"_tokens": 12_000, "_cost_usd": 3.10}, ctx.Err()
	}
	return map[string]any{"ok": true}, nil
}

// The duration-deadline exit is its own terminal return — it never reaches
// handleNodeFailure, so the booking threaded through that call cannot cover
// it. Neither side of this branch's merge tested it: the spend tests have no
// MaxDuration case, and main's deadline tests are about ATTRIBUTION (is this
// DeadlineExceeded really ours?), not about accounting. Dropping the booking
// while resolving the conflict there would have left a green tree.
//
// A node killed by max_duration is also the single most expensive way for one
// to die — it ran for the entire remaining budget — so it is the last one that
// can afford to be forgotten.
func TestBudgetDeadlineFailureBooksWhatTheNodeBurned(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "deadline_spend",
		Entry: "a",
		Nodes: map[string]ir.Node{
			// "a" is fast, so a checkpoint exists before "slow" dies and the
			// failure is resumable rather than first-node terminal.
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"slow": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "slow"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "a", To: "slow"}, {From: "slow", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		// Long enough to reach "slow", short enough that the run's remaining
		// duration is genuinely spent when the deadline fires — main's rule is
		// that the deadline must have ELAPSED, not merely be close.
		Budget: &ir.Budget{MaxDuration: "300ms"},
	}

	st := tmpStore(t)
	eng := New(wf, st, &meteredCtxBlockingExecutor{blockNode: "slow"})
	err := eng.Run(context.Background(), "run-deadline-spend", nil)
	if err == nil {
		t.Fatal("the duration deadline was supposed to end the run")
	}
	if !strings.Contains(err.Error(), "budget exceeded") || !strings.Contains(err.Error(), "duration") {
		t.Fatalf("this test rests on taking the duration-deadline exit, got: %v", err)
	}

	r, loadErr := st.LoadRun(context.Background(), "run-deadline-spend")
	if loadErr != nil {
		t.Fatalf("load run: %v", loadErr)
	}
	if r.Checkpoint == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if r.Checkpoint.BudgetTokensUsed != 12_000 {
		t.Fatalf("the timed-out node's tokens never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
	}
	if r.Checkpoint.BudgetCostUSD != 3.10 {
		t.Fatalf("the timed-out node's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
	}
}

// askingBackend parks on a question the way a delegate's ask_user does, with
// no spend of its own to report — so the only figure in the run below is the
// interaction LLM's, which is exactly what is being measured.
type askingBackend struct{ calls int }

func (b *askingBackend) Execute(context.Context, delegate.Task) (delegate.Result, error) {
	b.calls++
	return delegate.Result{}, &model.ErrNeedsInteraction{
		NodeID:    "worker",
		Questions: map[string]any{delegate.AskUserQuestionKey: "which database?"},
		SessionID: "sess-ask",
		Backend:   "metered_stub",
	}
}

// The same seam once more, from the OTHER surface that uses it: `interaction:
// llm` on an agent node auto-answers the delegate's ask_user through the very
// generation just fixed. Neither of the two engine call sites booked at all —
// the branch wired eight sites and missed these — so a failed auto-answer, a
// real billed call, left no trace on the run at all.
func TestFailedInteractionLLMBooksItsSpend(t *testing.T) {
	for _, mode := range []ir.InteractionMode{ir.InteractionLLM, ir.InteractionLLMOrHuman} {
		t.Run(mode.String(), func(t *testing.T) {
			wf := interactionWorkflow(mode)
			wf.Budget = &ir.Budget{MaxTokens: 1_000_000}
			worker := wf.Nodes["worker"].(*ir.AgentNode)
			worker.Backend = "metered_stub"
			worker.Model = "anthropic/claude-opus-5"
			worker.InteractionModel = "anthropic/claude-opus-5"

			client := &proseOnlyClient{}
			modelReg := model.NewRegistry()
			modelReg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })
			delReg := delegate.NewRegistry()
			delReg.Register("metered_stub", &askingBackend{})
			exec := model.NewClawExecutor(modelReg, wf,
				model.WithBackendRegistry(delReg),
				model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
			)

			st := tmpStore(t)
			eng := New(wf, st, exec)
			runID := "run-interaction-llm-spend"
			if err := eng.Run(context.Background(), runID, map[string]any{"task": "pick one"}); err == nil {
				t.Fatal("the run was supposed to fail on the interaction LLM")
			}
			if client.calls != 1 {
				t.Fatalf("expected exactly one auto-answer generation, got %d", client.calls)
			}

			r, err := st.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if r.Checkpoint == nil {
				t.Fatal("no checkpoint to read the budget from")
			}
			if r.Checkpoint.BudgetTokensUsed != 6_000 {
				t.Fatalf("the failed auto-answer's tokens never reached the run: %d", r.Checkpoint.BudgetTokensUsed)
			}
			if r.Checkpoint.BudgetCostUSD <= 0 {
				t.Fatalf("the failed auto-answer's cost never reached the run: %v", r.Checkpoint.BudgetCostUSD)
			}
		})
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

// The class is wider than a failing node: ANY branch exit that writes no
// checkpoint leaves the spend booked before it in memory only. A branch that
// spends and then reaches a fail node never calls recordFailedBranchSpend —
// its last node SUCCEEDED — so the accounting write never arms, and the run's
// durable budget forgets a whole branch's session exactly as it did for a
// failed node. Same forced ordering: the sibling checkpoints its own 1_000
// before this branch books anything.
func TestBranchSpendBeforeAFailNodeReachesTheRun(t *testing.T) {
	wf := budgetFanOutWorkflow(&ir.Budget{MaxTokens: 1_000_000, MaxParallelBranches: 2})
	for _, e := range wf.Edges {
		if e.From == "a" && e.To == "done" {
			e.To = "fail"
		}
	}
	st := &checkpointCapturingStore{RunStore: tmpStore(t)}

	siblingBooked := make(chan struct{})
	var once sync.Once
	st.onSave = func(cp *store.Checkpoint) {
		if cp != nil && cp.BudgetTokensUsed == 1_000 {
			once.Do(func() { close(siblingBooked) })
		}
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 1_000, "_cost_usd": 0.10}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		select {
		case <-siblingBooked:
		case <-time.After(30 * time.Second):
			t.Error("the sibling never checkpointed its own spend; the ordering this test rests on is gone")
		}
		return map[string]any{"ok": true, "_tokens": 15_000, "_cost_usd": 4.20}, nil
	})

	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-branch-failnode-spend", nil); err != nil {
		t.Fatalf("the best-effort join was supposed to carry the run: %v", err)
	}

	cp := st.lastCheckpoint()
	if cp == nil {
		t.Fatal("no checkpoint to read the budget from")
	}
	if cp.BudgetTokensUsed != 16_000 {
		t.Fatalf("the fail-node branch's session never reached the run: %d tokens", cp.BudgetTokensUsed)
	}
}

// The accounting write is a full checkpoint, so it must carry everything a
// checkpoint owns — including the pause pointer of a SIBLING. A run parked on
// a human gate names its interaction in cp.InteractionID, which is what resume
// reads to find the answer; a booking write that omitted it would answer "no
// interaction pending" for a run that has one, and the parked branch would be
// unreachable.
func TestBranchSpendWriteKeepsASiblingsPausePointer(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "fanout_gate_and_spend",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"pre":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "pre"}},
			"gate":    &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"a":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"collect": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitBestEffort},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "pre"},
			{From: "pre", To: "gate"},
			{From: "router", To: "a"},
			{From: "gate", To: "collect", Condition: "approved"},
			{From: "a", To: "collect"},
			{From: "collect", To: "done"},
		},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
		Loops:     map[string]*ir.Loop{},
		Foreaches: map[string]*ir.Foreach{},
		Budget:    &ir.Budget{MaxTokens: 1_000_000, MaxParallelBranches: 2},
	}

	st := &checkpointCapturingStore{RunStore: tmpStore(t)}
	parked := make(chan struct{})
	var once sync.Once
	st.onSave = func(cp *store.Checkpoint) {
		if cp != nil && cp.InteractionID != "" {
			once.Do(func() { close(parked) })
		}
	}

	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	aStarted := make(chan struct{})
	exec.on("pre", func(map[string]any) (map[string]any, error) {
		// Park only once the sibling is inside its node: a gate that parks
		// first cancels the sibling before it can spend anything.
		select {
		case <-aStarted:
		case <-time.After(30 * time.Second):
			t.Error("the spending sibling never started")
		}
		return map[string]any{"ok": true}, nil
	})
	exec.on("a", func(map[string]any) (map[string]any, error) {
		close(aStarted)
		select {
		case <-parked:
		case <-time.After(30 * time.Second):
			t.Error("the sibling never parked; the ordering this test rests on is gone")
		}
		return map[string]any{"_tokens": 15_000, "_cost_usd": 4.20}, errors.New("cancelled mid-branch")
	})

	eng := New(wf, st, exec)
	err := eng.Run(context.Background(), "run-spend-write-keeps-pause", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("the gate was supposed to park the run: %v", err)
	}

	cp := st.lastCheckpoint()
	if cp == nil {
		t.Fatal("no checkpoint at all")
	}
	if cp.InteractionID == "" {
		t.Fatal("the last checkpoint no longer names the pending interaction: the parked branch is unreachable on resume")
	}
}

// refusingCheckpointStore fails every SaveCheckpoint, which is what a branch
// boundary hits when the store is momentarily unreachable.
type refusingCheckpointStore struct {
	store.RunStore
}

func (s *refusingCheckpointStore) SaveCheckpoint(context.Context, string, *store.Checkpoint) error {
	return errors.New("store refused the checkpoint")
}

// spendUncheckpointed is the flag that sends a branch's booking to
// persistBranchSpend on the way out. Only a checkpoint that actually became
// durable may clear it: clearing it on a write that FAILED suppresses the
// retry precisely when it is the thing that saves the figure.
//
// The regression this pins is a condition, not a typo: the save error and the
// logger were folded into one `err != nil && e.logger != nil`, so on the
// DEFAULT engine — neither New nor NewFromRecipe wires a logger — a failed
// write fell through to the else arm and cleared the flag. Every engine these
// tests build takes that arm, so the buggy path was the one under test.
func TestBranchSpendSurvivesARefusedCheckpoint(t *testing.T) {
	wf := budgetFanOutWorkflow(&ir.Budget{MaxTokens: 1_000_000, MaxParallelBranches: 2})
	eng := New(wf, &refusingCheckpointStore{RunStore: tmpStore(t)}, newStubExecutor())
	if eng.logger != nil {
		t.Fatal("this test rests on the default engine having no logger")
	}

	parent := &runState{
		ctx:             context.Background(),
		runID:           "run-refused-checkpoint",
		budget:          newSharedBudget(wf.Budget, eng.logger),
		loopBudgetMarks: make(map[string]loopBudgetMark),
	}
	branchRS := &runState{
		ctx:             parent.ctx,
		runID:           parent.runID,
		budget:          parent.budget,
		loopBudgetMarks: make(map[string]loopBudgetMark),
	}
	parallel := newParallelInvocation("router", "router@root", map[string]string{"branch_router_0": "a"}, nil)
	result := &branchResult{
		branchID:            "branch_router_0",
		startNodeID:         "a",
		spendUncheckpointed: true,
	}

	eng.checkpointBranchState(parent, branchRS, result, "a", true, parallel, false)

	if !result.spendUncheckpointed {
		t.Fatal("a REFUSED checkpoint cleared the durability flag: the booking is now neither durable nor scheduled for persistBranchSpend, and the resume is handed back an allowance the run already burned")
	}
}

// The failing branch's booking told the daily-cap ledger a new cumulative
// figure under the branch's own key, and that ledger is MONOTONIC-MAX per
// key. The durable branch cursor a resume re-seeds from lives in the
// checkpoint, and the accounting write must carry it: left at its pre-failure
// value, the resumed branch restarts BELOW the high-water mark the failed
// attempt already wrote, and every contribution it makes afterwards is
// discarded — several calls, not one — until the recomputed cumulative
// overtakes it.
func TestFailedBranchSpendCarriesTheDurableCostCursor(t *testing.T) {
	wf := budgetFanOutWorkflow(&ir.Budget{MaxTokens: 1_000_000, MaxParallelBranches: 2})
	// Branch "a" gets a second node so it SPENDS on a linear edge (which
	// checkpoints nothing) and then fails: the cursor the run must keep is
	// the sum of both, not the failing node's alone.
	wf.Nodes["a2"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a2"}}
	for _, e := range wf.Edges {
		if e.From == "a" && e.To == "done" {
			e.To = "a2"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "a2", To: "done"})

	st := &checkpointCapturingStore{RunStore: tmpStore(t)}
	var mu sync.Mutex
	cursors := []float64{}
	st.onSave = func(cp *store.Checkpoint) {
		if cp == nil || cp.Parallel == nil {
			return
		}
		if b := cp.Parallel.Branches["branch_router_a"]; b != nil {
			mu.Lock()
			cursors = append(cursors, b.CostUSD)
			mu.Unlock()
		}
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 1_000, "_cost_usd": 0.10}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 2_000, "_cost_usd": 1.00}, nil
	})
	exec.on("a2", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"_tokens": 15_000, "_cost_usd": 4.20}, errors.New("stream closed mid-branch")
	})

	ledger := newMemSpendStore()
	guard := NewDailyCapGuard(ledger, clock.Default, DailyCapConfig{MaxCostPerDayUSD: 1_000})
	eng := New(wf, st, exec, WithDailyCap(guard))
	if err := eng.Run(context.Background(), "run-branch-cursor", nil); err != nil {
		t.Fatalf("the best-effort join was supposed to carry the run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cursors) == 0 {
		t.Fatal("no checkpoint ever carried the failing branch — the accounting write never happened")
	}
	// 1.00 (the linear node) + 4.20 (the failure) — what the ledger was told.
	if last := cursors[len(cursors)-1]; last != 5.20 {
		t.Fatalf("the durable branch cost cursor lags the ledger: checkpoint %v, ledger %v",
			last, ledger.get(clock.DayKey(clock.Default.Now())).RunsContributed)
	}
}
