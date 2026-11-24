package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// eventNumber reads a numeric event field back from the store, where JSON
// decoding has made every number a float64.
func eventNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return -1
}

// reaskBackend answers a schema-bearing node once WITHOUT a required field,
// then — asked again — with it. It names the session it ran, the shape a
// backend that resumes by id reports, and records every task it was handed.
type reaskBackend struct {
	tasks []delegate.Task
}

func (b *reaskBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	if len(b.tasks) == 1 {
		return delegate.Result{
			CostUSD:     0.63,
			Output:      map[string]any{"reply": "here is my answer", "_tokens": 83_000, "_cost_usd": 0.63},
			Tokens:      83_000,
			SessionID:   "sess-copi",
			BackendName: delegate.BackendClaudeCode,
		}, nil
	}
	return delegate.Result{
		CostUSD:     0.01,
		Output:      map[string]any{"reply": "here is my answer", "requires_authoring_context": false, "_tokens": 900, "_cost_usd": 0.01},
		Tokens:      900,
		SessionID:   "sess-copi",
		BackendName: delegate.BackendClaudeCode,
	}, nil
}

// The shape of #1385: a structured answer missing one required boolean at
// the end of a session. The engine's node must end on the re-asked answer —
// never failed_resumable, which cost the operator a resume and the whole
// turn again — and the run's record must name the re-ask and its outcome.
// Driven through the production ClawExecutor and a real delegate.Backend so
// the path under test is the one a run takes.
func TestSchemaReaskDeliversTheNodeInsteadOfFailingResumable(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "reask_test",
		Entry: "copi",
		Nodes: map[string]ir.Node{
			"copi": &ir.AgentNode{
				BaseNode:     ir.BaseNode{ID: "copi"},
				LLMFields:    ir.LLMFields{Backend: delegate.BackendClaudeCode, Model: "claude-opus-5"},
				SchemaFields: ir.SchemaFields{OutputSchema: "copi_turn"},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "copi", To: "done"}},
		Schemas: map[string]*ir.Schema{"copi_turn": {
			Name: "copi_turn",
			Fields: []*ir.SchemaField{
				{Name: "reply", Type: ir.FieldTypeString},
				{Name: "requires_authoring_context", Type: ir.FieldTypeBool},
			},
		}},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}
	backend := &reaskBackend{}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, backend)
	st := tmpStore(t)
	ctx := context.Background()
	// The store hooks are what turn the executor's lifecycle hooks into the
	// run's events — the record this test reads the re-ask back from.
	hooks := model.NewStoreEventHooks(ctx, st, "run-reask", iterlog.New(iterlog.LevelError, io.Discard), model.BuildSecretGuard(ctx, wf, nil, nil), nil)
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg),
		model.WithEventHooks(hooks),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
	)
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-reask", nil); err != nil {
		t.Fatalf("the re-ask was supposed to carry the run through, got: %v", err)
	}
	run, err := st.LoadRun(context.Background(), "run-reask")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("run status = %s, want finished (a failed_resumable here is the outcome #1385 reported)", run.Status)
	}
	if len(backend.tasks) != 2 {
		t.Fatalf("expected the answer and one re-ask, got %d delegation(s)", len(backend.tasks))
	}
	// The re-ask resumed the session the answer ran in and asked only for
	// the fix — it did not run the turn again.
	reask := backend.tasks[1]
	if reask.SessionID != "sess-copi" || reask.ForkSession {
		t.Errorf("the re-ask did not resume the answer's session: session=%q fork=%v", reask.SessionID, reask.ForkSession)
	}
	if !strings.Contains(reask.UserPrompt, "requires_authoring_context") {
		t.Errorf("the re-ask does not name the missing field: %q", reask.UserPrompt)
	}

	events, err := st.LoadEvents(context.Background(), "run-reask")
	if err != nil {
		t.Fatal(err)
	}
	var announced, ended bool
	for _, ev := range events {
		switch ev.Type {
		case store.EventDelegateRetry:
			msg, _ := ev.Data["error"].(string)
			if ev.Data["reask"] == model.ReaskResumeSession && strings.Contains(msg, "requires_authoring_context") {
				announced = true
			}
		case store.EventDelegateFinished:
			if eventNumber(ev.Data["attempt"]) == 2 && ev.Data["reask"] == model.ReaskResumeSession && eventNumber(ev.Data["tokens"]) == 900 {
				ended = true
			}
		}
	}
	if !announced {
		t.Error("no delegate_retry event names the re-ask (reask mode + the validation error)")
	}
	if !ended {
		t.Error("no delegate_finished event marks the re-ask's own end (attempt 2, its own tokens)")
	}
}

type schemaViolationBackend struct {
	tasks   []delegate.Task
	validAt int
}

