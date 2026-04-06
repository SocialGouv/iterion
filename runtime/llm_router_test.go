package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/store"
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
		Nodes: map[string]*ir.Node{
			"entry":      {ID: "entry", Kind: ir.NodeAgent},
			"llm_router": {ID: "llm_router", Kind: ir.NodeRouter, RouterMode: ir.RouterLLM, Model: "test-model"},
			"agent_a":    {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b":    {ID: "agent_b", Kind: ir.NodeAgent},
			"done":       {ID: "done", Kind: ir.NodeDone},
			"fail":       {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"task": "complex"}, nil
	})
	// LLM router returns a structured selection.
	exec.on("llm_router", func(input map[string]interface{}) (map[string]interface{}, error) {
		// Verify candidates were injected.
		candidates, ok := input["_route_candidates"].([]string)
		if !ok {
			t.Error("expected _route_candidates in input")
		}
		if len(candidates) != 2 {
			t.Errorf("expected 2 candidates, got %d", len(candidates))
		}
		return map[string]interface{}{
			"selected_route": "agent_a",
			"reasoning":      "task is complex, needs agent_a",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		agentACalled = true
		return map[string]interface{}{}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		agentBCalled = true
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-single", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run finished.
	r, err := s.LoadRun("run-llm-single")
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"task": "simple"}, nil
	})
	exec.on("llm_router", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"selected_route": "agent_b",
			"reasoning":      "task is simple, agent_b is enough",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		agentACalled = true
		return map[string]interface{}{}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		agentBCalled = true
		return map[string]interface{}{}, nil
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("llm_router", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
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
	r, err := s.LoadRun("run-llm-invalid")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected status failed, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: LLM router multi-mode fans out to selected subset
// ---------------------------------------------------------------------------

func TestLLMRouterMultiMode(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "llm_router_multi",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":      {ID: "entry", Kind: ir.NodeAgent},
			"llm_router": {ID: "llm_router", Kind: ir.NodeRouter, RouterMode: ir.RouterLLM, Model: "test-model", RouterMulti: true},
			"agent_a":    {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b":    {ID: "agent_b", Kind: ir.NodeAgent},
			"agent_c":    {ID: "agent_c", Kind: ir.NodeAgent},
			"final":      {ID: "final", Kind: ir.NodeAgent, AwaitStrategy: ir.AwaitWaitAll},
			"done":       {ID: "done", Kind: ir.NodeDone},
			"fail":       {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"context": "multi-task"}, nil
	})
	exec.on("llm_router", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Select only agent_a and agent_b, not agent_c.
		return map[string]interface{}{
			"selected_routes": []interface{}{"agent_a", "agent_b"},
			"reasoning":       "need both a and b but not c",
		}, nil
	})
	for _, id := range []string{"agent_a", "agent_b", "agent_c"} {
		id := id
		exec.on(id, func(_ map[string]interface{}) (map[string]interface{}, error) {
			mu.Lock()
			calledAgents = append(calledAgents, id)
			mu.Unlock()
			return map[string]interface{}{"result": "from_" + id}, nil
		})
	}
	exec.on("final", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-multi", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run finished.
	r, err := s.LoadRun("run-llm-multi")
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("llm_router", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"selected_route": "agent_a",
			"reasoning":      "test reasoning",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-llm-events", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events, err := s.LoadEvents("run-llm-events")
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
		Nodes: map[string]*ir.Node{
			"entry":      {ID: "entry", Kind: ir.NodeAgent},
			"llm_router": {ID: "llm_router", Kind: ir.NodeRouter, RouterMode: ir.RouterLLM, Model: "test-model", RouterMulti: true},
			"agent_a":    {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b":    {ID: "agent_b", Kind: ir.NodeAgent},
			"final":      {ID: "final", Kind: ir.NodeAgent, AwaitStrategy: ir.AwaitWaitAll},
			"done":       {ID: "done", Kind: ir.NodeDone},
			"fail":       {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("llm_router", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Only select agent_a — agent_b is not executed.
		return map[string]interface{}{
			"selected_routes": []interface{}{"agent_a"},
			"reasoning":       "only need a",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "from_a"}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "from_b"}, nil
	})
	exec.on("final", func(_ map[string]interface{}) (map[string]interface{}, error) {
		finalCalled = true
		return map[string]interface{}{}, nil
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

	r, err := s.LoadRun("run-llm-partial")
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
		Nodes: map[string]*ir.Node{
			"entry":      {ID: "entry", Kind: ir.NodeAgent},
			"llm_router": {ID: "llm_router", Kind: ir.NodeRouter, RouterMode: ir.RouterLLM, Model: ""},
			"agent_a":    {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b":    {ID: "agent_b", Kind: ir.NodeAgent},
			"done":       {ID: "done", Kind: ir.NodeDone},
			"fail":       {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"task": "review"}, nil
	})
	exec.on("llm_router", func(input map[string]interface{}) (map[string]interface{}, error) {
		// Verify the engine still treats this as an LLM router
		// (injects candidates) even with Model == "".
		if _, ok := input["_route_candidates"].([]string); !ok {
			t.Error("expected _route_candidates in input for model-less LLM router")
		}
		return map[string]interface{}{
			"selected_route": "agent_a",
			"reasoning":      "choosing agent_a",
		}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		agentACalled = true
		return map[string]interface{}{}, nil
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

	r, err := s.LoadRun("run-llm-no-model")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
}
