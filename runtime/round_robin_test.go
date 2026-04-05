package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/store"
)

// ===========================================================================
// Round-robin router tests
// ===========================================================================

// ---------------------------------------------------------------------------
// Helper: build a round-robin workflow with a loop
//   entry -> judge -> router(round_robin) -> agent_a -> judge (loop)
//                                         -> agent_b -> judge (loop)
//   judge -> done (when ready)
// ---------------------------------------------------------------------------

func roundRobinWorkflow(maxIterations int) *ir.Workflow {
	return &ir.Workflow{
		Name:  "round_robin_test",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":   {ID: "entry", Kind: ir.NodeAgent},
			"judge":   {ID: "judge", Kind: ir.NodeJudge},
			"router":  {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterRoundRobin},
			"agent_a": {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b": {ID: "agent_b", Kind: ir.NodeAgent},
			"done":    {ID: "done", Kind: ir.NodeDone},
			"fail":    {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "judge"},
			// Judge not ready -> round-robin router (loop)
			{From: "judge", To: "router", Condition: "ready", Negated: true, LoopName: "refine"},
			// Judge ready -> done
			{From: "judge", To: "done", Condition: "ready", Negated: false},
			// Round-robin edges (unconditional)
			{From: "router", To: "agent_a"},
			{From: "router", To: "agent_b"},
			// Both agents loop back to judge
			{From: "agent_a", To: "judge"},
			{From: "agent_b", To: "judge"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"refine": {Name: "refine", MaxIterations: maxIterations},
		},
	}
}

// ---------------------------------------------------------------------------
// Test: round-robin alternation over 4 iterations
// ---------------------------------------------------------------------------

func TestRoundRobinAlternation(t *testing.T) {
	wf := roundRobinWorkflow(4)

	var mu sync.Mutex
	var callOrder []string
	iteration := 0

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ready": false}, nil
	})
	exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		iter := iteration
		iteration++
		mu.Unlock()
		// Not ready for 4 iterations, then ready.
		ready := iter >= 4
		return map[string]interface{}{"ready": ready}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		callOrder = append(callOrder, "agent_a")
		mu.Unlock()
		return map[string]interface{}{}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		callOrder = append(callOrder, "agent_b")
		mu.Unlock()
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-rr-alt", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run finished.
	r, err := s.LoadRun("run-rr-alt")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Verify alternation: agent_a, agent_b, agent_a, agent_b.
	expected := []string{"agent_a", "agent_b", "agent_a", "agent_b"}
	if len(callOrder) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(callOrder), callOrder)
	}
	for i, want := range expected {
		if callOrder[i] != want {
			t.Errorf("call[%d] = %s, want %s (full order: %v)", i, callOrder[i], want, callOrder)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: round-robin with 3 targets
// ---------------------------------------------------------------------------

func TestRoundRobinThreeTargets(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "round_robin_three",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":   {ID: "entry", Kind: ir.NodeAgent},
			"judge":   {ID: "judge", Kind: ir.NodeJudge},
			"router":  {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterRoundRobin},
			"agent_a": {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b": {ID: "agent_b", Kind: ir.NodeAgent},
			"agent_c": {ID: "agent_c", Kind: ir.NodeAgent},
			"done":    {ID: "done", Kind: ir.NodeDone},
			"fail":    {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "judge"},
			{From: "judge", To: "router", Condition: "ready", Negated: true, LoopName: "refine"},
			{From: "judge", To: "done", Condition: "ready", Negated: false},
			{From: "router", To: "agent_a"},
			{From: "router", To: "agent_b"},
			{From: "router", To: "agent_c"},
			{From: "agent_a", To: "judge"},
			{From: "agent_b", To: "judge"},
			{From: "agent_c", To: "judge"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"refine": {Name: "refine", MaxIterations: 6},
		},
	}

	var mu sync.Mutex
	var callOrder []string
	iteration := 0

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ready": false}, nil
	})
	exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		iter := iteration
		iteration++
		mu.Unlock()
		return map[string]interface{}{"ready": iter >= 3}, nil
	})
	for _, id := range []string{"agent_a", "agent_b", "agent_c"} {
		id := id
		exec.on(id, func(_ map[string]interface{}) (map[string]interface{}, error) {
			mu.Lock()
			callOrder = append(callOrder, id)
			mu.Unlock()
			return map[string]interface{}{}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-rr-three", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify rotation: a, b, c.
	expected := []string{"agent_a", "agent_b", "agent_c"}
	if len(callOrder) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(callOrder), callOrder)
	}
	for i, want := range expected {
		if callOrder[i] != want {
			t.Errorf("call[%d] = %s, want %s", i, callOrder[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: round-robin counter persisted in checkpoint on human pause
// ---------------------------------------------------------------------------

func TestRoundRobinCounterPersistence(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "round_robin_persist",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":   {ID: "entry", Kind: ir.NodeAgent},
			"router":  {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterRoundRobin},
			"agent_a": {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b": {ID: "agent_b", Kind: ir.NodeAgent},
			"human": {
				ID:        "human",
				Kind:      ir.NodeHuman,
				Interaction: ir.InteractionHuman,
			},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "agent_a"},
			{From: "router", To: "agent_b"},
			{From: "agent_a", To: "human"},
			{From: "agent_b", To: "human"},
			{From: "human", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "from_a"}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "from_b"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	// Run — should pause at human node after selecting agent_a (counter=0).
	err := eng.Run(context.Background(), "run-rr-persist", nil)
	if err == nil {
		t.Fatal("expected ErrRunPaused")
	}

	r, err := s.LoadRun("run-rr-persist")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("expected paused status, got %s", r.Status)
	}

	// Verify round-robin counter was persisted.
	if r.Checkpoint == nil {
		t.Fatal("checkpoint is nil")
	}
	counter, ok := r.Checkpoint.RoundRobinCounters["router"]
	if !ok {
		t.Fatal("round_robin counter for 'router' not found in checkpoint")
	}
	if counter != 1 {
		t.Errorf("expected round_robin counter = 1, got %d", counter)
	}
}

// ---------------------------------------------------------------------------
// Test: round-robin events contain selection metadata
// ---------------------------------------------------------------------------

func TestRoundRobinEvents(t *testing.T) {
	wf := roundRobinWorkflow(2)

	iteration := 0
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ready": false}, nil
	})
	exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
		iteration++
		return map[string]interface{}{"ready": iteration >= 3}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-rr-events", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events, err := s.LoadEvents("run-rr-events")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	// Find node_started events for the router.
	var routerEvents []*store.Event
	for _, evt := range events {
		if evt.Type == store.EventNodeStarted && evt.NodeID == "router" {
			routerEvents = append(routerEvents, evt)
		}
	}

	if len(routerEvents) != 2 {
		t.Fatalf("expected 2 router node_started events, got %d", len(routerEvents))
	}

	// First traversal: index 0, selected agent_a.
	if mode, _ := routerEvents[0].Data["mode"].(string); mode != "round_robin" {
		t.Errorf("event[0] mode = %v, want round_robin", mode)
	}
	if idx, ok := routerEvents[0].Data["round_robin_index"]; !ok {
		t.Error("event[0] missing round_robin_index")
	} else if intIdx, ok := idx.(int); !ok || intIdx != 0 {
		// JSON round-trip may produce float64; handle both.
		if floatIdx, ok := idx.(float64); !ok || int(floatIdx) != 0 {
			t.Errorf("event[0] round_robin_index = %v, want 0", idx)
		}
	}

	// Second traversal: index 1, selected agent_b.
	if idx, ok := routerEvents[1].Data["round_robin_index"]; !ok {
		t.Error("event[1] missing round_robin_index")
	} else if intIdx, ok := idx.(int); !ok || intIdx != 1 {
		if floatIdx, ok := idx.(float64); !ok || int(floatIdx) != 1 {
			t.Errorf("event[1] round_robin_index = %v, want 1", idx)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: round-robin edges with "with {}" data mappings are resolved correctly
// ---------------------------------------------------------------------------

func TestRoundRobinWithDataMappings(t *testing.T) {
	// Workflow: entry -> router(round_robin) -> agent_a / agent_b -> done
	// Each edge from router carries a "with {}" mapping that sets a "role" field.
	wf := &ir.Workflow{
		Name:  "round_robin_with",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":   {ID: "entry", Kind: ir.NodeAgent},
			"judge":   {ID: "judge", Kind: ir.NodeJudge},
			"router":  {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterRoundRobin},
			"agent_a": {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b": {ID: "agent_b", Kind: ir.NodeAgent},
			"done":    {ID: "done", Kind: ir.NodeDone},
			"fail":    {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "judge"},
			{From: "judge", To: "router", Condition: "ready", Negated: true, LoopName: "refine"},
			{From: "judge", To: "done", Condition: "ready", Negated: false},
			// Round-robin edges with data mappings.
			{From: "router", To: "agent_a", With: []*ir.DataMapping{
				{Key: "role", Refs: []*ir.Ref{{Kind: ir.RefVars, Path: []string{"role_a"}, Raw: "{{vars.role_a}}"}}, Raw: "{{vars.role_a}}"},
				{Key: "plan", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "plan"}, Raw: "{{outputs.entry.plan}}"}}, Raw: "{{outputs.entry.plan}}"},
			}},
			{From: "router", To: "agent_b", With: []*ir.DataMapping{
				{Key: "role", Refs: []*ir.Ref{{Kind: ir.RefVars, Path: []string{"role_b"}, Raw: "{{vars.role_b}}"}}, Raw: "{{vars.role_b}}"},
				{Key: "plan", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "plan"}, Raw: "{{outputs.entry.plan}}"}}, Raw: "{{outputs.entry.plan}}"},
			}},
			{From: "agent_a", To: "judge"},
			{From: "agent_b", To: "judge"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			"role_a": {Name: "role_a", Type: ir.VarString, HasDefault: true, Default: "claude"},
			"role_b": {Name: "role_b", Type: ir.VarString, HasDefault: true, Default: "codex"},
		},
		Loops: map[string]*ir.Loop{
			"refine": {Name: "refine", MaxIterations: 4},
		},
	}

	var mu sync.Mutex
	var receivedInputs []map[string]interface{}
	iteration := 0

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ready": false, "plan": "the-plan"}, nil
	})
	exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		iter := iteration
		iteration++
		mu.Unlock()
		return map[string]interface{}{"ready": iter >= 2}, nil
	})
	exec.on("agent_a", func(input map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		receivedInputs = append(receivedInputs, copyMap(input))
		mu.Unlock()
		return map[string]interface{}{}, nil
	})
	exec.on("agent_b", func(input map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		receivedInputs = append(receivedInputs, copyMap(input))
		mu.Unlock()
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-rr-with", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 iterations: agent_a gets role=claude, agent_b gets role=codex.
	if len(receivedInputs) != 2 {
		t.Fatalf("expected 2 agent calls, got %d", len(receivedInputs))
	}

	// First call: agent_a should receive role="claude" and plan="the-plan".
	if role, _ := receivedInputs[0]["role"].(string); role != "claude" {
		t.Errorf("agent_a role = %q, want %q", role, "claude")
	}
	if plan, _ := receivedInputs[0]["plan"].(string); plan != "the-plan" {
		t.Errorf("agent_a plan = %q, want %q", plan, "the-plan")
	}

	// Second call: agent_b should receive role="codex" and plan="the-plan".
	if role, _ := receivedInputs[1]["role"].(string); role != "codex" {
		t.Errorf("agent_b role = %q, want %q", role, "codex")
	}
	if plan, _ := receivedInputs[1]["plan"].(string); plan != "the-plan" {
		t.Errorf("agent_b plan = %q, want %q", plan, "the-plan")
	}
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	cp := make(map[string]interface{}, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
