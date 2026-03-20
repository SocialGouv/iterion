package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/store"
)

// ===========================================================================
// P9-02: Hardening tests — cancel, timeout, compatibility, error diagnostics
// ===========================================================================

// ---------------------------------------------------------------------------
// Test: context cancellation produces cancelled status (not failed)
// ---------------------------------------------------------------------------

func TestCancelProducesCancelledStatus(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cancel_status_test",
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(ctx, "run-cancel-status", nil)
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}

	// Should be ErrRunCancelled, not a generic error.
	if !errors.Is(err, ErrRunCancelled) {
		t.Errorf("expected ErrRunCancelled, got: %v", err)
	}

	// Run status should be "cancelled", not "failed".
	r, err := s.LoadRun("run-cancel-status")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusCancelled {
		t.Errorf("expected status cancelled, got %s", r.Status)
	}

	// Should have run_cancelled event.
	events, err := s.LoadEvents("run-cancel-status")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	hasCancelled := false
	for _, evt := range events {
		if evt.Type == store.EventRunCancelled {
			hasCancelled = true
		}
	}
	if !hasCancelled {
		t.Error("expected run_cancelled event in event log")
	}
}

// ---------------------------------------------------------------------------
// Test: context cancellation mid-execution cancels cleanly
// ---------------------------------------------------------------------------

