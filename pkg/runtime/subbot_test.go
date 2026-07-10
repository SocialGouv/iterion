package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSubbotRunsChildAndMapsOutput verifies a SubbotNode resolves its with:
// mappings into the child vars, invokes the injected SubbotRunner, and maps the
// child's terminal output to outputs.<subbot>.<field> for downstream routing.
func TestSubbotRunsChildAndMapsOutput(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "subbot_test",
		Entry: "plan",
		Nodes: map[string]ir.Node{
			"plan": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "plan"}},
			"run_child": &ir.SubbotNode{
				BaseNode: ir.BaseNode{ID: "run_child"},
				Source:   "child.bot",
				With: []*ir.DataMapping{
					{Key: "ticket", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"plan", "id"}}}, Raw: "{{outputs.plan.id}}"},
				},
			},
			"ok":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "ok"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "plan", To: "run_child"},
			{From: "run_child", To: "ok", Condition: "validated"},
			{From: "run_child", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var gotSource string
	var gotVars map[string]any
	var gotParent, gotNode string
	runner := func(_ context.Context, req SubbotRequest) (map[string]any, error) {
		gotSource = req.Source
		gotVars = req.Vars
		gotParent = req.ParentRunID
		gotNode = req.NodeID
		return map[string]any{"validated": true, "pr": "42"}, nil
	}

	exec := newStubExecutor()
	exec.on("plan", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"id": "T-7"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec, WithSubbotRunner(runner))
	if err := eng.Run(context.Background(), "run-subbot", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-subbot")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("expected finished, got %s", r.Status)
	}
	if gotSource != "child.bot" {
		t.Errorf("runner source = %q, want child.bot", gotSource)
	}
	if gotVars["ticket"] != "T-7" {
		t.Errorf("child var ticket = %v, want T-7", gotVars["ticket"])
	}
	if gotParent != "run-subbot" || gotNode != "run_child" {
		t.Errorf("parent/node linkage = %q/%q", gotParent, gotNode)
	}
	// The child's validated=true output must have routed via the conditional
	// edge to `ok` — surfaced as an edge_selected event to "ok".
	events, _ := s.LoadEvents(context.Background(), "run-subbot")
	routedToOK := false
	for _, ev := range events {
		if ev.Type == store.EventEdgeSelected {
			if to, _ := ev.Data["to"].(string); to == "ok" {
				routedToOK = true
			}
		}
	}
	if !routedToOK {
		t.Error("expected the subbot's validated=true to route to the `ok` done node")
	}
}

// TestSubbotWithoutRunnerErrors asserts a subbot node fails cleanly when no
// SubbotRunner is wired (the bare engine can't compile a child).
func TestSubbotWithoutRunnerErrors(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "subbot_norunner",
		Entry: "run_child",
		Nodes: map[string]ir.Node{
			"run_child": &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "run_child"}, Source: "child.bot"},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":      &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "run_child", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec) // no SubbotRunner
	err := eng.Run(context.Background(), "run-subbot-norunner", nil)
	if err == nil {
		t.Fatal("expected an error when no SubbotRunner is wired")
	}
}
