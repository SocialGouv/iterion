package cli

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

type summaryExecutor struct{}

func (summaryExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return map[string]any{"summary": "did it"}, nil
}

// TestReport_countsTheNodesThatProducedSomething: the report counts a node
// execution per finish that carries something beyond the facts the engine
// stamps on every finish — a run of one agent then done executed one node,
// not two.
func TestReport_countsTheNodesThatProducedSomething(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := testStore(t)
	wf := &ir.Workflow{
		Name:  "report_count",
		Entry: "act",
		Nodes: map[string]ir.Node{
			"act":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "act"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "act", To: "done"}},
	}
	ctx := context.Background()
	const runID = "run-report-count"
	eng := runtime.New(wf, s, summaryExecutor{}, runtime.WithLogger(iterlog.Nop()), runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"))
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if n := buildReport(r, evs, s).Metrics.NodeCount; n != 1 {
		t.Fatalf("Node Executions = %d for one agent node, want 1", n)
	}
}
