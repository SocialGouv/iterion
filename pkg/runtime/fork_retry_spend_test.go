package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// cliSessionStub mimics the documented claude CLI >= 2.1.277 cost semantics
// (code.claude.com/docs/en/agent-sdk/cost-tracking, "Accumulate costs across
// multiple calls"): a normal exit saves the session's totals; a later call
// that RESUMES or FORKS the session restores them, so its total_cost_usd
// already includes the parent's earlier spend. iterion pins 2.1.280.
type cliSessionStub struct {
	mu    sync.Mutex
	saved map[string]float64
	next  int
	spend []float64
	fail  []bool
	tasks []delegate.Task
}

func (b *cliSessionStub) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := len(b.tasks)
	b.tasks = append(b.tasks, task)
	restored, id := 0.0, ""
	switch {
	case task.SessionID == "":
		b.next++
		id = fmt.Sprintf("s%d", b.next)
	case task.ForkSession:
		restored = b.saved[task.SessionID]
		b.next++
		id = fmt.Sprintf("s%d", b.next)
	default:
		restored = b.saved[task.SessionID]
		id = task.SessionID
	}
	total := restored + b.spend[i]
	b.saved[id] = total
	res := delegate.Result{CostUSD: total, CostIsSessionTotal: true, SessionID: id, Tokens: 1000, BackendName: "cli_stub"}
	if b.fail[i] {
		return res, errors.New("error_during_execution")
	}
	res.Output = map[string]any{"ok": true, "result": "x"}
	return res, nil
}

func probeForkWorkflow(mode ir.SessionMode) *ir.Workflow {
	llm := ir.LLMFields{Backend: "cli_stub", Model: "anthropic/claude-opus-5"}
	return &ir.Workflow{
		Name:  "fork_probe",
		Entry: "u",
		Nodes: map[string]ir.Node{
			"u":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "u"}, LLMFields: llm},
			"n":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, LLMFields: llm, Session: mode},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "u", To: "n", With: []*ir.DataMapping{{
				Key:  "_session_id",
				Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"u", "_session_id"}, Raw: "{{outputs.u._session_id}}"}},
				Raw:  "{{outputs.u._session_id}}",
			}}},
			{From: "n", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 12},
	}
}

func TestProbeG2ForkRetryBooksParentSpendPerAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode ir.SessionMode
	}{{"fork", ir.SessionFork}, {"inherit", ir.SessionInherit}, {"fresh", ir.SessionFresh}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			// u spends 5.00; n's first attempt spends 0.30 and fails, its
			// recovery retry spends 0.40 and succeeds. Real spend = 5.70.
			stub := &cliSessionStub{saved: map[string]float64{}, spend: []float64{5.0, 0.3, 0.4}, fail: []bool{false, true, false}}
			reg := delegate.NewRegistry()
			reg.Register("cli_stub", stub)
			wf := probeForkWorkflow(tc.mode)
			exec := model.NewClawExecutor(model.NewRegistry(), wf,
				model.WithBackendRegistry(reg),
				model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
				if prior(ErrCodeExecutionFailed) > 0 {
					return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeExecutionFailed
				}
				return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeExecutionFailed
			})
			st := &checkpointCapturingStore{RunStore: tmpStore(t)}
			err := New(wf, st, exec, WithRecoveryDispatch(dispatch)).Run(context.Background(), "fork-probe-"+tc.name, nil)
			cp := st.lastCheckpoint()
			booked := -1.0
			if cp != nil {
				booked = cp.BudgetCostUSD
			}
			var sids []string
			for _, tk := range stub.tasks {
				sids = append(sids, fmt.Sprintf("{sid=%q fork=%v}", tk.SessionID, tk.ForkSession))
			}
			fmt.Printf("PROBE mode=%-8s err=%v calls=%d tasks=%v booked_cost=%.2f real_spend=5.70 overcount=%.2f\n",
				tc.name, err, len(stub.tasks), sids, booked, booked-5.70)
			want := 5.70
			if tc.mode == ir.SessionInherit {
				want = 10.70
			}
			if err != nil || math.Abs(booked-want) > 1e-9 {
				t.Errorf("mode %s booked %.2f USD, want %.2f", tc.name, booked, want)
			}
		})
	}
}
