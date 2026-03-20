package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/store"
)

// ===========================================================================
// P4-01: Parallel fan-out / join tests
// ===========================================================================

// ---------------------------------------------------------------------------
// Helper: build a simple fan-out workflow
//   entry -> router(fan_out_all) -> agent_a -> join
//                                -> agent_b -> join
//   join -> next_node -> done
// ---------------------------------------------------------------------------

func fanOutWorkflow(joinStrategy ir.JoinStrategy) *ir.Workflow {
	return &ir.Workflow{
		Name:  "fanout_test",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"agent_a": {ID: "agent_a", Kind: ir.NodeAgent},
			"agent_b": {ID: "agent_b", Kind: ir.NodeAgent},
			"merge": {
				ID:           "merge",
				Kind:         ir.NodeJoin,
				JoinStrategy: joinStrategy,
				Require:      []string{"agent_a", "agent_b"},
			},
			"finalize": {ID: "finalize", Kind: ir.NodeAgent},
			"done":     {ID: "done", Kind: ir.NodeDone},
			"fail":     {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router", With: []*ir.DataMapping{
				{Key: "context", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "summary"}}}, Raw: "{{outputs.entry.summary}}"},
			}},
			{From: "router", To: "agent_a", With: []*ir.DataMapping{
				{Key: "context", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "summary"}}}, Raw: "{{outputs.entry.summary}}"},
			}},
			{From: "router", To: "agent_b", With: []*ir.DataMapping{
				{Key: "context", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "summary"}}}, Raw: "{{outputs.entry.summary}}"},
			}},
			{From: "agent_a", To: "merge"},
			{From: "agent_b", To: "merge"},
			{From: "merge", To: "finalize", With: []*ir.DataMapping{
				{Key: "reviews", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"merge"}}}, Raw: "{{outputs.merge}}"},
			}},
			{From: "finalize", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// ---------------------------------------------------------------------------
// Test: fan-out with wait_all — both branches succeed
// ---------------------------------------------------------------------------

func TestFanOutWaitAllSuccess(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinWaitAll)

	var capturedFinalizeInput map[string]interface{}
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "PR context"}, nil
	})
	exec.on("agent_a", func(input map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "A says LGTM", "approved": true}, nil
	})
	exec.on("agent_b", func(input map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "B says needs work", "approved": false}, nil
	})
	exec.on("finalize", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedFinalizeInput = input
		return map[string]interface{}{"result": "merged"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-fanout-ok", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run finished.
	r, err := s.LoadRun("run-fanout-ok")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Verify both branch outputs are available in the join output.
	if capturedFinalizeInput == nil {
		t.Fatal("finalize node was never called")
	}
	reviews, ok := capturedFinalizeInput["reviews"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected reviews map, got %T: %v", capturedFinalizeInput["reviews"], capturedFinalizeInput["reviews"])
	}
	// The join output should contain agent_a and agent_b entries.
	if reviews["agent_a"] == nil {
		t.Error("join output missing agent_a")
	}
	if reviews["agent_b"] == nil {
		t.Error("join output missing agent_b")
	}

	// Verify events contain branch_started events.
	events, err := s.LoadEvents("run-fanout-ok")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	branchStarted := 0
	joinReady := 0
	for _, evt := range events {
		if evt.Type == store.EventBranchStarted {
			branchStarted++
			if evt.BranchID == "" {
				t.Error("branch_started event missing branch_id")
			}
		}
		if evt.Type == store.EventJoinReady {
			joinReady++
		}
	}
	if branchStarted != 2 {
		t.Errorf("expected 2 branch_started events, got %d", branchStarted)
	}
	if joinReady != 1 {
		t.Errorf("expected 1 join_ready event, got %d", joinReady)
	}
}

// ---------------------------------------------------------------------------
// Test: fan-out with wait_all — one branch fails → run fails
// ---------------------------------------------------------------------------

func TestFanOutWaitAllPartialFailure(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinWaitAll)

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "context"}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "A ok"}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return nil, errors.New("LLM timeout")
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-fanout-fail", nil)
	if err == nil {
		t.Fatal("expected error from wait_all with failed branch")
	}

	r, err := s.LoadRun("run-fanout-fail")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected status failed, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: fan-out with best_effort — one branch fails → run continues
