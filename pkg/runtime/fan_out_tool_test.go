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

// toolFanOutWorkflow builds:
//
//	entry -> dispatch(fan_out_each over entry.items) -> run_scene(tool) -> collect(wait_all) -> done
//
// parallelSafe toggles ToolNode.ParallelSafe; maxParallel sets the budget cap
// (0 = uncapped, i.e. len(items)). Mirror of subbotFanOutWorkflow for the tool
// opt-out.
func toolFanOutWorkflow(parallelSafe bool, maxParallel int) *ir.Workflow {
	router := &ir.RouterNode{
		BaseNode:    ir.BaseNode{ID: "dispatch"},
		RouterMode:  ir.RouterFanOutEach,
		Over:        "{{outputs.entry.items}}",
		OverRefs:    []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}, Raw: "{{outputs.entry.items}}"}},
		ItemBinding: "item",
	}
	wf := &ir.Workflow{
		Name:  "fan_out_each_tool_parallel",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": router,
			"run_scene": &ir.ToolNode{
				BaseNode:     ir.BaseNode{ID: "run_scene"},
				Command:      "write --scene {{outputs.dispatch.item.id}}",
				ParallelSafe: parallelSafe,
			},
			"collect": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":    &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch"},
			{From: "dispatch", To: "run_scene"},
			{From: "run_scene", To: "collect"},
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

// TestFanOutEach_ToolParallel_AllowedWhenParallelSafe proves a `parallel_safe:`
// tool template is admitted by the workspace-safety guard at
// max_parallel_branches>1 AND actually runs concurrently. Concurrency is
// measured with the same deterministic barrier the subbot test uses (no sleep):
// each replay blocks until all N are inside, so a serialized implementation
// would deadlock rather than pass — the peak is exact.
func TestFanOutEach_ToolParallel_AllowedWhenParallelSafe(t *testing.T) {
	const n = 3
	wf := toolFanOutWorkflow(true, n)

	var active, peak int32
	barrier := make(chan struct{})
	var once sync.Once
	var calls int64

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("run_scene", func(_ map[string]any) (map[string]any, error) {
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
		<-barrier // block until all N replays are concurrently inside
		atomic.AddInt32(&active, -1)
		return map[string]any{"committed": true}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fe-tool-parallel", nil); err != nil {
		t.Fatalf("parallel_safe tool fan_out_each at max_parallel=%d should be allowed, got %v", n, err)
	}
	r, _ := s.LoadRun(context.Background(), "run-fe-tool-parallel")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("expected finished, got %s", r.Status)
	}
	if got := atomic.LoadInt64(&calls); got != n {
		t.Fatalf("tool executed %d times, want %d (one per element)", got, n)
	}
	if got := atomic.LoadInt32(&peak); got != int32(n) {
		t.Fatalf("peak tool concurrency = %d, want %d (parallel_safe template did not run in parallel)", got, n)
	}
}

// TestFanOutEach_ToolParallel_RejectedWhenTemplateHasOtherMutatingNode proves the
// exemption is scoped to the parallel_safe node itself: a template that chains a
// parallel_safe tool INTO a second, non-safe tool still contains a mutating node,
// so the guard rejects the parallel fan-out before any replay runs.
func TestFanOutEach_ToolParallel_RejectedWhenTemplateHasOtherMutatingNode(t *testing.T) {
	wf := toolFanOutWorkflow(true, 3)
	// Insert a plain (non-parallel_safe) tool between run_scene and collect.
	wf.Nodes["post_process"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "post_process"}, Command: "aggregate > out/report.json"}
	// Rewire: run_scene -> post_process -> collect.
	for _, e := range wf.Edges {
		if e.From == "run_scene" && e.To == "collect" {
			e.To = "post_process"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "post_process", To: "collect"})

	var sceneCalls, postCalls int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("run_scene", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&sceneCalls, 1)
		return map[string]any{"committed": true}, nil
	})
	exec.on("post_process", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&postCalls, 1)
		return map[string]any{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-fe-tool-mixed", nil)
	if err == nil {
		t.Fatal("expected workspace safety error: template chains a non-parallel_safe tool")
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("expected RuntimeError ErrCodeWorkspaceSafety, got %T: %v", err, err)
	}
	if sceneCalls != 0 || postCalls != 0 {
		t.Fatalf("no branch node should run after safety rejection; scene=%d post=%d", sceneCalls, postCalls)
	}
}

// TestValidateWorkspaceSafety_ParallelSafeToolsStillRejectedInFanOutAll pins the
// scope of the opt-out: `parallel_safe:` relaxes only the fan_out_each guard.
// In a STATIC fan_out_all the branches are distinct nodes with no item-key
// disjointness guarantee, so two parallel_safe tools writing in parallel remain
// a violation.
func TestValidateWorkspaceSafety_ParallelSafeToolsStillRejectedInFanOutAll(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"tool_a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool_a"}, Command: "build A > out/report.json", ParallelSafe: true},
			"tool_b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool_b"}, Command: "build B > out/report.json", ParallelSafe: true},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "tool_a"},
			{From: "router", To: "tool_b"},
			{From: "tool_a", To: "join"},
			{From: "tool_b", To: "join"},
			{From: "join", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	err := e.validateWorkspaceSafety("router", []*ir.Edge{
		{From: "router", To: "tool_a"},
		{From: "router", To: "tool_b"},
	})
	if err == nil {
		t.Fatal("parallel_safe must not relax the static fan_out_all guard")
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("expected RuntimeError ErrCodeWorkspaceSafety, got %T: %v", err, err)
	}
}

// TestFanOutEach_ToolParallel_RejectedWhenNotParallelSafe is the safe-default
// non-regression: without `parallel_safe:`, a tool template fanned out with
// max_parallel_branches>1 is still rejected by WORKSPACE_SAFETY before any
// replay runs.
func TestFanOutEach_ToolParallel_RejectedWhenNotParallelSafe(t *testing.T) {
	wf := toolFanOutWorkflow(false, 3)

	var calls int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("run_scene", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&calls, 1)
		return map[string]any{"committed": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-fe-tool-reject", nil)
	if err == nil {
		t.Fatal("expected workspace safety error for non-parallel_safe parallel tool")
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("expected RuntimeError ErrCodeWorkspaceSafety, got %T: %v", err, err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("tool executed %d time(s), want 0 after safety rejection", got)
	}
}
