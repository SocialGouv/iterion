package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/store"
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
		store.EventNodeStarted, // analyze
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
	if r.Status != store.RunStatusCancelled {
		t.Errorf("expected status cancelled, got %s", r.Status)
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

// ===========================================================================
// P3-03: Human pause/resume tests
// ===========================================================================

// humanWorkflow builds a workflow: agent -> human -> agent -> done
func humanWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "human_pause_test",
		Entry: "analyze",
		Nodes: map[string]*ir.Node{
			"analyze":   {ID: "analyze", Kind: ir.NodeAgent, Publish: "analysis"},
			"review":    {ID: "review", Kind: ir.NodeHuman, Interaction: ir.InteractionHuman, Publish: "human_decisions"},
			"integrate": {ID: "integrate", Kind: ir.NodeAgent},
			"done":      {ID: "done", Kind: ir.NodeDone},
			"fail":      {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "analyze", To: "review", With: []*ir.DataMapping{
				{Key: "summary", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"analyze", "summary"}}}, Raw: "{{outputs.analyze.summary}}"},
			}},
			{From: "review", To: "integrate", With: []*ir.DataMapping{
				{Key: "decisions", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"review"}}}, Raw: "{{outputs.review}}"},
			}},
			{From: "integrate", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// ---------------------------------------------------------------------------
// Test: human node pauses the run
// ---------------------------------------------------------------------------

func TestHumanPause(t *testing.T) {
	wf := humanWorkflow()
	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "needs review"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-human", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused, got: %v", err)
	}

	// Verify run status is paused.
	r, err := s.LoadRun("run-human")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("expected status paused_waiting_human, got %s", r.Status)
	}

	// Verify checkpoint exists.
	if r.Checkpoint == nil {
		t.Fatal("expected checkpoint to be set")
	}
	if r.Checkpoint.NodeID != "review" {
		t.Errorf("checkpoint node = %q, want review", r.Checkpoint.NodeID)
	}

	// Verify interaction was created.
	interaction, err := s.LoadInteraction("run-human", r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("load interaction: %v", err)
	}
	if interaction.NodeID != "review" {
		t.Errorf("interaction node = %q, want review", interaction.NodeID)
	}
	if interaction.AnsweredAt != nil {
		t.Error("interaction should not be answered yet")
	}
	// Questions should contain the mapped input from the edge.
	if interaction.Questions["summary"] != "needs review" {
		t.Errorf("questions[summary] = %v, want 'needs review'", interaction.Questions["summary"])
	}

	// Verify events.
	events, err := s.LoadEvents("run-human")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	expectedTypes := []store.EventType{
		store.EventRunStarted,
		store.EventNodeStarted,         // analyze
		store.EventArtifactWritten,     // analyze publish
		store.EventNodeFinished,        // analyze
		store.EventEdgeSelected,        // analyze -> review
		store.EventNodeStarted,         // review (human)
		store.EventHumanInputRequested, // human questions
		store.EventRunPaused,           // run paused
	}
	if len(events) != len(expectedTypes) {
		t.Fatalf("expected %d events, got %d", len(expectedTypes), len(events))
	}
	for i, et := range expectedTypes {
		if events[i].Type != et {
			t.Errorf("event[%d]: expected %s, got %s", i, et, events[i].Type)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: human pause then resume completes the run
// ---------------------------------------------------------------------------

func TestHumanPauseAndResume(t *testing.T) {
	wf := humanWorkflow()

	var capturedIntegrateInput map[string]interface{}
	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "needs review"}, nil
	})
	exec.on("integrate", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedIntegrateInput = input
		return map[string]interface{}{"result": "integrated"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	// Phase 1: Run until pause.
	err := eng.Run(context.Background(), "run-resume", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused, got: %v", err)
	}

	// Phase 2: Resume with answers.
	answers := map[string]interface{}{
		"approve": true,
		"comment": "Ship it!",
	}
	err = eng.Resume(context.Background(), "run-resume", answers)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	// Verify run finished.
	r, err := s.LoadRun("run-resume")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
	// Checkpoint should be cleared after resume.
	if r.Checkpoint != nil {
		t.Error("checkpoint should be nil after run finishes")
	}

	// Verify human answers were passed to the integrate node.
	if capturedIntegrateInput == nil {
		t.Fatal("integrate node was never called")
	}
	decisions, ok := capturedIntegrateInput["decisions"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected decisions map, got %T: %v", capturedIntegrateInput["decisions"], capturedIntegrateInput["decisions"])
	}
	if decisions["approve"] != true {
		t.Errorf("decisions[approve] = %v, want true", decisions["approve"])
	}
	if decisions["comment"] != "Ship it!" {
		t.Errorf("decisions[comment] = %v, want 'Ship it!'", decisions["comment"])
	}

	// Verify interaction was answered.
	ids, err := s.ListInteractions("run-resume")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(ids))
	}
	interaction, err := s.LoadInteraction("run-resume", ids[0])
	if err != nil {
		t.Fatalf("load interaction: %v", err)
	}
	if interaction.AnsweredAt == nil {
		t.Error("interaction should be answered")
	}
	if interaction.Answers["approve"] != true {
		t.Errorf("interaction answer approve = %v", interaction.Answers["approve"])
	}

	// Verify human artifact was persisted (review has publish: human_decisions).
	art, err := s.LoadArtifact("run-resume", "review", 0)
	if err != nil {
		t.Fatalf("load human artifact: %v", err)
	}
	if art.Data["approve"] != true {
		t.Errorf("human artifact approve = %v", art.Data["approve"])
	}

	// Verify full event sequence.
	events, err := s.LoadEvents("run-resume")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	expectedTypes := []store.EventType{
		// Phase 1: Run until pause
		store.EventRunStarted,
		store.EventNodeStarted,     // analyze
		store.EventArtifactWritten, // analyze publish
		store.EventNodeFinished,    // analyze
		store.EventEdgeSelected,    // analyze -> review
		store.EventNodeStarted,     // review (human)
		store.EventHumanInputRequested,
		store.EventRunPaused,
		// Phase 2: Resume
		store.EventHumanAnswersRecorded,
		store.EventArtifactWritten, // human publish
		store.EventNodeFinished,    // review
		store.EventRunResumed,
		store.EventEdgeSelected, // review -> integrate
		store.EventNodeStarted,  // integrate
		store.EventNodeFinished, // integrate
		store.EventEdgeSelected, // integrate -> done
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
}

