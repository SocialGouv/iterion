package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// skipWorkflow is a fan_out_all whose agent_a has a bounded-iteration
// edge to a dummy node PLUS an else/fallback to the real join. A
// self-loop on agent_a would make findConvergencePoint treat agent_a as
// the join (two incoming sources), so execBranch would never evaluate
// its outgoing edges. Targeting a distinct dummy keeps agent_a in the
// body: skip must take the fallback and never run dummy.
func skipWorkflow(loop *ir.Edge) *ir.Workflow {
	return &ir.Workflow{
		Name:  "fanout_skip_iter",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"agent_a":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"agent_b":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_b"}},
			"dummy":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dummy"}},
			"finalize": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "finalize"}, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "agent_a"},
			{From: "router", To: "agent_b"},
			loop,
			{From: "agent_a", To: "finalize", IsElse: loop.Condition != ""},
			{From: "dummy", To: "finalize"},
			{From: "agent_b", To: "finalize"},
			{From: "finalize", To: "done"},
		},
		Loops:     map[string]*ir.Loop{},
		Foreaches: map[string]*ir.Foreach{},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
	}
}

func runSkipCase(t *testing.T, runID string, wf *ir.Workflow) (agentA, dummy int32) {
	t.Helper()
	var agentACalls, dummyCalls atomic.Int32
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "x"}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		agentACalls.Add(1)
		return map[string]any{"retry": true, "review": "a"}, nil
	})
	exec.on("dummy", func(_ map[string]any) (map[string]any, error) {
		dummyCalls.Add(1)
		return map[string]any{"review": "dummy"}, nil
	})
	exec.on("agent_b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"review": "b"}, nil
	})
	exec.on("finalize", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "ok"}, nil
	})

	eng := New(wf, tmpStore(t), exec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	return agentACalls.Load(), dummyCalls.Load()
}

// TestExecBranch_SkipsLoopEdge is defence in depth for C244: a hand-built
// IR with a loop inside a fan_out_all branch must take the fallback.
func TestExecBranch_SkipsLoopEdge(t *testing.T) {
	wf := skipWorkflow(&ir.Edge{From: "agent_a", To: "dummy", LoopName: "refine", Condition: "retry"})
	wf.Loops["refine"] = &ir.Loop{Name: "refine", MaxIterations: 5}

	agentA, dummy := runSkipCase(t, "run-skip-loop", wf)
	if agentA != 1 {
		t.Fatalf("agent_a calls = %d, want 1", agentA)
	}
	if dummy != 0 {
		t.Fatalf("dummy calls = %d, want 0 (loop edge must be skipped)", dummy)
	}
}

// TestExecBranch_SkipsForeachEdge covers the sibling hole: a foreach
// back-edge has no LoopName, so the old skip (LoopName != "") treated it
// as an unconditional edge and would take it. IsBoundedIteration() must skip it.
func TestExecBranch_SkipsForeachEdge(t *testing.T) {
	wf := skipWorkflow(&ir.Edge{From: "agent_a", To: "dummy", ForeachName: "scan"})
	wf.Foreaches["scan"] = &ir.Foreach{Name: "scan", Item: "item", CollectionRaw: "{{outputs.entry.items}}"}

	agentA, dummy := runSkipCase(t, "run-skip-foreach", wf)
	if agentA != 1 {
		t.Fatalf("agent_a calls = %d, want 1", agentA)
	}
	if dummy != 0 {
		t.Fatalf("dummy calls = %d, want 0 (foreach edge must be skipped)", dummy)
	}
}