func (b *schemaViolationBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	verdict := fmt.Sprintf("wrong-%d", len(b.tasks))
	if b.validAt > 0 && len(b.tasks) >= b.validAt {
		verdict = "pass"
	}
	return delegate.Result{Output: map[string]any{"summary": verdict, "score": 1}, Tokens: 10, CostUSD: 0.01, BackendName: delegate.BackendClaudeCode, SessionID: "test-session"}, nil
}

func TestSchemaReaskUsesRuntimeBudgetAndEvents(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		budget, validAt, calls int
		succeeds               bool
	}{
		{"enum corrected", 2, 2, 2, true},
		{"second correction", 2, 3, 3, true},
		{"exhausted", 2, 0, 3, false},
		{"disabled", 0, 0, 1, false},
		{"one correction", 1, 0, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := validationWorkflow()
			node := wf.Nodes["my_agent"].(*ir.AgentNode)
			node.Backend = delegate.BackendClaudeCode
			wf.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
			b := &schemaViolationBackend{validAt: tc.validAt}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			st := tmpStore(t)
			exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			genericRetries := 0
			eng := New(wf, st, exec, WithOutputValidation(true), WithOutputCorrectionBudget(tc.budget), WithRecoveryDispatch(func(_ context.Context, err error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
				var rt *RuntimeError
				if errors.As(err, &rt) && rt.Code == ErrCodeSchemaValidation {
					return RecoveryAction{Kind: RecoveryFailTerminal}, rt.Code
				}
				genericRetries++
				if prior(ErrCodeExecutionFailed) == 0 {
					return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeExecutionFailed
				}
				return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeExecutionFailed
			}))
			err := eng.Run(context.Background(), "schema-loop", nil)
			if tc.succeeds && err != nil {
				t.Fatal(err)
			}
			if !tc.succeeds {
				var rt *RuntimeError
				if !errors.As(err, &rt) || rt.Code != ErrCodeSchemaValidation {
					t.Fatalf("want typed schema failure, got %v", err)
				}
			}
			if len(b.tasks) != tc.calls || genericRetries != 0 {
				t.Fatalf("calls=%d, generic retries=%d; want %d/0", len(b.tasks), genericRetries, tc.calls)
			}
			for _, task := range b.tasks[1:] {
				if !strings.Contains(task.UserPrompt, "not in enum") {
					t.Fatalf("feedback missing: %q", task.UserPrompt)
				}
			}
			run, err := st.LoadRun(context.Background(), "schema-loop")
			if err != nil {
				t.Fatal(err)
			}
			episode := run.OutputCorrections["my_agent"]
			if episode.Attempts != tc.calls-1 || episode.Budget != tc.budget {
				t.Fatalf("episode=%+v", episode)
			}
			events, err := st.LoadEvents(context.Background(), "schema-loop")
			if err != nil {
				t.Fatal(err)
			}
			attemptEvents := 0
			for _, event := range events {
				if event.Type == store.EventNodeRecovery && event.Data["strategy"] == "schema_output_correction" && event.Data["phase"] == "attempt" {
					attemptEvents++
				}
			}
			if attemptEvents != tc.calls-1 {
				t.Fatalf("correction events=%d want %d", attemptEvents, tc.calls-1)
			}
			if !tc.succeeds {
				_ = eng.Resume(context.Background(), "schema-loop", nil)
				if len(b.tasks) != tc.calls+1 {
					t.Fatalf("resume reset correction allowance: calls=%d", len(b.tasks))
				}
				resumed, err := st.LoadRun(context.Background(), "schema-loop")
				if err != nil {
					t.Fatal(err)
				}
				if resumed.OutputCorrections["my_agent"].Attempts != tc.calls-1 {
					t.Fatalf("resume ledger=%+v", resumed.OutputCorrections)
				}
			}
		})
	}
}

func TestSchemaReaskRouterUsesRuntimeAllowance(t *testing.T) {
	for _, budget := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			wf := validationWorkflow()
			wf.Nodes["my_agent"] = &ir.RouterNode{BaseNode: ir.BaseNode{ID: "my_agent"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaudeCode}, RouterMode: ir.RouterLLM}
			b := &schemaViolationBackend{}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			st := tmpStore(t)
			exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			err := New(wf, st, exec, WithOutputCorrectionBudget(budget)).Run(context.Background(), "router-budget", nil)
			var schemaErr *model.OutputSchemaError
			if !errors.As(err, &schemaErr) || len(b.tasks) != budget+1 {
				t.Fatalf("calls=%d, budget=%d err=%v", len(b.tasks), budget, err)
			}
			run, err := st.LoadRun(context.Background(), "router-budget")
			if err != nil {
				t.Fatal(err)
			}
			if ep, ok := run.OutputCorrections["my_agent"]; !ok || ep.Attempts != budget {
				t.Fatalf("missing router allowance: %+v", run.OutputCorrections)
			}
		})
	}
}