// ---------------------------------------------------------------------------
// Test: resume on non-paused run returns error
// ---------------------------------------------------------------------------

func TestResumeNonPausedRun(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "simple",
		Entry: "step",
		Nodes: map[string]*ir.Node{
			"step": {ID: "step", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			{From: "step", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	// Run to completion.
	if err := eng.Run(context.Background(), "run-done", nil); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Try to resume a finished run.
	err := eng.Resume(context.Background(), "run-done", map[string]interface{}{"x": 1})
	if err == nil {
		t.Fatal("expected error when resuming finished run")
	}
}

// ---------------------------------------------------------------------------
// Test: upstream nodes are NOT replayed on resume
// ---------------------------------------------------------------------------

func TestResumeDoesNotReplayUpstream(t *testing.T) {
	wf := humanWorkflow()

	analyzeCallCount := 0
	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		analyzeCallCount++
		return map[string]interface{}{"summary": "done"}, nil
	})
	exec.on("integrate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	// Run until pause.
	err := eng.Run(context.Background(), "run-noreplay", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused, got: %v", err)
	}
	if analyzeCallCount != 1 {
		t.Fatalf("analyze should have been called once, got %d", analyzeCallCount)
	}

	// Resume.
	err = eng.Resume(context.Background(), "run-noreplay", map[string]interface{}{"ok": true})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	// Analyze should NOT have been called again.
	if analyzeCallCount != 1 {
		t.Errorf("analyze was replayed on resume: called %d times", analyzeCallCount)
	}
}

// ---------------------------------------------------------------------------
// Test: human node with upstream outputs preserved in checkpoint
// ---------------------------------------------------------------------------

func TestCheckpointPreservesUpstreamOutputs(t *testing.T) {
	wf := humanWorkflow()

	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "analysis result"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-cp", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused, got: %v", err)
	}

	r, err := s.LoadRun("run-cp")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}

	// Checkpoint should contain analyze's output.
	if r.Checkpoint.Outputs["analyze"] == nil {
		t.Fatal("checkpoint should contain analyze output")
	}
	if r.Checkpoint.Outputs["analyze"]["summary"] != "analysis result" {
		t.Errorf("checkpoint outputs[analyze][summary] = %v", r.Checkpoint.Outputs["analyze"]["summary"])
	}
}