// ---------------------------------------------------------------------------

func TestFanOutBestEffortPartialFailure(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinBestEffort)

	var capturedFinalizeInput map[string]interface{}
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "context"}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "A says ok"}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return nil, errors.New("provider error")
	})
	exec.on("finalize", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedFinalizeInput = input
		return map[string]interface{}{"result": "partial merge"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-best-effort", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Run should finish successfully.
	r, err := s.LoadRun("run-best-effort")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}

	// Verify finalize received the join output with failed branches metadata.
	if capturedFinalizeInput == nil {
		t.Fatal("finalize node was never called")
	}
	reviews, ok := capturedFinalizeInput["reviews"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected reviews map, got %T", capturedFinalizeInput["reviews"])
	}

	// agent_a should be present.
	if reviews["agent_a"] == nil {
		t.Error("join output missing agent_a")
	}
	// agent_b should NOT be present (it failed).
	if reviews["agent_b"] != nil {
		t.Error("join output should not contain failed agent_b")
	}
	// Failed branches metadata should be exposed.
	if reviews["_failed_branches"] == nil {
		t.Error("join output missing _failed_branches metadata")
	}

	// Verify join_ready event includes failed_branches info.
	events, err := s.LoadEvents("run-best-effort")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	found := false
	for _, evt := range events {
		if evt.Type == store.EventJoinReady {
			found = true
			if evt.Data["failed_branches"] == nil {
				t.Error("join_ready event missing failed_branches data")
			}
		}
	}
	if !found {
		t.Error("no join_ready event found")
	}
}

// ---------------------------------------------------------------------------
// Test: branches actually execute concurrently
// ---------------------------------------------------------------------------

func TestFanOutConcurrentExecution(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinWaitAll)

	// Track execution overlap using channels.
	var maxConcurrent int64
	var currentConcurrent int64

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "go"}, nil
	})

	branchHandler := func(_ map[string]interface{}) (map[string]interface{}, error) {
		cur := atomic.AddInt64(&currentConcurrent, 1)
		// Track max concurrency.
		for {
			old := atomic.LoadInt64(&maxConcurrent)
			if cur <= old || atomic.CompareAndSwapInt64(&maxConcurrent, old, cur) {
				break
			}
		}
		// Small sleep to ensure overlap window.
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt64(&currentConcurrent, -1)
		return map[string]interface{}{"ok": true}, nil
	}

	exec.on("agent_a", branchHandler)
	exec.on("agent_b", branchHandler)
	exec.on("finalize", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-concurrent", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if atomic.LoadInt64(&maxConcurrent) < 2 {
		t.Errorf("expected concurrent execution (max concurrent=%d), branches ran sequentially", maxConcurrent)
	}
}

// ---------------------------------------------------------------------------
// Test: bounded parallelism respects max_parallel_branches
// ---------------------------------------------------------------------------

