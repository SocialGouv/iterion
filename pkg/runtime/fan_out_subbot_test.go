package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// subbotFanOutWorkflow builds:
//
//	entry -> dispatch(fan_out_each over entry.items) -> run_child(subbot) -> collect(wait_all) -> done
//
// isolated toggles SubbotNode.Isolated; maxParallel sets the budget cap (0 =
// uncapped, i.e. len(items)). Shared by the parallel-subbot workspace-safety
// tests below.
func subbotFanOutWorkflow(isolated bool, maxParallel int) *ir.Workflow {
	router := &ir.RouterNode{
		BaseNode:    ir.BaseNode{ID: "dispatch"},
		RouterMode:  ir.RouterFanOutEach,
		Over:        "{{outputs.entry.items}}",
		OverRefs:    []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}, Raw: "{{outputs.entry.items}}"}},
		ItemBinding: "item",
	}
	wf := &ir.Workflow{
		Name:  "fan_out_each_subbot_parallel",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": router,
			"run_child": &ir.SubbotNode{
				BaseNode: ir.BaseNode{ID: "run_child"},
				Source:   "child.bot",
				Isolated: isolated,
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
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	if maxParallel > 0 {
		wf.Budget = &ir.Budget{MaxParallelBranches: maxParallel}
	}
	return wf
}

// TestFanOutEach_SubbotParallel_AllowedWhenIsolated proves an `isolated:` subbot
// template is admitted by the workspace-safety guard at max_parallel_branches>1
// AND actually runs its children concurrently. Concurrency is measured with the
// same deterministic barrier the semaphore tests use (no sleep): the runner
// blocks each child until all N are inside, so a serialized implementation would
// deadlock rather than pass — the peak is exact.
func TestFanOutEach_SubbotParallel_AllowedWhenIsolated(t *testing.T) {
	const n = 3
	wf := subbotFanOutWorkflow(true, n)

	var active, peak int32
	barrier := make(chan struct{})
	var once sync.Once
	var calls int64
	runner := func(_ context.Context, req SubbotRequest) (map[string]any, error) {
		atomic.AddInt64(&calls, 1)
		cur := atomic.AddInt32(&active, 1)
		for { // lock-free max
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		if cur >= int32(n) {
			once.Do(func() { close(barrier) })
		}
		<-barrier // block until all N children are concurrently inside
		atomic.AddInt32(&active, -1)
		return map[string]any{"committed": true}, nil
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec, WithSubbotRunner(runner))
	if err := eng.Run(context.Background(), "run-fe-subbot-parallel", nil); err != nil {
		t.Fatalf("isolated subbot fan_out_each at max_parallel=%d should be allowed, got %v", n, err)
	}
	r, _ := s.LoadRun(context.Background(), "run-fe-subbot-parallel")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("expected finished, got %s", r.Status)
	}
	if got := atomic.LoadInt64(&calls); got != n {
		t.Fatalf("subbot runner invoked %d times, want %d (one per element)", got, n)
	}
	if got := atomic.LoadInt32(&peak); got != int32(n) {
		t.Fatalf("peak subbot concurrency = %d, want %d (isolated template did not run in parallel)", got, n)
	}
}

// TestFanOutEach_SubbotParallel_RejectedWhenNotIsolated is the safe-default
// non-regression: without `isolated:`, a subbot template fanned out with
// max_parallel_branches>1 is still rejected by WORKSPACE_SAFETY before any child
// runs.
func TestFanOutEach_SubbotParallel_RejectedWhenNotIsolated(t *testing.T) {
	wf := subbotFanOutWorkflow(false, 3)

	var calls int64
	runner := func(_ context.Context, _ SubbotRequest) (map[string]any, error) {
		atomic.AddInt64(&calls, 1)
		return map[string]any{"committed": true}, nil
	}
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec, WithSubbotRunner(runner))
	err := eng.Run(context.Background(), "run-fe-subbot-reject", nil)
	if err == nil {
		t.Fatal("expected workspace safety error for non-isolated parallel subbot")
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("expected RuntimeError ErrCodeWorkspaceSafety, got %T: %v", err, err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("subbot runner invoked %d time(s), want 0 after safety rejection", got)
	}
}

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
	runner := func(_ context.Context, req SubbotRequest) (map[string]any, error) {
		atomic.AddInt64(&calls, 1)
		mu.Lock()
		if tk, ok := req.Vars["ticket"].(string); ok {
			gotTickets[tk] = true
		}
		mu.Unlock()
		return map[string]any{"committed": true}, nil
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"done": true}, nil
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