// ---------------------------------------------------------------------------
// Test: human pause in a workflow with loop counters preserves them
// ---------------------------------------------------------------------------

func TestHumanPausePreservesLoopCounters(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "loop_human_test",
		Entry: "fix",
		Nodes: map[string]*ir.Node{
			"fix":    {ID: "fix", Kind: ir.NodeAgent},
			"judge":  {ID: "judge", Kind: ir.NodeJudge},
			"review": {ID: "review", Kind: ir.NodeHuman, Interaction: ir.InteractionHuman},
			"done":   {ID: "done", Kind: ir.NodeDone},
			"fail":   {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "fix", To: "judge"},
			{From: "judge", To: "review", Condition: "needs_human"},
			{From: "judge", To: "fix", Condition: "needs_human", Negated: true, LoopName: "retry"},
			{From: "review", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 5},
		},
	}

	fixCount := 0
	exec := newStubExecutor()
	exec.on("fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		fixCount++
		return map[string]interface{}{"attempt": fixCount}, nil
	})
	exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// First two: loop back; third: needs human.
		needsHuman := fixCount >= 3
		return map[string]interface{}{"needs_human": needsHuman}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-loop-human", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused, got: %v", err)
	}

	// Fix should have been called 3 times (2 loops + 1 final).
	if fixCount != 3 {
		t.Errorf("expected 3 fix calls, got %d", fixCount)
	}

	// Checkpoint should preserve loop counters.
	r, _ := s.LoadRun("run-loop-human")
	if r.Checkpoint.LoopCounters["retry"] != 2 {
		t.Errorf("expected loop counter retry=2, got %d", r.Checkpoint.LoopCounters["retry"])
	}

	// Resume and finish.
	err = eng.Resume(context.Background(), "run-loop-human", map[string]interface{}{"approved": true})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	r, _ = s.LoadRun("run-loop-human")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: resume with non-existent run returns error
// ---------------------------------------------------------------------------

func TestResumeNonExistentRun(t *testing.T) {
	wf := humanWorkflow()
	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Resume(context.Background(), "no-such-run", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error when resuming non-existent run")
	}
}

// ===========================================================================
// Human auto_answer mode tests
// ===========================================================================

// humanModeWorkflow builds: agent -> human(mode) -> agent -> done
func humanModeWorkflow(mode ir.InteractionMode) *ir.Workflow {
	return &ir.Workflow{
		Name:  "human_mode_test",
		Entry: "analyze",
		Nodes: map[string]*ir.Node{
			"analyze":   {ID: "analyze", Kind: ir.NodeAgent},
			"review":    {ID: "review", Kind: ir.NodeHuman, Interaction: mode, Model: "test-model", OutputSchema: "review_output"},
			"integrate": {ID: "integrate", Kind: ir.NodeAgent},
			"done":      {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			{From: "analyze", To: "review"},
			{From: "review", To: "integrate"},
			{From: "integrate", To: "done"},
		},
		Schemas: map[string]*ir.Schema{
			"review_output": {Name: "review_output", Fields: []*ir.SchemaField{
				{Name: "approved", Type: ir.FieldTypeBool},
				{Name: "reason", Type: ir.FieldTypeString},
			}},
		},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func TestHumanAutoAnswer(t *testing.T) {
	wf := humanModeWorkflow(ir.InteractionLLM)
	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "all good"}, nil
	})
	exec.on("review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"approved": true, "reason": "auto-approved"}, nil
	})
	exec.on("integrate", func(input map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-auto", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Run should complete without pausing.
	r, err := s.LoadRun("run-auto")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
}

func TestHumanAutoAnswerPublishArtifact(t *testing.T) {
	wf := humanModeWorkflow(ir.InteractionLLM)
	wf.Nodes["review"].Publish = "review_artifact"

	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "all good"}, nil
	})
	exec.on("review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"approved": true, "reason": "auto-approved"}, nil
	})
	exec.on("integrate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-auto-pub", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify artifact was persisted.
	art, err := s.LoadArtifact("run-auto-pub", "review", 0)
	if err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if art.Data["approved"] != true {
		t.Errorf("expected approved=true in artifact, got %v", art.Data["approved"])
	}
}