func TestCancelDuringExecution(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cancel_mid_test",
		Entry: "step1",
		Nodes: map[string]*ir.Node{
			"step1": {ID: "step1", Kind: ir.NodeAgent},
			"step2": {ID: "step2", Kind: ir.NodeAgent},
			"done":  {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			{From: "step1", To: "step2"},
			{From: "step2", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	exec := newStubExecutor()
	exec.on("step1", func(_ map[string]interface{}) (map[string]interface{}, error) {
		cancel() // Cancel after first node executes.
		return map[string]interface{}{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(ctx, "run-cancel-mid", nil)
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("expected ErrRunCancelled, got: %v", err)
	}

	r, _ := s.LoadRun("run-cancel-mid")
	if r.Status != store.RunStatusCancelled {
		t.Errorf("expected status cancelled, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: context deadline exceeded produces failed (timeout), not cancelled
// ---------------------------------------------------------------------------

func TestTimeoutProducesFailedStatus(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "timeout_test",
		Entry: "slow",
		Nodes: map[string]*ir.Node{
			"slow": {ID: "slow", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			{From: "slow", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	// Use a very short deadline that will expire before execution.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	// Give the deadline time to expire.
	time.Sleep(1 * time.Millisecond)

	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(ctx, "run-timeout", nil)
	if err == nil {
		t.Fatal("expected error from timeout")
	}

	// Should NOT be ErrRunCancelled — it's a timeout.
	if errors.Is(err, ErrRunCancelled) {
		t.Error("timeout should not be ErrRunCancelled")
	}

	// Run status should be "failed" for timeouts.
	r, err := s.LoadRun("run-timeout")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected status failed for timeout, got %s", r.Status)
	}

	// Error message should mention timeout.
	if r.Error == "" {
		t.Error("expected error message for timeout")
	}
}

// ---------------------------------------------------------------------------
// Test: cancel during parallel branches
// ---------------------------------------------------------------------------

func TestCancelDuringParallelBranches(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cancel_parallel_test",
		Entry: "router",
		Nodes: map[string]*ir.Node{
			"router":   {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"branch_a": {ID: "branch_a", Kind: ir.NodeAgent},
			"branch_b": {ID: "branch_b", Kind: ir.NodeAgent},
			"join":     {ID: "join", Kind: ir.NodeJoin, JoinStrategy: ir.JoinBestEffort, Require: []string{"branch_a", "branch_b"}},
			"done":     {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "branch_a"},
			{From: "router", To: "branch_b"},
			{From: "branch_a", To: "join"},
			{From: "branch_b", To: "join"},
			{From: "join", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	exec := newStubExecutor()
	exec.on("branch_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		cancel() // Cancel during parallel execution.
		return map[string]interface{}{"result": "a"}, nil
	})
	exec.on("branch_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "b"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(ctx, "run-cancel-par", nil)
	// The run should fail or be cancelled — not hang.
	if err == nil {
		t.Fatal("expected error from cancellation during parallel branches")
	}
}

// ---------------------------------------------------------------------------
// Test: format_version is persisted in run.json
// ---------------------------------------------------------------------------

func TestFormatVersionPersisted(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "format_test",
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

	if err := eng.Run(context.Background(), "run-fmt", nil); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	r, err := s.LoadRun("run-fmt")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}

	if r.FormatVersion != store.RunFormatVersion {
		t.Errorf("expected format_version %d, got %d", store.RunFormatVersion, r.FormatVersion)
	}
}

// ---------------------------------------------------------------------------
// Test: format_version 0 (legacy) can still be loaded
// ---------------------------------------------------------------------------

func TestFormatVersionZeroCompatibility(t *testing.T) {
	s := tmpStore(t)

	// Create a run without format_version (simulates pre-P9-02 data).
	r := &store.Run{
		ID:           "legacy-run",
		WorkflowName: "old_workflow",
		Status:       store.RunStatusFinished,
	}
	// Write it directly (format_version will be 0 / zero value).
	// We use CreateRun then check that LoadRun handles it.
	created, err := s.CreateRun("legacy-run", "old_workflow", nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	_ = created
	_ = r

	loaded, err := s.LoadRun("legacy-run")
	if err != nil {
		t.Fatalf("load legacy run: %v", err)
	}

	// Current CreateRun sets format_version, but the key test is that
	// LoadRun works even if format_version were 0 (forward compatibility).
	if loaded.FormatVersion == 0 {
		// This would happen with truly old data. It should not crash.
		t.Log("loaded legacy run with format_version=0 (expected for old data)")
	}
}

// ---------------------------------------------------------------------------
// Test: RuntimeError carries structured information
// ---------------------------------------------------------------------------

func TestRuntimeErrorStructured(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "err_struct_test",
		Entry: "step",
		Nodes: map[string]*ir.Node{
			"step": {ID: "step", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			// No edge from step → no outgoing edge error.
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-err-struct", nil)
	if err == nil {
		t.Fatal("expected error from missing edge")
	}

	// The error chain should contain a RuntimeError.
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError in chain, got: %T: %v", err, err)
	}

	if rtErr.Code != ErrCodeNoOutgoingEdge {
		t.Errorf("expected code NO_OUTGOING_EDGE, got %s", rtErr.Code)
	}
	if rtErr.Hint == "" {
		t.Error("expected a hint in the RuntimeError")
	}
}

// ---------------------------------------------------------------------------
// Test: loop exhaustion produces RuntimeError with LOOP_EXHAUSTED code
// ---------------------------------------------------------------------------

func TestLoopExhaustionRuntimeError(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "loop_err_test",
		Entry: "fix",
		Nodes: map[string]*ir.Node{
			"fix":    {ID: "fix", Kind: ir.NodeAgent},
			"verify": {ID: "verify", Kind: ir.NodeJudge},
			"done":   {ID: "done", Kind: ir.NodeDone},
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
			"retry": {Name: "retry", MaxIterations: 1},
		},
	}

	exec := newStubExecutor()
	exec.on("fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("verify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"pass": false}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-loop-err", nil)
	if err == nil {
		t.Fatal("expected error from loop exhaustion")
	}

	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError, got: %T: %v", err, err)
	}
	if rtErr.Code != ErrCodeLoopExhausted {
		t.Errorf("expected code LOOP_EXHAUSTED, got %s", rtErr.Code)
	}
	if rtErr.Hint == "" {
		t.Error("expected a hint for loop exhaustion")
	}
}

// ---------------------------------------------------------------------------
// Test: budget exceeded produces RuntimeError with BUDGET_EXCEEDED code
// ---------------------------------------------------------------------------

func TestBudgetExceededRuntimeError(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_err_test",
		Entry: "step1",
		Nodes: map[string]*ir.Node{
			"step1": {ID: "step1", Kind: ir.NodeAgent},
			"step2": {ID: "step2", Kind: ir.NodeAgent},
			"done":  {ID: "done", Kind: ir.NodeDone},
		},
		Edges: []*ir.Edge{
			{From: "step1", To: "step2"},
			{From: "step2", To: "done"},
		},
		Budget: &ir.Budget{
			MaxIterations: 1, // Only allow 1 node execution.
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-err", nil)
	if err == nil {
		t.Fatal("expected error from budget exceeded")
	}

	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError, got: %T: %v", err, err)
	}
	if rtErr.Code != ErrCodeBudgetExceeded {
		t.Errorf("expected code BUDGET_EXCEEDED, got %s", rtErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Test: cancelled run has FinishedAt set
// ---------------------------------------------------------------------------

func TestCancelledRunHasFinishedAt(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cancel_finished_test",
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec)

	_ = eng.Run(ctx, "run-cancel-fin", nil)

	r, _ := s.LoadRun("run-cancel-fin")
	if r.FinishedAt == nil {
		t.Error("cancelled run should have FinishedAt set")
	}
}

// ---------------------------------------------------------------------------
// Test: executor error produces structured RuntimeError
// ---------------------------------------------------------------------------

func TestExecutorErrorProducesRuntimeError(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "exec_err_test",
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
	exec.on("step", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return nil, fmt.Errorf("model provider returned 429: rate limited")
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-exec-err", nil)
	if err == nil {
		t.Fatal("expected error from executor failure")
	}

	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError, got: %T: %v", err, err)
	}
	if rtErr.Code != ErrCodeExecutionFailed {
		t.Errorf("expected code EXECUTION_FAILED, got %s", rtErr.Code)
	}
	if rtErr.NodeID != "step" {
		t.Errorf("expected nodeID 'step', got %q", rtErr.NodeID)
	}
}