func TestFanOutBoundedParallelism(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "bounded_test",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":      {ID: "a", Kind: ir.NodeAgent},
			"b":      {ID: "b", Kind: ir.NodeAgent},
			"c":      {ID: "c", Kind: ir.NodeAgent},
			"join": {
				ID:           "join",
				Kind:         ir.NodeJoin,
				JoinStrategy: ir.JoinWaitAll,
				Require:      []string{"a", "b", "c"},
			},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "router", To: "c"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "c", To: "join"},
			{From: "join", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxParallelBranches: 2},
	}

	var maxConcurrent int64
	var currentConcurrent int64

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	branchHandler := func(_ map[string]interface{}) (map[string]interface{}, error) {
		cur := atomic.AddInt64(&currentConcurrent, 1)
		for {
			old := atomic.LoadInt64(&maxConcurrent)
			if cur <= old || atomic.CompareAndSwapInt64(&maxConcurrent, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&currentConcurrent, -1)
		return map[string]interface{}{"ok": true}, nil
	}

	exec.on("a", branchHandler)
	exec.on("b", branchHandler)
	exec.on("c", branchHandler)

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-bounded", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	max := atomic.LoadInt64(&maxConcurrent)
	if max > 2 {
		t.Errorf("expected max 2 concurrent branches (budget), got %d", max)
	}

	// Verify run succeeded.
	r, err := s.LoadRun("run-bounded")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: multi-step branches (review -> plan -> join)
// ---------------------------------------------------------------------------

func TestFanOutMultiStepBranches(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "multistep_test",
		Entry: "context_builder",
		Nodes: map[string]*ir.Node{
			"context_builder": {ID: "context_builder", Kind: ir.NodeAgent, Publish: "pr_context"},
			"review_fanout":   {ID: "review_fanout", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"claude_review":   {ID: "claude_review", Kind: ir.NodeAgent},
			"gpt_review":      {ID: "gpt_review", Kind: ir.NodeAgent},
			"claude_plan":     {ID: "claude_plan", Kind: ir.NodeAgent},
			"gpt_plan":        {ID: "gpt_plan", Kind: ir.NodeAgent},
			"plans_join": {
				ID:           "plans_join",
				Kind:         ir.NodeJoin,
				JoinStrategy: ir.JoinWaitAll,
				Require:      []string{"claude_plan", "gpt_plan"},
			},
			"merge": {ID: "merge", Kind: ir.NodeAgent},
			"done":  {ID: "done", Kind: ir.NodeDone},
			"fail":  {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "context_builder", To: "review_fanout", With: []*ir.DataMapping{
				{Key: "pr_context", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"context_builder"}}}, Raw: "{{outputs.context_builder}}"},
			}},
			// Fan-out to two parallel review branches.
			{From: "review_fanout", To: "claude_review", With: []*ir.DataMapping{
				{Key: "pr_context", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"context_builder"}}}, Raw: "{{outputs.context_builder}}"},
			}},
			{From: "review_fanout", To: "gpt_review", With: []*ir.DataMapping{
				{Key: "pr_context", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"context_builder"}}}, Raw: "{{outputs.context_builder}}"},
			}},
			// Each review leads to a plan (still within the branch).
			{From: "claude_review", To: "claude_plan", With: []*ir.DataMapping{
				{Key: "review", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"claude_review"}}}, Raw: "{{outputs.claude_review}}"},
			}},
			{From: "gpt_review", To: "gpt_plan", With: []*ir.DataMapping{
				{Key: "review", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"gpt_review"}}}, Raw: "{{outputs.gpt_review}}"},
			}},
			// Both plans converge to the join.
			{From: "claude_plan", To: "plans_join"},
			{From: "gpt_plan", To: "plans_join"},
			// Join -> merge -> done
			{From: "plans_join", To: "merge", With: []*ir.DataMapping{
				{Key: "claude_plan", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"claude_plan"}}}, Raw: "{{outputs.claude_plan}}"},
				{Key: "gpt_plan", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"gpt_plan"}}}, Raw: "{{outputs.gpt_plan}}"},
			}},
			{From: "merge", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var capturedMergeInput map[string]interface{}

	exec := newStubExecutor()
	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"diff": "+foo", "files": []string{"main.go"}}, nil
	})
	exec.on("claude_review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"issues": []string{"naming"}, "approved": false}, nil
	})
	exec.on("gpt_review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"issues": []string{"error handling"}, "approved": false}, nil
	})
	exec.on("claude_plan", func(input map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"steps": []string{"rename vars"}, "source": "claude"}, nil
	})
	exec.on("gpt_plan", func(input map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"steps": []string{"add error checks"}, "source": "gpt"}, nil
	})
	exec.on("merge", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedMergeInput = input
		return map[string]interface{}{"final_plan": "merged"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-multistep", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, err := s.LoadRun("run-multistep")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}

	// Verify merge received both plans.
	if capturedMergeInput == nil {
		t.Fatal("merge node was never called")
	}
	claudePlan, ok := capturedMergeInput["claude_plan"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected claude_plan map, got %T", capturedMergeInput["claude_plan"])
	}
	if claudePlan["source"] != "claude" {
		t.Errorf("claude_plan source = %v, want 'claude'", claudePlan["source"])
	}
	gptPlan, ok := capturedMergeInput["gpt_plan"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected gpt_plan map, got %T", capturedMergeInput["gpt_plan"])
	}
	if gptPlan["source"] != "gpt" {
		t.Errorf("gpt_plan source = %v, want 'gpt'", gptPlan["source"])
	}

	// Verify artifact was written for context_builder.
	art, err := s.LoadArtifact("run-multistep", "context_builder", 0)
	if err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if art.Data["diff"] != "+foo" {
		t.Errorf("artifact diff = %v", art.Data["diff"])
	}
}

// ---------------------------------------------------------------------------
// Test: context cancellation during fan-out
// ---------------------------------------------------------------------------

func TestFanOutContextCancellation(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinWaitAll)

	ctx, cancel := context.WithCancel(context.Background())

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "go"}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Cancel context while branches are running.
		cancel()
		time.Sleep(10 * time.Millisecond) // let cancellation propagate
		return nil, ctx.Err()
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(ctx, "run-cancel-fanout", nil)
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}

	r, err := s.LoadRun("run-cancel-fanout")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected status failed, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: branch events carry correct branch_id
