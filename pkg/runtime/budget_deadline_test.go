package runtime

import (
	"context"
	"strings"
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

func (e *ctxBlockingExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]interface{}) (map[string]interface{}, error) {
	if node.NodeID() == e.blockNode {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]interface{}{"ok": true}, nil
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
