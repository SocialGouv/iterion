package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// elseWorkflow: check routes to "big" when its output field is true,
// else to "small".
func elseWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "else_test",
		Entry: "check",
		Nodes: map[string]ir.Node{
			"check": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "check"}},
			"big":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "big"}},
			"small": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "small"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "check", To: "big", Condition: "big"},
			{From: "check", To: "small", IsElse: true},
			{From: "big", To: "done"},
			{From: "small", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func TestElseEdge_Routing(t *testing.T) {
	run := func(t *testing.T, big bool) (visitedBig, visitedSmall bool) {
		t.Helper()
		exec := newStubExecutor()
		exec.on("check", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"big": big}, nil
		})
		exec.on("big", func(_ map[string]any) (map[string]any, error) {
			visitedBig = true
			return map[string]any{"ok": true}, nil
		})
		exec.on("small", func(_ map[string]any) (map[string]any, error) {
			visitedSmall = true
			return map[string]any{"ok": true}, nil
		})
		s := tmpStore(t)
		eng := New(elseWorkflow(), s, exec)
		if err := eng.Run(context.Background(), "run-else", nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		r, err := s.LoadRun(context.Background(), "run-else")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != store.RunStatusFinished {
			t.Fatalf("status = %s, want finished", r.Status)
		}
		return visitedBig, visitedSmall
	}

	t.Run("when matches — else not taken", func(t *testing.T) {
		big, small := run(t, true)
		if !big || small {
			t.Fatalf("visited big=%v small=%v, want big only", big, small)
		}
	})
	t.Run("when misses — else taken", func(t *testing.T) {
		big, small := run(t, false)
		if big || !small {
			t.Fatalf("visited big=%v small=%v, want small only", big, small)
		}
	})
}

// TestElseEdge_PreferredOverStrayUnconditional: the validator forbids
// coexistence, but the runtime tie-break is defensive — a
// programmatically-built IR with both picks the explicit form.
func TestElseEdge_PreferredOverStrayUnconditional(t *testing.T) {
	wf := elseWorkflow()
	wf.Edges = append(wf.Edges, &ir.Edge{From: "check", To: "fail"})
	exec := newStubExecutor()
	visitedSmall := false
	exec.on("check", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"big": false}, nil
	})
	exec.on("small", func(_ map[string]any) (map[string]any, error) {
		visitedSmall = true
		return map[string]any{"ok": true}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-else-tie", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !visitedSmall {
		t.Fatal("else edge must win the fallback tie-break")
	}
}