// ---------------------------------------------------------------------------

func TestFanOutBranchEventIDs(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinWaitAll)

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "go"}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("finalize", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	if err := eng.Run(context.Background(), "run-branch-ids", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events, err := s.LoadEvents("run-branch-ids")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	// Collect unique branch IDs from branch events.
	branchIDs := map[string]bool{}
	for _, evt := range events {
		if evt.BranchID != "" {
			branchIDs[evt.BranchID] = true
		}
	}

	// Should have exactly 2 distinct branch IDs.
	if len(branchIDs) != 2 {
		t.Errorf("expected 2 distinct branch IDs, got %d: %v", len(branchIDs), branchIDs)
	}

	// Both should follow the naming convention.
	expected := map[string]bool{
		"branch_router_agent_a": true,
		"branch_router_agent_b": true,
	}
	for bid := range branchIDs {
		if !expected[bid] {
			t.Errorf("unexpected branch ID: %s", bid)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: best_effort with ALL branches failing
// ---------------------------------------------------------------------------

func TestFanOutBestEffortAllFail(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinBestEffort)

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "go"}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return nil, errors.New("fail A")
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return nil, errors.New("fail B")
	})
	exec.on("finalize", func(input map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-all-fail", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v (best_effort should tolerate all failures)", err)
	}

	r, err := s.LoadRun("run-all-fail")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished (best_effort), got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: double parallel review scenario (resembling PR workflow)
// ---------------------------------------------------------------------------

func TestDualReviewParallelToMerge(t *testing.T) {
	// context -> fanout -> claude_review -> join -> merge -> done
	//                   -> gpt_review    -> join
	wf := &ir.Workflow{
		Name:  "dual_review",
		Entry: "context",
		Nodes: map[string]*ir.Node{
			"context":       {ID: "context", Kind: ir.NodeAgent, Publish: "pr_ctx"},
			"review_fanout": {ID: "review_fanout", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"claude_review": {ID: "claude_review", Kind: ir.NodeAgent, Publish: "claude_verdict"},
			"gpt_review":    {ID: "gpt_review", Kind: ir.NodeAgent, Publish: "gpt_verdict"},
			"reviews_join": {
				ID:           "reviews_join",
				Kind:         ir.NodeJoin,
				JoinStrategy: ir.JoinWaitAll,
				Require:      []string{"claude_review", "gpt_review"},
			},
			"merge_reviews": {ID: "merge_reviews", Kind: ir.NodeAgent, Publish: "merged_review"},
			"done":          {ID: "done", Kind: ir.NodeDone},
			"fail":          {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "context", To: "review_fanout"},
			{From: "review_fanout", To: "claude_review", With: []*ir.DataMapping{
				{Key: "pr_ctx", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"context"}}}, Raw: "{{outputs.context}}"},
			}},
			{From: "review_fanout", To: "gpt_review", With: []*ir.DataMapping{
				{Key: "pr_ctx", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"context"}}}, Raw: "{{outputs.context}}"},
			}},
			{From: "claude_review", To: "reviews_join"},
			{From: "gpt_review", To: "reviews_join"},
			{From: "reviews_join", To: "merge_reviews", With: []*ir.DataMapping{
				{Key: "claude", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"claude_review"}}}, Raw: "{{outputs.claude_review}}"},
				{Key: "gpt", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"gpt_review"}}}, Raw: "{{outputs.gpt_review}}"},
			}},
			{From: "merge_reviews", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	var capturedMergeInput map[string]interface{}
	exec := newStubExecutor()
	exec.on("context", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"diff": "...", "title": "Add feature X"}, nil
	})
	exec.on("claude_review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"approved": true, "summary": "Claude: LGTM"}, nil
	})
	exec.on("gpt_review", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"approved": false, "summary": "GPT: needs work"}, nil
	})
	exec.on("merge_reviews", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedMergeInput = input
		return map[string]interface{}{"final_verdict": "needs work"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-dual-review", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, err := s.LoadRun("run-dual-review")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}

	// Verify merge received both reviews.
	if capturedMergeInput == nil {
		t.Fatal("merge_reviews never called")
	}
	claudeReview, ok := capturedMergeInput["claude"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected claude map, got %T", capturedMergeInput["claude"])
	}
	if claudeReview["summary"] != "Claude: LGTM" {
		t.Errorf("claude summary = %v", claudeReview["summary"])
	}

	// Verify artifacts for branch nodes.
	artClaude, err := s.LoadArtifact("run-dual-review", "claude_review", 0)
	if err != nil {
		t.Fatalf("load claude artifact: %v", err)
	}
	if artClaude.Data["approved"] != true {
		t.Errorf("claude artifact approved = %v", artClaude.Data["approved"])
	}

	artGPT, err := s.LoadArtifact("run-dual-review", "gpt_review", 0)
	if err != nil {
		t.Fatalf("load gpt artifact: %v", err)
	}
	if artGPT.Data["approved"] != false {
		t.Errorf("gpt artifact approved = %v", artGPT.Data["approved"])
	}
}

// ---------------------------------------------------------------------------
// Test: event ordering — branch events are properly interleaved
// ---------------------------------------------------------------------------

func TestFanOutEventOrdering(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinWaitAll)

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "go"}, nil
	})
	exec.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("finalize", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	if err := eng.Run(context.Background(), "run-evt-order", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events, err := s.LoadEvents("run-evt-order")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	// Extract event types in order (non-branch events).
	var mainTypes []store.EventType
	for _, evt := range events {
		if evt.BranchID == "" {
			mainTypes = append(mainTypes, evt.Type)
		}
	}

	// Main flow should be: run_started, node_started(entry), node_finished(entry),
	// edge_selected, node_started(router), node_finished(router),
	// node_started(join), join_ready, node_finished(join),
	// edge_selected, node_started(finalize), node_finished(finalize),
	// edge_selected, node_started(done), node_finished(done), run_finished

	// Verify run_started is first and run_finished is last.
	if len(mainTypes) == 0 {
		t.Fatal("no main flow events")
	}
	if mainTypes[0] != store.EventRunStarted {
		t.Errorf("first event = %s, want run_started", mainTypes[0])
	}
	if mainTypes[len(mainTypes)-1] != store.EventRunFinished {
		t.Errorf("last event = %s, want run_finished", mainTypes[len(mainTypes)-1])
	}

	// Verify join_ready appears exactly once.
	joinCount := 0
	for _, et := range mainTypes {
		if et == store.EventJoinReady {
			joinCount++
		}
	}
	if joinCount != 1 {
		t.Errorf("expected 1 join_ready, got %d", joinCount)
	}

	// Verify sequences are globally unique (no duplicates).
	seenSeqs := map[int64]bool{}
	for _, evt := range events {
		if seenSeqs[evt.Seq] {
			t.Errorf("duplicate sequence number %d", evt.Seq)
		}
		seenSeqs[evt.Seq] = true
	}

	// Verify per-branch sequences are monotonically increasing.
	branchLastSeq := map[string]int64{}
	for _, evt := range events {
		key := evt.BranchID // "" for main flow
		if last, ok := branchLastSeq[key]; ok && evt.Seq <= last {
			t.Errorf("non-monotonic seq within stream %q: %d <= %d", key, evt.Seq, last)
		}
		branchLastSeq[key] = evt.Seq
	}
}

// ---------------------------------------------------------------------------
// Test: fan-out with data flowing through branches
// ---------------------------------------------------------------------------

func TestFanOutDataFlow(t *testing.T) {
	wf := fanOutWorkflow(ir.JoinWaitAll)

	var capturedA, capturedB map[string]interface{}
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "important PR"}, nil
	})
	exec.on("agent_a", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedA = input
		return map[string]interface{}{"review": "A review"}, nil
	})
	exec.on("agent_b", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedB = input
		return map[string]interface{}{"review": "B review"}, nil
	})
	exec.on("finalize", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	if err := eng.Run(context.Background(), "run-dataflow", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both agents should have received the context from entry.
	if capturedA == nil || capturedB == nil {
		t.Fatal("agents were not called")
	}
	if capturedA["context"] != "important PR" {
		t.Errorf("agent_a context = %v, want 'important PR'", capturedA["context"])
	}
	if capturedB["context"] != "important PR" {
		t.Errorf("agent_b context = %v, want 'important PR'", capturedB["context"])
	}
}

// ---------------------------------------------------------------------------
// Test: sequential fan-outs (router -> join -> router -> join)
// ---------------------------------------------------------------------------

func TestSequentialFanOuts(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "seq_fanout_test",
		Entry: "entry",
		Nodes: map[string]*ir.Node{
			"entry":   {ID: "entry", Kind: ir.NodeAgent},
			"router1": {ID: "router1", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":       {ID: "a", Kind: ir.NodeAgent},
			"b":       {ID: "b", Kind: ir.NodeAgent},
			"join1":   {ID: "join1", Kind: ir.NodeJoin, JoinStrategy: ir.JoinWaitAll, Require: []string{"a", "b"}},
			"mid":     {ID: "mid", Kind: ir.NodeAgent},
			"router2": {ID: "router2", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"c":       {ID: "c", Kind: ir.NodeAgent},
			"d":       {ID: "d", Kind: ir.NodeAgent},
			"join2":   {ID: "join2", Kind: ir.NodeJoin, JoinStrategy: ir.JoinWaitAll, Require: []string{"c", "d"}},
			"done":    {ID: "done", Kind: ir.NodeDone},
			"fail":    {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router1"},
			{From: "router1", To: "a"},
			{From: "router1", To: "b"},
			{From: "a", To: "join1"},
			{From: "b", To: "join1"},
			{From: "join1", To: "mid"},
			{From: "mid", To: "router2"},
			{From: "router2", To: "c"},
			{From: "router2", To: "d"},
			{From: "c", To: "join2"},
			{From: "d", To: "join2"},
			{From: "join2", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	callLog := []string{}
	var mu sync.Mutex
	exec := newStubExecutor()

	register := func(id string) {
		exec.on(id, func(_ map[string]interface{}) (map[string]interface{}, error) {
			mu.Lock()
			callLog = append(callLog, id)
			mu.Unlock()
			return map[string]interface{}{"from": id}, nil
		})
	}
	for _, id := range []string{"entry", "a", "b", "mid", "c", "d"} {
		register(id)
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-seq-fanout", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, err := s.LoadRun("run-seq-fanout")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}

	// All 6 nodes should have been called.
	mu.Lock()
	defer mu.Unlock()
	if len(callLog) != 6 {
		t.Errorf("expected 6 node calls, got %d: %v", len(callLog), callLog)
	}

	// entry and mid must come before their respective fan-out groups.
	sort.Strings(callLog) // just verify all are present
	expected := []string{"a", "b", "c", "d", "entry", "mid"}
	for i, exp := range expected {
		if i >= len(callLog) || callLog[i] != exp {
			t.Errorf("callLog[%d] = %v, want %s", i, callLog[i], exp)
		}
	}
}

// keep import used
var _ = fmt.Sprintf
var _ = sort.Strings
var _ sync.Mutex
