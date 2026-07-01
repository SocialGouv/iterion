package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestFanOutEach_SubbotPerElement is the regression for #61: a `subbot` node
// inside a `fan_out_each` branch used to fail with `unsupported node kind
// "subbot"` because branch nodes were dispatched to the model executor (which
// has no subbot case), while the main graph dispatched subbot via execSubbot.
//
// It asserts the documented "map a sub-bot over the items" pattern: the child
// runs once per element, and its `with:` mappings resolve against the BRANCH
// scope so {{outputs.dispatch.item.id}} is the element's id.
func TestFanOutEach_SubbotPerElement(t *testing.T) {
	router := &ir.RouterNode{
		BaseNode:    ir.BaseNode{ID: "dispatch"},
		RouterMode:  ir.RouterFanOutEach,
		Over:        "{{outputs.entry.items}}",
		OverRefs:    []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}, Raw: "{{outputs.entry.items}}"}},
		ItemBinding: "item",
	}
	wf := &ir.Workflow{
		Name:  "fan_out_each_subbot",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": router,
			"run_child": &ir.SubbotNode{
				BaseNode: ir.BaseNode{ID: "run_child"},
				Source:   "child.bot",
				With: []*ir.DataMapping{
					{Key: "ticket", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"dispatch", "item", "id"}, Raw: "{{outputs.dispatch.item.id}}"}}, Raw: "{{outputs.dispatch.item.id}}"},
				},
			},
			"collect": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":    &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch"},
			{From: "dispatch", To: "run_child"},
			{From: "run_child", To: "collect"},
			{From: "collect", To: "done"},
		},
		Budget:  &ir.Budget{MaxParallelBranches: 1},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var mu sync.Mutex
	gotTickets := map[string]bool{}
	var calls int64
	runner := func(_ context.Context, req SubbotRequest) (map[string]interface{}, error) {
		atomic.AddInt64(&calls, 1)
		mu.Lock()
		if tk, ok := req.Vars["ticket"].(string); ok {
			gotTickets[tk] = true
		}
		mu.Unlock()
		return map[string]interface{}{"committed": true}, nil
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"items": []interface{}{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("collect", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec, WithSubbotRunner(runner))
	if err := eng.Run(context.Background(), "run-fe-subbot", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-fe-subbot")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("expected finished, got %s", r.Status)
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("subbot runner invoked %d times, want 3 (one per element)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"A", "B", "C"} {
		if !gotTickets[id] {
			t.Errorf("subbot never ran for element %s (branch-scope with: resolution broken)", id)
		}
	}
}
