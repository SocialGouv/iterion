package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// foreachWorkflow builds:
//
//	entry -> proc
//	proc  -> proc as foreach scan(item in {{outputs.entry.items}})   # self-loop iterates
//	proc  -> done                                                    # exit after last element
//
// `proc` records the element id it received ({{each.scan.item.id}}) and the
// each.scan.last flag, so the test asserts ordered, once-per-element execution.
func foreachWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "foreach_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"proc":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "proc"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			// Both the forward-entry and the back-edge carry the element binding
			// so `proc` sees each.scan.item on EVERY iteration (index 0..N-1). In
			// real bots a node references {{each.scan.item}} in its own
			// prompt/command, resolved against the current index every iteration.
			{From: "entry", To: "proc", With: []*ir.DataMapping{
				{Key: "id", Refs: []*ir.Ref{{Kind: ir.RefEach, Path: []string{"scan", "item", "id"}}}, Raw: "{{each.scan.item.id}}"},
			}},
			{From: "proc", To: "proc", ForeachName: "scan", With: []*ir.DataMapping{
				{Key: "id", Refs: []*ir.Ref{{Kind: ir.RefEach, Path: []string{"scan", "item", "id"}}}, Raw: "{{each.scan.item.id}}"},
			}},
			{From: "proc", To: "done"},
		},
		Foreaches: map[string]*ir.Foreach{
			"scan": {
				Name:           "scan",
				Item:           "item",
				CollectionRaw:  "{{outputs.entry.items}}",
				CollectionRefs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}}},
			},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// TestForeachSequential verifies the body runs once per element, in order, with
// the element bound under each.<name>.
func TestForeachSequential(t *testing.T) {
	wf := foreachWorkflow()

	var mu sync.Mutex
	var got []string

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{
			map[string]any{"id": "a"},
			map[string]any{"id": "b"},
			map[string]any{"id": "c"},
		}}, nil
	})
	exec.on("proc", func(input map[string]any) (map[string]any, error) {
		id, _ := input["id"].(string)
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-foreach", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-foreach")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("expected finished, got %s", r.Status)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 iterations, got %d (%v)", len(got), got)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i] != want {
			t.Fatalf("iteration %d = %q, want %q (order=%v)", i, got[i], want, got)
		}
	}
}