// ===========================================================================
// Human auto_or_pause mode tests
// ===========================================================================

func TestHumanAutoOrPause_Proceeds(t *testing.T) {
	wf := humanModeWorkflow(ir.InteractionLLMOrHuman)
	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "straightforward"}, nil
	})
	exec.on("review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"needs_human_input": false,
			"approved":          true,
			"reason":            "auto-decided",
		}, nil
	})
	exec.on("integrate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-aop-proceed", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	r, err := s.LoadRun("run-aop-proceed")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Verify needs_human_input was stripped from output.
	events, err := s.LoadEvents("run-aop-proceed")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	// Should not find any pause events.
	for _, ev := range events {
		if ev.Type == store.EventRunPaused {
			t.Error("unexpected run_paused event")
		}
	}
}

func TestHumanAutoOrPause_Pauses(t *testing.T) {
	wf := humanModeWorkflow(ir.InteractionLLMOrHuman)
	exec := newStubExecutor()
	exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "complex change"}, nil
	})
	exec.on("review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"needs_human_input": true,
			"approved":          false,
			"reason":            "too complex for auto",
		}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-aop-pause", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused, got: %v", err)
	}

	// Verify run is paused.
	r, err := s.LoadRun("run-aop-pause")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("expected status paused_waiting_human, got %s", r.Status)
	}

	// Now resume with human answers.
	exec.on("integrate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"done": true}, nil
	})

	answers := map[string]interface{}{"approved": true, "reason": "human approved"}
	err = eng.Resume(context.Background(), "run-aop-pause", answers)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	r, err = s.LoadRun("run-aop-pause")
	if err != nil {
		t.Fatalf("load run after resume: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished after resume, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// formatOutputPreview tests
// ---------------------------------------------------------------------------

func TestFormatOutputPreview(t *testing.T) {
	tests := []struct {
		name string
		data map[string]interface{}
		want string // substring that must appear (empty = expect empty result)
	}{
		{
			name: "nil data",
			data: nil,
			want: "",
		},
		{
			name: "empty data",
			data: map[string]interface{}{},
			want: "",
		},
		{
			name: "only internal fields",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"_tokens": 100,
					"_model":  "gpt-4",
				},
			},
			want: "",
		},
		{
			name: "text-only output",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"text":    "Here is my analysis of the code.",
					"_tokens": 200,
				},
			},
			want: "Here is my analysis of the code.",
		},
		{
			name: "structured judge output",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"verdict":    "rejected",
					"reasoning":  "Missing error handling",
					"confidence": 0.85,
					"_tokens":    450,
					"_model":     "claude",
				},
			},
			want: "verdict: rejected",
		},
		{
			name: "structured judge reasoning appears",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"verdict":   "rejected",
					"reasoning": "Missing error handling",
					"_tokens":   450,
				},
			},
			want: "reasoning: Missing error handling",
		},
		{
			name: "verdict before reasoning in order",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"reasoning": "some reason",
					"verdict":   "approved",
					"_tokens":   100,
				},
			},
			want: "verdict: approved | reasoning: some reason",
		},
		{
			name: "router single route (no output wrapper)",
			data: map[string]interface{}{
				"selected_route": "fix_agent",
				"reasoning":      "Issues found",
			},
			want: "selected_route: fix_agent",
		},
		{
			name: "router multi route",
			data: map[string]interface{}{
				"selected_routes": []interface{}{"agent_a", "agent_b"},
				"reasoning":       "Both needed",
			},
			want: "selected_routes: [agent_a, agent_b]",
		},
		{
			name: "boolean field",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"approved": true,
					"feedback": "Looks good",
					"_tokens":  100,
				},
			},
			want: "approved: true",
		},
		{
			name: "long text is truncated",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"text":    strings.Repeat("x", 600),
					"_tokens": 100,
				},
			},
			want: "...",
		},
		{
			name: "newlines replaced with spaces",
			data: map[string]interface{}{
				"output": map[string]interface{}{
					"text":    "line1\nline2\nline3",
					"_tokens": 100,
				},
			},
			want: "line1 line2 line3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatOutputPreview(tt.data)
			if tt.want == "" {
				if got != "" {
					t.Errorf("expected empty string, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("expected result to contain %q, got %q", tt.want, got)
			}
		})
	}
}
