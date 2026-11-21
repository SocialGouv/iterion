package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

type correctionEngineBackend struct {
	mu            sync.Mutex
	calls         map[string]int
	alwaysInvalid bool
	refusal       error
}

func (b *correctionEngineBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls[task.NodeID]++
	n := b.calls[task.NodeID]
	if n > 1 && b.refusal != nil {
		return delegate.Result{Tokens: 3, CostUSD: 1}, b.refusal
	}
	out := map[string]any{"reply": fmt.Sprintf("answer-%d", n)}
	if n > 1 && !b.alwaysInvalid {
		out["ok"] = true
	}
	return delegate.Result{Output: out, Tokens: 5, CostUSD: 1, SessionID: "session-" + task.NodeID, BackendName: delegate.BackendClaudeCode}, nil
}
func correctionEngineWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "correction-engine", Entry: "worker", Nodes: map[string]ir.Node{
		"worker": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaudeCode, Model: "claude-opus-5", Readonly: true}, SchemaFields: ir.SchemaFields{OutputSchema: "answer"}},
		"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}, Edges: []*ir.Edge{{From: "worker", To: "done"}}, Schemas: map[string]*ir.Schema{"answer": {Name: "answer", Fields: []*ir.SchemaField{{Name: "reply", Type: ir.FieldTypeString}, {Name: "ok", Type: ir.FieldTypeBool}}}}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{}, Budget: &ir.Budget{MaxTokens: 100000}}
}

// This exercises real Engine dispatch with a real acquisition supplied by the
// fixture. It does NOT claim that production Run/Resume installs ownership.
func TestCorrectionCompositionThreeDispatchEntries(t *testing.T) {
	for _, entry := range []string{"trunk", "branch", "reinvoke"} {
		t.Run(entry, func(t *testing.T) {
			s, x := correctionFixture(t)
			wf := correctionEngineWorkflow()
			if entry == "branch" {
				wf.Entry = "fork"
				wf.Nodes["fork"] = &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fork"}, RouterMode: ir.RouterFanOutAll}
				wf.Nodes["worker2"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker2"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaudeCode, Model: "claude-opus-5", Readonly: true}, SchemaFields: ir.SchemaFields{OutputSchema: "answer"}}
				wf.Nodes["done"].(*ir.DoneNode).AwaitMode = ir.AwaitWaitAll
				wf.Edges = []*ir.Edge{{From: "fork", To: "worker"}, {From: "fork", To: "worker2"}, {From: "worker", To: "done"}, {From: "worker2", To: "done"}}
			}
			b := &correctionEngineBackend{calls: map[string]int{}}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			if _, ok := any(ex).(OutputCorrectorWithUsage); !ok {
				t.Fatal("runtime usage is not the shared store protocol")
			}
			e := New(wf, s, ex)
			ctx := x.context(context.Background())
			var err error
			if entry == "reinvoke" {
				rs := e.newRunState("correction", nil)
				rs.ctx = ctx
				err = e.reInvokeBackend(ctx, rs, "worker", wf.Nodes["worker"], &model.ErrNeedsInteraction{NodeID: "worker", Backend: delegate.BackendClaudeCode, SessionID: "session-worker", Questions: map[string]any{"question": "ok?"}}, map[string]any{"answer": "yes"}, 0)
			} else {
				err = e.Run(ctx, "correction", nil)
			}
			if err != nil {
				t.Fatalf("real %s dispatch: %v", entry, err)
			}
			r := readCorrection(t, s)
			want := 1
			if entry == "branch" {
				want = 2
			}
			if len(r.OutputCorrections) != want {
				t.Fatalf("real %s dispatch did not enter engine ledger: %+v", entry, r.OutputCorrections)
			}
			scopes := map[string]bool{}
			for _, ep := range r.OutputCorrections {
				if ep.Status != "succeeded" || ep.Attempts != 1 || ep.Usage.Tokens != 5 || len(ep.Reservations) != 1 || ep.Reservations[0].ExecutionEpoch != r.InitialUsageReservations[ep.Reservations[0].InitialUsageID].ExecutionEpoch {
					t.Fatalf("%s bad ledger %+v", entry, ep)
				}
				scopes[ep.Reservations[0].ScopeKey] = true
			}
			if len(scopes) != want {
				t.Fatalf("branch daily identities collided: %v", scopes)
			}
			for node, calls := range b.calls {
				if calls != 2 {
					t.Fatalf("%s calls=%d", node, calls)
				}
			}
		})
	}
}
func TestCorrectionCompositionZeroAndRejectedFailure(t *testing.T) {
	for _, budget := range []int{0, 1} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			s, x := correctionFixture(t)
			wf := correctionEngineWorkflow()
			b := &correctionEngineBackend{calls: map[string]int{}, alwaysInvalid: true}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			e := New(wf, s, ex, WithOutputCorrectionBudget(budget))
			err := e.Run(x.context(context.Background()), "correction", nil)
			if err == nil {
				t.Fatal("invalid output published")
			}
			r := readCorrection(t, s)
			if b.calls["worker"] != budget+1 {
				t.Fatalf("calls=%v", b.calls)
			}
			if budget == 0 && len(r.OutputCorrections) != 0 {
				t.Fatal("zero created episode")
			}
			var re *RuntimeError
			var schema *model.OutputSchemaError
			if !errors.As(err, &re) || re.Code != ErrCodeSchemaValidation || !errors.As(err, &schema) {
				t.Fatalf("producer schema failure lost its typed terminal wrapper: %v", err)
			}
			if r.Checkpoint == nil || r.Checkpoint.BudgetTokensUsed != 5*(budget+1) {
				t.Fatalf("aggregate bill lost %+v", r.Checkpoint)
			}
		})
	}
}
