package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestWithParentRunIDPersistsLineage(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "child",
		Entry: "done",
		Nodes: map[string]ir.Node{
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	s := tmpStore(t)
	eng := New(wf, s, newStubExecutor(), WithParentRunID("run-parent"))
	if err := eng.Run(context.Background(), "run-child", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	child, err := s.LoadRun(context.Background(), "run-child")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if child.ParentRunID != "run-parent" {
		t.Fatalf("ParentRunID = %q, want run-parent", child.ParentRunID)
	}
	children, err := s.ListChildRuns(context.Background(), "run-parent")
	if err != nil {
		t.Fatalf("ListChildRuns: %v", err)
	}
	if len(children) != 1 || children[0] != child.ID {
		t.Fatalf("ListChildRuns = %v, want [%s]", children, child.ID)
	}
	if child.Status != store.RunStatusFinished {
		t.Fatalf("status = %q, want %q", child.Status, store.RunStatusFinished)
	}
}
