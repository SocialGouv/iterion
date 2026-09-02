package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ===========================================================================
// LLM router tests
// ===========================================================================

// ---------------------------------------------------------------------------
// Helper: build a single-mode LLM router workflow
//   entry -> llm_router -> agent_a -> done
//                       -> agent_b -> done
// ---------------------------------------------------------------------------

func llmRouterWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "llm_router_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"llm_router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "llm_router"}, LLMFields: ir.LLMFields{Model: "test-model"}, RouterMode: ir.RouterLLM},
			"agent_a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"agent_b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_b"}},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":       &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "llm_router"},
			{From: "llm_router", To: "agent_a"},
			{From: "llm_router", To: "agent_b"},
			{From: "agent_a", To: "done"},
			{From: "agent_b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router selects a single route
// ---------------------------------------------------------------------------

func TestLLMRouterSingleRoute(t *testing.T) {
	wf := llmRouterWorkflow()

	var agentACalled, agentBCalled bool

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"task": "complex"}, nil
	})
	// LLM router returns a structured selection.
	exec.on("llm_router", func(input map[string]any) (map[string]any, error) {
		// Verify candidates were injected.
		candidates, ok := input["_route_candidates"].([]string)
		if !ok {
			t.Error("expected _route_candidates in input")
		}
		if len(candidates) != 2 {
			t.Errorf("expected 2 candidates, got %d", len(candidates))
		}
		return map[string]any{
			"selected_route": "agent_a",
			"reasoning":      "task is complex, needs agent_a",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		agentACalled = true
		return map[string]any{}, nil
	})
	exec.on("agent_b", func(_ map[string]any) (map[string]any, error) {
		agentBCalled = true
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-single", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run finished.
	r, err := s.LoadRun(context.Background(), "run-llm-single")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Only agent_a should have been called.
	if !agentACalled {
		t.Error("expected agent_a to be called")
	}
	if agentBCalled {
		t.Error("expected agent_b NOT to be called")
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router selects the other route
// ---------------------------------------------------------------------------

func TestLLMRouterSelectsOtherRoute(t *testing.T) {
	wf := llmRouterWorkflow()

	var agentACalled, agentBCalled bool

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"task": "simple"}, nil
	})
	exec.on("llm_router", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"selected_route": "agent_b",
			"reasoning":      "task is simple, agent_b is enough",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		agentACalled = true
		return map[string]any{}, nil
	})
	exec.on("agent_b", func(_ map[string]any) (map[string]any, error) {
		agentBCalled = true
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-other", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agentACalled {
		t.Error("expected agent_a NOT to be called")
	}
	if !agentBCalled {
		t.Error("expected agent_b to be called")
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router invalid selection returns error
// ---------------------------------------------------------------------------

func TestLLMRouterInvalidSelection(t *testing.T) {
	wf := llmRouterWorkflow()

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("llm_router", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"selected_route": "nonexistent_agent",
			"reasoning":      "wrong choice",
		}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-invalid", nil)
	if err == nil {
		t.Fatal("expected error for invalid route selection")
	}

	// Verify the run failed.
	r, err := s.LoadRun(context.Background(), "run-llm-invalid")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("expected status failed_resumable, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router multi-mode fans out to selected subset
// ---------------------------------------------------------------------------

func TestLLMRouterMultiMode(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "llm_router_multi",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"llm_router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "llm_router"}, LLMFields: ir.LLMFields{Model: "test-model"}, RouterMode: ir.RouterLLM, RouterMulti: true},
			"agent_a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"agent_b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_b"}},
			"agent_c":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_c"}},
			"final":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "final"}, AwaitMode: ir.AwaitWaitAll},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":       &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "llm_router"},
			{From: "llm_router", To: "agent_a"},
			{From: "llm_router", To: "agent_b"},
			{From: "llm_router", To: "agent_c"},
			{From: "agent_a", To: "final"},
			{From: "agent_b", To: "final"},
			{From: "agent_c", To: "final"},
			{From: "final", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var mu sync.Mutex
	var calledAgents []string

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"context": "multi-task"}, nil
	})
	exec.on("llm_router", func(_ map[string]any) (map[string]any, error) {
		// Select only agent_a and agent_b, not agent_c.
		return map[string]any{
			"selected_routes": []any{"agent_a", "agent_b"},
			"reasoning":       "need both a and b but not c",
		}, nil
	})
	for _, id := range []string{"agent_a", "agent_b", "agent_c"} {
		id := id
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			mu.Lock()
			calledAgents = append(calledAgents, id)
			mu.Unlock()
			return map[string]any{"result": "from_" + id}, nil
		})
	}
	exec.on("final", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-multi", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run finished.
	r, err := s.LoadRun(context.Background(), "run-llm-multi")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Only agent_a and agent_b should have been called, not agent_c.
	mu.Lock()
	defer mu.Unlock()
	if len(calledAgents) != 2 {
		t.Fatalf("expected 2 agent calls, got %d: %v", len(calledAgents), calledAgents)
	}

	hasA, hasB, hasC := false, false, false
	for _, id := range calledAgents {
		switch id {
		case "agent_a":
			hasA = true
		case "agent_b":
			hasB = true
		case "agent_c":
			hasC = true
		}
	}
	if !hasA || !hasB {
		t.Errorf("expected agent_a and agent_b to be called, got %v", calledAgents)
	}
	if hasC {
		t.Error("expected agent_c NOT to be called")
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router events contain routing metadata
// ---------------------------------------------------------------------------

func TestLLMRouterEvents(t *testing.T) {
	wf := llmRouterWorkflow()

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("llm_router", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"selected_route": "agent_a",
			"reasoning":      "test reasoning",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-events", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events, err := s.LoadEvents(context.Background(), "run-llm-events")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	// Find node_started event for the LLM router.
	var routerStarted *store.Event
	for _, evt := range events {
		if evt.Type == store.EventNodeStarted && evt.NodeID == "llm_router" {
			routerStarted = evt
			break
		}
	}
	if routerStarted == nil {
		t.Fatal("missing node_started event for llm_router")
	}
	if mode, _ := routerStarted.Data["mode"].(string); mode != "llm" {
		t.Errorf("node_started mode = %v, want llm", mode)
	}

	// Find node_finished event for the LLM router.
	var routerFinished *store.Event
	for _, evt := range events {
		if evt.Type == store.EventNodeFinished && evt.NodeID == "llm_router" {
			routerFinished = evt
			break
		}
	}
	if routerFinished == nil {
		t.Fatal("missing node_finished event for llm_router")
	}
	if route, _ := routerFinished.Data["selected_route"].(string); route != "agent_a" {
		t.Errorf("node_finished selected_route = %v, want agent_a", route)
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router multi-mode — join Require not satisfied
// ---------------------------------------------------------------------------

func TestLLMRouterMultiModePartialSelection(t *testing.T) {
	// When LLM router selects only agent_a (not agent_b), the convergence
	// point should still succeed — wait_all waits for all started branches,
	// not all possible incoming edges.
	wf := &ir.Workflow{
		Name:  "llm_router_multi_partial",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"llm_router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "llm_router"}, LLMFields: ir.LLMFields{Model: "test-model"}, RouterMode: ir.RouterLLM, RouterMulti: true},
			"agent_a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"agent_b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_b"}},
			"final":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "final"}, AwaitMode: ir.AwaitWaitAll},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":       &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "llm_router"},
			{From: "llm_router", To: "agent_a"},
			{From: "llm_router", To: "agent_b"},
			{From: "agent_a", To: "final"},
			{From: "agent_b", To: "final"},
			{From: "final", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var finalCalled bool
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("llm_router", func(_ map[string]any) (map[string]any, error) {
		// Only select agent_a — agent_b is not executed.
		return map[string]any{
			"selected_routes": []any{"agent_a"},
			"reasoning":       "only need a",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "from_a"}, nil
	})
	exec.on("agent_b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "from_b"}, nil
	})
	exec.on("final", func(_ map[string]any) (map[string]any, error) {
		finalCalled = true
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-partial", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !finalCalled {
		t.Error("expected final node to be called after partial LLM selection")
	}

	r, err := s.LoadRun(context.Background(), "run-llm-partial")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router with no explicit model still dispatches correctly
// Regression test for the bug where the executor dispatch gate checked
// node.Model != "" instead of node.RouterMode == RouterLLM.
// ---------------------------------------------------------------------------

func TestLLMRouterNoExplicitModel(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "llm_router_no_model",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"llm_router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "llm_router"}, RouterMode: ir.RouterLLM},
			"agent_a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"agent_b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_b"}},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":       &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "llm_router"},
			{From: "llm_router", To: "agent_a"},
			{From: "llm_router", To: "agent_b"},
			{From: "agent_a", To: "done"},
			{From: "agent_b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var agentACalled bool

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"task": "review"}, nil
	})
	exec.on("llm_router", func(input map[string]any) (map[string]any, error) {
		// Verify the engine still treats this as an LLM router
		// (injects candidates) even with Model == "".
		if _, ok := input["_route_candidates"].([]string); !ok {
			t.Error("expected _route_candidates in input for model-less LLM router")
		}
		return map[string]any{
			"selected_route": "agent_a",
			"reasoning":      "choosing agent_a",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		agentACalled = true
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-no-model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !agentACalled {
		t.Error("expected agent_a to be called")
	}

	r, err := s.LoadRun(context.Background(), "run-llm-no-model")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: LLM multi-router cancellation abandons a wedged branch (F1)
// ---------------------------------------------------------------------------

// A branch wedged in executor.Execute (ignores ctx) under an LLM
// multi-select router must not hang the run after cancellation: like
// execFanOut, execLLMRouterMulti bounds its post-cancel collector drain by
// branchCancelGracePeriod and abandons the wedged branch. Without the bound
// the collector blocks forever on the wedged branch's result.
//
// Regression test for the LLM-router fan-out missing the cancellation
// hardening that fan_out.go received (ctx-aware semaphore acquire + bounded
// collector drain). Mirrors TestFanOutCancelAbandonsWedgedBranch.
func TestLLMRouterMultiCancelAbandonsWedgedBranch(t *testing.T) {
	oldGrace := branchCancelGracePeriod
	branchCancelGracePeriod = 100 * time.Millisecond
	defer func() { branchCancelGracePeriod = oldGrace }()

	wf := &ir.Workflow{
		Name:  "llm_router_wedged",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"llm_router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "llm_router"}, LLMFields: ir.LLMFields{Model: "test-model"}, RouterMode: ir.RouterLLM, RouterMulti: true},
			"agent_a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"agent_b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_b"}},
			"final":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "final"}, AwaitMode: ir.AwaitWaitAll},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":       &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "llm_router"},
			{From: "llm_router", To: "agent_a"},
			{From: "llm_router", To: "agent_b"},
			{From: "agent_a", To: "final"},
			{From: "agent_b", To: "final"},
			{From: "final", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxParallelBranches: 2}, // both branches run concurrently
	}

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("llm_router", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"selected_routes": []any{"agent_a", "agent_b"},
			"reasoning":       "need both",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		cancel() // trip cancellation while agent_b is mid-flight
		return nil, ctx.Err()
	})
	exec.on("agent_b", func(_ map[string]any) (map[string]any, error) {
		<-release // wedged: ignores ctx, never returns until released
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, "run-llm-wedged", nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a cancellation error")
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("llm router fan_out hung on a wedged branch despite cancellation (collector drain not bounded)")
	}
	close(release) // let the wedged branch goroutine exit cleanly

	// The abandoned branch keeps emitting events (its deferred branch_finished)
	// after Run already returned. Wait for that last write before the test's
	// t.TempDir cleanup runs, otherwise RemoveAll races the late write and
	// fails with "directory not empty".
	waitBranchFinished(t, s, "run-llm-wedged", "branch_llm_router_agent_b")
}

// TestLLMRouterMultiResumeReusesPersistedSelection pins the durability promise
// of the llm-multi fan-out: once an invocation is persisted, a branch-gate
// resume re-enters execLoop AT THE ROUTER and must reuse the selection it
// already paid for rather than asking the model again.
//
// Re-asking is not merely wasteful. The model could return a different subset,
// and ensureParallelInvocation compares the branch set against the persisted
// one — a changed set is a hard "rewind the router before resuming" error, so
// a re-ask can strand a resumable run outright. The reuse path must also not
// emit a second router node_finished: it skips node_started (the whole
// pre-execute block is bypassed), so an unpaired finish would corrupt the
// timeline the studio reducer folds.
func TestLLMRouterMultiResumeReusesPersistedSelection(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "llm_router_multi_resume",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"llm_router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "llm_router"}, LLMFields: ir.LLMFields{Model: "test-model"}, RouterMode: ir.RouterLLM, RouterMulti: true},
			"gate_a":     &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate_a"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"work_a":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work_a"}},
			"work_b":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work_b"}},
			"work_c":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work_c"}},
			"final":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "final"}, AwaitMode: ir.AwaitWaitAll},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "llm_router"},
			{From: "llm_router", To: "gate_a"},
			{From: "llm_router", To: "work_b"},
			{From: "llm_router", To: "work_c"},
			{From: "gate_a", To: "work_a"},
			{From: "work_a", To: "final"},
			{From: "work_b", To: "final"},
			{From: "work_c", To: "final"},
			{From: "final", To: "done"},
		},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
		Loops:     map[string]*ir.Loop{},
		Foreaches: map[string]*ir.Foreach{},
	}

	var mu sync.Mutex
	routerCalls := 0
	calls := map[string]int{}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"context": "multi"}, nil
	})
	exec.on("llm_router", func(_ map[string]any) (map[string]any, error) {
		mu.Lock()
		routerCalls++
		// A second ask returns a DIFFERENT subset, so a re-ask cannot pass
		// silently: it would either change the branch set (a hard resume
		// error) or run the wrong branches.
		reask := routerCalls > 1
		mu.Unlock()
		if reask {
			return map[string]any{"selected_routes": []any{"work_c"}, "reasoning": "re-ask"}, nil
		}
		return map[string]any{"selected_routes": []any{"gate_a", "work_b"}, "reasoning": "first ask"}, nil
	})
	for _, id := range []string{"work_a", "work_b", "work_c", "final"} {
		id := id
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			mu.Lock()
			calls[id]++
			mu.Unlock()
			return map[string]any{"result": id}, nil
		})
	}

	s := tmpStore(t)
	const runID = "run-llm-multi-resume"
	if err := New(wf, s, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want a pause at the branch gate", err)
	}
	if err := New(wf, s, exec).Resume(context.Background(), runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("resume = %v, want completion", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if routerCalls != 1 {
		t.Errorf("llm router executed %d times, want 1 — the resume re-asked the model instead of reusing the persisted selection", routerCalls)
	}
	if calls["work_a"] != 1 || calls["work_b"] != 1 {
		t.Errorf("branch calls = %#v, want work_a and work_b once each", calls)
	}
	if calls["work_c"] != 0 {
		t.Errorf("work_c ran %d times — it was never in the persisted selection", calls["work_c"])
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleRouterLifecyclePair(t, events, "llm_router")
}
