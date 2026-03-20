package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/store"
)

// ---------------------------------------------------------------------------
// stubExecutor — configurable per-node executor for tests
// ---------------------------------------------------------------------------

type stubExecutor struct {
	handlers map[string]func(map[string]interface{}) (map[string]interface{}, error)
}

func newStubExecutor() *stubExecutor {
	return &stubExecutor{handlers: make(map[string]func(map[string]interface{}) (map[string]interface{}, error))}
}

func (s *stubExecutor) on(nodeID string, fn func(map[string]interface{}) (map[string]interface{}, error)) {
	s.handlers[nodeID] = fn
}

func (s *stubExecutor) Execute(_ context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	if fn, ok := s.handlers[node.ID]; ok {
		return fn(input)
	}
	// Default: return empty output.
	return map[string]interface{}{}, nil
}

// ---------------------------------------------------------------------------
// tmpStore helper
// ---------------------------------------------------------------------------

func tmpStore(t *testing.T) *store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Test: linear path  agent -> tool -> judge -> done
// ---------------------------------------------------------------------------

func TestLinearPath(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "linear_test",
		Entry: "analyze",
		Nodes: map[string]*ir.Node{
			"analyze": {ID: "analyze", Kind: ir.NodeAgent, Publish: "analysis"},
			"run_cmd": {ID: "run_cmd", Kind: ir.NodeTool, Command: "echo ok"},
			"verify":  {ID: "verify", Kind: ir.NodeJudge},
			"done":    {ID: "done", Kind: ir.NodeDone},
			"fail":    {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "analyze", To: "run_cmd"},
			{From: "run_cmd", To: "verify", With: []*ir.DataMapping{
				{Key: "result", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"run_cmd"}}}, Raw: "{{outputs.run_cmd}}"},
			}},
			{From: "verify", To: "done", Condition: "pass", Negated: false},
			{From: "verify", To: "fail", Condition: "pass", Negated: true},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "all good"}, nil
	})
	exec.on("run_cmd", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"exit_code": 0, "output": "ok"}, nil
	})
	exec.on("verify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"pass": true, "reason": "CI green"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-001", map[string]interface{}{"branch": "main"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run status.
	r, err := s.LoadRun("run-001")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Verify events.
	events, err := s.LoadEvents("run-001")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	expectedTypes := []store.EventType{
		store.EventRunStarted,
		store.EventNodeStarted,  // analyze
		store.EventArtifactWritten,
		store.EventNodeFinished, // analyze
		store.EventEdgeSelected, // analyze -> run_cmd
		store.EventNodeStarted,  // run_cmd
		store.EventNodeFinished, // run_cmd
		store.EventEdgeSelected, // run_cmd -> verify
		store.EventNodeStarted,  // verify
		store.EventNodeFinished, // verify
		store.EventEdgeSelected, // verify -> done
		store.EventNodeStarted,  // done
		store.EventNodeFinished, // done
		store.EventRunFinished,
	}

	if len(events) != len(expectedTypes) {
		t.Fatalf("expected %d events, got %d", len(expectedTypes), len(events))
	}
	for i, et := range expectedTypes {
		if events[i].Type != et {
			t.Errorf("event[%d]: expected %s, got %s", i, et, events[i].Type)
		}
	}

	// Verify artifact was persisted for "analyze" (has publish).
	art, err := s.LoadArtifact("run-001", "analyze", 0)
	if err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if art.Data["summary"] != "all good" {
		t.Errorf("artifact data mismatch: %v", art.Data)
	}
}

// ---------------------------------------------------------------------------
// Test: bounded loop  agent -> judge -> done (when pass) or loop back
// ---------------------------------------------------------------------------

func TestBoundedLoop(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "loop_test",
		Entry: "fix",
		Nodes: map[string]*ir.Node{
			"fix":    {ID: "fix", Kind: ir.NodeAgent},
			"verify": {ID: "verify", Kind: ir.NodeJudge, Publish: "verdict"},
			"done":   {ID: "done", Kind: ir.NodeDone},
			"fail":   {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "fix", To: "verify"},
			{From: "verify", To: "done", Condition: "pass"},
			{From: "verify", To: "fix", Condition: "pass", Negated: true, LoopName: "retry"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 3},
		},
	}

	callCount := 0
	exec := newStubExecutor()
	exec.on("fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		callCount++
		return map[string]interface{}{"patch": fmt.Sprintf("attempt-%d", callCount)}, nil
	})
	exec.on("verify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Fail twice, succeed on the third attempt.
		pass := callCount >= 3
		return map[string]interface{}{"pass": pass, "reason": "check"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-loop", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have called fix 3 times.
	if callCount != 3 {
		t.Errorf("expected 3 fix calls, got %d", callCount)
	}

	// Verify run finished successfully.
	r, err := s.LoadRun("run-loop")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Check that edge_selected events include loop iteration info.
	events, err := s.LoadEvents("run-loop")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	loopEdges := 0
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data["loop"] != nil {
			loopEdges++
		}
	}
	// Two loop-back edges (iterations 1 and 2), third time goes to done.
	if loopEdges != 2 {
		t.Errorf("expected 2 loop edge events, got %d", loopEdges)
	}

	// Verify artifact versions: verify ran 3 times with publish.
	art, err := s.LoadLatestArtifact("run-loop", "verify")
	if err != nil {
		t.Fatalf("load latest artifact: %v", err)
	}
	if art.Version != 2 {
		t.Errorf("expected latest artifact version 2, got %d", art.Version)
	}
}

// ---------------------------------------------------------------------------
// Test: loop exhaustion leads to failure
// ---------------------------------------------------------------------------

func TestLoopExhaustion(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "exhaust_test",
		Entry: "fix",
		Nodes: map[string]*ir.Node{
			"fix":    {ID: "fix", Kind: ir.NodeAgent},
			"verify": {ID: "verify", Kind: ir.NodeJudge},
			"done":   {ID: "done", Kind: ir.NodeDone},
			"fail":   {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "fix", To: "verify"},
			{From: "verify", To: "done", Condition: "pass"},
			{From: "verify", To: "fix", Condition: "pass", Negated: true, LoopName: "retry"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 2},
		},
	}

	exec := newStubExecutor()
	exec.on("fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("verify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"pass": false}, nil // always fail
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-exhaust", nil)
	if err == nil {
		t.Fatal("expected error from loop exhaustion")
	}

	r, err := s.LoadRun("run-exhaust")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected status failed, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: fail node terminates with error
// ---------------------------------------------------------------------------

func TestFailNode(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "fail_test",
		Entry: "check",
		Nodes: map[string]*ir.Node{
			"check": {ID: "check", Kind: ir.NodeJudge},
			"done":  {ID: "done", Kind: ir.NodeDone},
			"fail":  {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "check", To: "done", Condition: "ok"},
			{From: "check", To: "fail", Condition: "ok", Negated: true},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": false}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-fail", nil)
	if err == nil {
		t.Fatal("expected error from fail node")
	}

	r, err := s.LoadRun("run-fail")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected status failed, got %s", r.Status)
	}

	// Verify run_failed event emitted.
	events, err := s.LoadEvents("run-fail")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	lastEvent := events[len(events)-1]
	if lastEvent.Type != store.EventRunFailed {
		t.Errorf("expected last event run_failed, got %s", lastEvent.Type)
	}
}

// ---------------------------------------------------------------------------
// Test: context cancellation
// ---------------------------------------------------------------------------

func TestContextCancellation(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cancel_test",
		Entry: "slow",
		Nodes: map[string]*ir.Node{
			"slow": {ID: "slow", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "slow", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(ctx, "run-cancel", nil)
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}

	r, err := s.LoadRun("run-cancel")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected status failed, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: data mapping with vars and outputs
// ---------------------------------------------------------------------------

func TestDataMappingWithVars(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "mapping_test",
		Entry: "step1",
		Nodes: map[string]*ir.Node{
			"step1": {ID: "step1", Kind: ir.NodeAgent},
			"step2": {ID: "step2", Kind: ir.NodeAgent},
			"done":  {ID: "done", Kind: ir.NodeDone},
			"fail":  {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "step1", To: "step2", With: []*ir.DataMapping{
				{Key: "analysis", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"step1", "summary"}}}, Raw: "{{outputs.step1.summary}}"},
				{Key: "context", Refs: []*ir.Ref{{Kind: ir.RefVars, Path: []string{"repo"}}}, Raw: "{{vars.repo}}"},
			}},
			{From: "step2", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			"repo": {Name: "repo", Type: ir.VarString, HasDefault: true, Default: "my-repo"},
		},
		Loops: map[string]*ir.Loop{},
	}

	var capturedInput map[string]interface{}
	exec := newStubExecutor()
	exec.on("step1", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "looks good"}, nil
	})
	exec.on("step2", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedInput = input
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-map", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedInput["analysis"] != "looks good" {
		t.Errorf("expected analysis='looks good', got %v", capturedInput["analysis"])
	}
	if capturedInput["context"] != "my-repo" {
		t.Errorf("expected context='my-repo', got %v", capturedInput["context"])
	}
}
