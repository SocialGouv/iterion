// Package e2e runs end-to-end scenarios that exercise the full pipeline:
// parse .iter file → compile to IR → execute on runtime engine → verify
// events, artifacts, verdicts, loops and metrics.
//
// Covered flagship workflows:
//   - pr_refine_single_model        — sequential, refine loop, global reloop
//   - pr_refine_dual_model_parallel — parallel fan-out/join, global reloop
//   - pr_refine_dual_model_parallel_compliance — human gate, alternating refine loop
//   - ci_fix_until_green            — tool node, bounded fix loop
package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/benchmark"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// compileFixture parses and compiles a .iter file from the examples directory.
func compileFixture(t *testing.T, name string) *ir.Workflow {
	t.Helper()
	path := filepath.Join("..", "examples", name)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	pr := parser.Parse(name, string(src))
	if len(pr.Diagnostics) > 0 {
		for _, d := range pr.Diagnostics {
			t.Logf("parse diagnostic: %s", d.Error())
		}
	}
	if pr.File == nil {
		t.Fatalf("parse returned nil AST for %s", name)
	}

	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			t.Logf("compile diagnostic: %s", d.Error())
		}
		t.Fatalf("compilation errors for %s", name)
	}

	return cr.Workflow
}

// compileFixtureStubSafe compiles the fixture and strips tools from all
// nodes so the workspace safety check doesn't reject parallel branches
// when running with stub executors (tools are never actually called).
func compileFixtureStubSafe(t *testing.T, name string) *ir.Workflow {
	t.Helper()
	wf := compileFixture(t, name)
	for _, node := range wf.Nodes {
		switch n := node.(type) {
		case *ir.AgentNode:
			n.Tools = nil
			n.ToolMaxSteps = 0
		case *ir.JudgeNode:
			n.Tools = nil
			n.ToolMaxSteps = 0
		}
	}
	return wf
}

func tmpStore(t *testing.T) *store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return s
}

// scenarioExecutor is a configurable stub executor for E2E tests.
type scenarioExecutor struct {
	mu       sync.Mutex
	handlers map[string]func(map[string]interface{}) (map[string]interface{}, error)
	calls    []string // ordered log of executed node IDs
}

func newScenarioExecutor() *scenarioExecutor {
	return &scenarioExecutor{
		handlers: make(map[string]func(map[string]interface{}) (map[string]interface{}, error)),
	}
}

func (e *scenarioExecutor) on(nodeID string, fn func(map[string]interface{}) (map[string]interface{}, error)) {
	e.handlers[nodeID] = fn
}

func (e *scenarioExecutor) Execute(_ context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	e.mu.Lock()
	e.calls = append(e.calls, node.NodeID())
	e.mu.Unlock()

	if fn, ok := e.handlers[node.NodeID()]; ok {
		return fn(input)
	}
	// Default: return empty output with a _tokens marker for metrics.
	return map[string]interface{}{"_tokens": 10, "_cost_usd": 0.001}, nil
}

func (e *scenarioExecutor) callCount(nodeID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, id := range e.calls {
		if id == nodeID {
			n++
		}
	}
	return n
}

func (e *scenarioExecutor) wasCalled(nodeID string) bool {
	return e.callCount(nodeID) > 0
}

// countEventType counts events of a given type in the event list.
func countEventType(events []*store.Event, t store.EventType) int {
	n := 0
	for _, evt := range events {
		if evt.Type == t {
			n++
		}
	}
	return n
}

// hasEvent checks whether at least one event of given type exists.
func hasEvent(events []*store.Event, t store.EventType) bool {
	return countEventType(events, t) > 0
}

// eventNodeIDs returns all node IDs for events of a given type.
func eventNodeIDs(events []*store.Event, t store.EventType) []string {
	var ids []string
	for _, evt := range events {
		if evt.Type == t {
			ids = append(ids, evt.NodeID)
		}
	}
	return ids
}

// ===========================================================================
// Scenario 1: pr_refine_single_model
// ===========================================================================

// TestSingleModel_HappyPath — all checks pass first try.
// Expected path: context_builder → reviewer → planner → compliance_check(approved)
//
//	→ act_on_plan → final_verify(approved) → done
func TestSingleModel_HappyPath(t *testing.T) {
	wf := compileFixture(t, "pr_refine_single_model.iter")
	exec := newScenarioExecutor()

	// Wire up stubs that produce the expected outputs.
	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pr_title": "fix: typo", "pr_description": "...", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+fix", "changed_files": []string{"a.go"},
			"repository_summary": "repo", "implementation_notes": []string{},
			"risky_areas": []string{}, "_tokens": 100, "_cost_usd": 0.01,
		}, nil
	})
	exec.on("reviewer", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "minor issues", "issues": []string{"typo"},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{"fix typo"}, "confidence": "high",
			"_tokens": 200, "_cost_usd": 0.02,
		}, nil
	})
	exec.on("planner", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "fix typo", "goals": []string{"fix"}, "ordered_steps": []string{"s1"},
			"validation_steps": []string{"v1"}, "risks": []string{},
			"addressed_issues": []string{"typo"}, "_tokens": 150, "_cost_usd": 0.015,
		}, nil
	})
	exec.on("compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
			"_tokens": 80, "_cost_usd": 0.008,
		}, nil
	})
	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "fixed", "files_changed": []string{"a.go"},
			"commands_run": []string{}, "tests_run": []string{"go test"},
			"remaining_risks": []string{}, "_tokens": 300, "_cost_usd": 0.03,
		}, nil
	})
	exec.on("final_verify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
			"_tokens": 100, "_cost_usd": 0.01,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-single-happy", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify run finished.
	r, _ := s.LoadRun("e2e-single-happy")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// Verify events contain the full sequence.
	events, _ := s.LoadEvents("e2e-single-happy")
	if !hasEvent(events, store.EventRunStarted) {
		t.Error("missing run_started event")
	}
	if !hasEvent(events, store.EventRunFinished) {
		t.Error("missing run_finished event")
	}

	// Verify artifacts for published nodes.
	art, err := s.LoadArtifact("e2e-single-happy", "context_builder", 0)
	if err != nil {
		t.Fatalf("load pr_context artifact: %v", err)
	}
	if art.Data["pr_title"] != "fix: typo" {
		t.Errorf("pr_context artifact pr_title = %v", art.Data["pr_title"])
	}

	actArt, err := s.LoadArtifact("e2e-single-happy", "act_on_plan", 0)
	if err != nil {
		t.Fatalf("load act_report artifact: %v", err)
	}
	if actArt.Data["applied"] != true {
		t.Errorf("act_report artifact applied = %v", actArt.Data["applied"])
	}

	verdictArt, err := s.LoadArtifact("e2e-single-happy", "final_verify", 0)
	if err != nil {
		t.Fatalf("load final_verdict artifact: %v", err)
	}
	if verdictArt.Data["approved"] != true {
		t.Errorf("final_verdict approved = %v", verdictArt.Data["approved"])
	}

	// Verify no refine loop was entered.
	if exec.wasCalled("refine_plan") {
		t.Error("refine_plan should not have been called in happy path")
	}

	// Verify metrics via benchmark.CollectMetrics.
	m, err := benchmark.CollectMetrics(s, "e2e-single-happy", "single_model", "approved")
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	if m.Status != "finished" {
		t.Errorf("metric status = %q", m.Status)
	}
	if m.TotalTokens == 0 {
		t.Error("metric tokens should be > 0")
	}
	if m.TotalCostUSD == 0 {
		t.Error("metric cost should be > 0")
	}
	if m.Verdict != "true" {
		t.Errorf("metric verdict = %q, want true", m.Verdict)
	}
}

// TestSingleModel_RefineLoop — compliance_check fails, enters refine loop,
// then compliance_check_after_refine approves.
func TestSingleModel_RefineLoop(t *testing.T) {
	wf := compileFixture(t, "pr_refine_single_model.iter")
	exec := newScenarioExecutor()

	refineCount := 0

	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pr_title": "fix", "pr_description": "", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+", "changed_files": []string{"a.go"},
			"repository_summary": "repo", "implementation_notes": []string{},
			"risky_areas": []string{},
		}, nil
	})
	exec.on("reviewer", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "issues", "issues": []string{"x"},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "medium",
		}, nil
	})
	exec.on("planner", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "plan", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	})
	exec.on("compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Always reject to enter the refine loop.
		return map[string]interface{}{
			"approved": false, "issues": []string{"incomplete"},
			"blocking_reasons": []string{"gap"}, "recommended_fixes": []string{"fix"},
			"confidence": "medium",
		}, nil
	})
	exec.on("refine_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		refineCount++
		return map[string]interface{}{
			"summary": "refined", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	})
	exec.on("compliance_check_after_refine", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Approve on first refinement.
		if refineCount >= 1 {
			return map[string]interface{}{
				"approved": true, "issues": []string{}, "blocking_reasons": []string{},
				"recommended_fixes": []string{}, "confidence": "high",
			}, nil
		}
		return map[string]interface{}{
			"approved": false, "issues": []string{"still bad"},
			"blocking_reasons": []string{"gap"}, "recommended_fixes": []string{},
			"confidence": "low",
		}, nil
	})
	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "done", "files_changed": []string{},
			"commands_run": []string{}, "tests_run": []string{},
			"remaining_risks": []string{},
		}, nil
	})
	exec.on("final_verify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-single-refine", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-single-refine")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// Verify the refine loop was entered.
	if refineCount == 0 {
		t.Error("refine_plan should have been called at least once")
	}

	// Verify loop edge events exist.
	events, _ := s.LoadEvents("e2e-single-refine")
	loopEdges := 0
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data != nil {
			if _, ok := evt.Data["loop"]; ok {
				loopEdges++
			}
		}
	}
	if loopEdges == 0 {
		t.Error("expected at least one loop edge event")
	}
}

// TestSingleModel_GlobalReloop — final_verify rejects, causing a global
// reloop back to context_builder.
func TestSingleModel_GlobalReloop(t *testing.T) {
	wf := compileFixture(t, "pr_refine_single_model.iter")
	exec := newScenarioExecutor()

	contextBuilderCalls := 0

	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		contextBuilderCalls++
		return map[string]interface{}{
			"pr_title": "fix", "pr_description": "", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+", "changed_files": []string{},
			"repository_summary": "repo", "implementation_notes": []string{},
			"risky_areas": []string{},
		}, nil
	})
	exec.on("reviewer", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "x", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	})
	exec.on("planner", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "p", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	})
	exec.on("compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})
	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "done", "files_changed": []string{},
			"commands_run": []string{}, "tests_run": []string{},
			"remaining_risks": []string{},
		}, nil
	})
	exec.on("final_verify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Reject on first pass, approve on second.
		if contextBuilderCalls >= 2 {
			return map[string]interface{}{
				"approved": true, "issues": []string{}, "blocking_reasons": []string{},
				"recommended_fixes": []string{}, "confidence": "high",
			}, nil
		}
		return map[string]interface{}{
			"approved": false, "issues": []string{"not good enough"},
			"blocking_reasons": []string{}, "recommended_fixes": []string{},
			"confidence": "low",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-single-reloop", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-single-reloop")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// context_builder should have been called twice (global reloop).
	if contextBuilderCalls != 2 {
		t.Errorf("context_builder calls = %d, want 2", contextBuilderCalls)
	}

	// Verify artifact versioning: context_builder should have version 0 and 1.
	art1, err := s.LoadArtifact("e2e-single-reloop", "context_builder", 1)
	if err != nil {
		t.Fatalf("load context_builder artifact v1: %v", err)
	}
	if art1.Data["pr_title"] != "fix" {
		t.Errorf("context_builder v1 pr_title = %v", art1.Data["pr_title"])
	}

	// Verify final verdict is approved.
	verdictArt, _ := s.LoadLatestArtifact("e2e-single-reloop", "final_verify")
	if verdictArt == nil {
		t.Fatal("missing final_verdict artifact")
	}
	if verdictArt.Data["approved"] != true {
		t.Errorf("final_verdict approved = %v", verdictArt.Data["approved"])
	}
}

// ===========================================================================
// Scenario 2: pr_refine_dual_model_parallel
// ===========================================================================

// TestDualParallel_HappyPath — both models review in parallel, plans are
// synthesized, merged, act, final reviews approve.
func TestDualParallel_HappyPath(t *testing.T) {
	wf := compileFixtureStubSafe(t, "pr_refine_dual_model_parallel.iter")
	exec := newScenarioExecutor()

	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pr_title": "feat: parallel", "pr_description": "", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+code", "changed_files": []string{"b.go"},
			"repository_summary": "repo", "implementation_notes": []string{},
			"risky_areas": []string{}, "_tokens": 100, "_cost_usd": 0.01,
		}, nil
	})

	reviewOutput := func() (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "review", "issues": []string{"i1"},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
			"_tokens": 150, "_cost_usd": 0.015,
		}, nil
	}
	exec.on("claude_review", func(_ map[string]interface{}) (map[string]interface{}, error) { return reviewOutput() })
	exec.on("gpt_review", func(_ map[string]interface{}) (map[string]interface{}, error) { return reviewOutput() })

	planOutput := func() (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "plan", "goals": []string{}, "ordered_steps": []string{"s1"},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{}, "_tokens": 120, "_cost_usd": 0.012,
		}, nil
	}
	exec.on("claude_plan", func(_ map[string]interface{}) (map[string]interface{}, error) { return planOutput() })
	exec.on("gpt_plan", func(_ map[string]interface{}) (map[string]interface{}, error) { return planOutput() })

	synthOutput := func() (map[string]interface{}, error) {
		return map[string]interface{}{
			"merged_summary": "synth", "agreements": []string{}, "disagreements": []string{},
			"missing_items": []string{}, "final_steps": []string{}, "risk_notes": []string{},
			"_tokens": 100, "_cost_usd": 0.01,
		}, nil
	}
	exec.on("claude_plan_synthesis", func(_ map[string]interface{}) (map[string]interface{}, error) { return synthOutput() })
	exec.on("gpt_plan_synthesis", func(_ map[string]interface{}) (map[string]interface{}, error) { return synthOutput() })

	exec.on("final_plan_merge", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "merged plan", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{}, "_tokens": 100, "_cost_usd": 0.01,
		}, nil
	})
	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "applied", "files_changed": []string{},
			"commands_run": []string{}, "tests_run": []string{},
			"remaining_risks": []string{}, "_tokens": 300, "_cost_usd": 0.03,
		}, nil
	})

	finalReviewOutput := func() (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "summary": "lgtm", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
			"_tokens": 100, "_cost_usd": 0.01,
		}, nil
	}
	exec.on("claude_final_review", func(_ map[string]interface{}) (map[string]interface{}, error) { return finalReviewOutput() })
	exec.on("gpt_final_review", func(_ map[string]interface{}) (map[string]interface{}, error) { return finalReviewOutput() })

	exec.on("final_compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
			"_tokens": 80, "_cost_usd": 0.008,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-dual-happy", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-dual-happy")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// Verify parallel branches were used (branch_started events).
	events, _ := s.LoadEvents("e2e-dual-happy")
	branchEvents := countEventType(events, store.EventBranchStarted)
	if branchEvents == 0 {
		t.Error("expected branch_started events for parallel execution")
	}

	// Verify both review models were called.
	if !exec.wasCalled("claude_review") {
		t.Error("claude_review was not called")
	}
	if !exec.wasCalled("gpt_review") {
		t.Error("gpt_review was not called")
	}

	// Verify join events.
	joinEvents := countEventType(events, store.EventJoinReady)
	if joinEvents == 0 {
		t.Error("expected join_ready events")
	}

	// Verify published artifacts.
	prCtx, err := s.LoadArtifact("e2e-dual-happy", "context_builder", 0)
	if err != nil {
		t.Fatalf("missing pr_context artifact: %v", err)
	}
	if prCtx.Data["pr_title"] != "feat: parallel" {
		t.Errorf("pr_context.pr_title = %v", prCtx.Data["pr_title"])
	}

	mergedPlan, err := s.LoadArtifact("e2e-dual-happy", "final_plan_merge", 0)
	if err != nil {
		t.Fatalf("missing final_merged_plan artifact: %v", err)
	}
	if mergedPlan.Data["summary"] != "merged plan" {
		t.Errorf("merged_plan.summary = %v", mergedPlan.Data["summary"])
	}

	verdict, err := s.LoadArtifact("e2e-dual-happy", "final_compliance_check", 0)
	if err != nil {
		t.Fatalf("missing final_verdict artifact: %v", err)
	}
	if verdict.Data["approved"] != true {
		t.Errorf("final_verdict.approved = %v", verdict.Data["approved"])
	}

	// Verify metrics.
	m, _ := benchmark.CollectMetrics(s, "e2e-dual-happy", "dual_parallel", "approved")
	if m.TotalTokens == 0 {
		t.Error("metrics tokens should be > 0")
	}
	if m.ModelCalls == 0 {
		t.Error("metrics model_calls should be > 0")
	}
}

// TestDualParallel_GlobalReloop — final compliance check rejects,
// then approves on second full pass.
func TestDualParallel_GlobalReloop(t *testing.T) {
	wf := compileFixtureStubSafe(t, "pr_refine_dual_model_parallel.iter")
	exec := newScenarioExecutor()

	contextCalls := 0
	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		contextCalls++
		return map[string]interface{}{
			"pr_title": "feat", "pr_description": "", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+", "changed_files": []string{},
			"repository_summary": "r", "implementation_notes": []string{},
			"risky_areas": []string{},
		}, nil
	})

	genericReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "r", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_review", genericReview)
	exec.on("gpt_review", genericReview)

	genericPlan := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "p", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	}
	exec.on("claude_plan", genericPlan)
	exec.on("gpt_plan", genericPlan)

	genericSynth := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"merged_summary": "s", "agreements": []string{}, "disagreements": []string{},
			"missing_items": []string{}, "final_steps": []string{}, "risk_notes": []string{},
		}, nil
	}
	exec.on("claude_plan_synthesis", genericSynth)
	exec.on("gpt_plan_synthesis", genericSynth)

	exec.on("final_plan_merge", genericPlan)
	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "done", "files_changed": []string{},
			"commands_run": []string{}, "tests_run": []string{},
			"remaining_risks": []string{},
		}, nil
	})

	genericFinalReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "summary": "ok", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_final_review", genericFinalReview)
	exec.on("gpt_final_review", genericFinalReview)

	exec.on("final_compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		if contextCalls >= 2 {
			return map[string]interface{}{
				"approved": true, "issues": []string{}, "blocking_reasons": []string{},
				"recommended_fixes": []string{}, "confidence": "high",
			}, nil
		}
		return map[string]interface{}{
			"approved": false, "issues": []string{"no"}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "low",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-dual-reloop", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-dual-reloop")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	if contextCalls != 2 {
		t.Errorf("context_builder calls = %d, want 2 (global reloop)", contextCalls)
	}
}

// ===========================================================================
// Scenario 3: pr_refine_dual_model_parallel_compliance
// ===========================================================================

// TestCompliance_HappyPath_NoHumanGate — compliance passes, technical
// decision gate says no human needed → straight to act.
func TestCompliance_HappyPath_NoHumanGate(t *testing.T) {
	wf := compileFixtureStubSafe(t, "pr_refine_dual_model_parallel_compliance.iter")
	exec := newScenarioExecutor()

	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pr_title": "feat", "pr_description": "", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+", "changed_files": []string{},
			"repository_summary": "r", "implementation_notes": []string{},
			"risky_areas": []string{},
		}, nil
	})

	genericReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "r", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_review", genericReview)
	exec.on("gpt_review", genericReview)

	genericPlan := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "p", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	}
	exec.on("claude_plan", genericPlan)
	exec.on("gpt_plan", genericPlan)

	genericSynth := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"merged_summary": "s", "agreements": []string{}, "disagreements": []string{},
			"missing_items": []string{}, "final_steps": []string{}, "risk_notes": []string{},
		}, nil
	}
	exec.on("claude_plan_synthesis", genericSynth)
	exec.on("gpt_plan_synthesis", genericSynth)
	exec.on("final_plan_merge", genericPlan)

	exec.on("plan_compliance_check_initial", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})
	exec.on("technical_decision_gate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"needs_human_input": false, "summary": "no human needed",
			"decision_areas": []string{}, "questions": []string{},
			"rationales": []string{}, "expected_decisions": []string{},
		}, nil
	})
	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "done", "files_changed": []string{},
			"commands_run": []string{}, "tests_run": []string{},
			"remaining_risks": []string{},
		}, nil
	})

	genericFinalReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "summary": "ok", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_final_review", genericFinalReview)
	exec.on("gpt_final_review", genericFinalReview)
	exec.on("final_pr_compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-comp-nohuman", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-comp-nohuman")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// Human checkpoint should NOT have been reached.
	if exec.wasCalled("integrate_human_clarifications") {
		t.Error("should not have called integrate_human_clarifications when no human needed")
	}
}

// TestCompliance_HumanGate — technical decision gate needs human,
// run pauses, resume continues to completion.
func TestCompliance_HumanGate(t *testing.T) {
	wf := compileFixtureStubSafe(t, "pr_refine_dual_model_parallel_compliance.iter")
	exec := newScenarioExecutor()

	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pr_title": "feat", "pr_description": "", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+", "changed_files": []string{},
			"repository_summary": "r", "implementation_notes": []string{},
			"risky_areas": []string{},
		}, nil
	})

	genericReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "r", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_review", genericReview)
	exec.on("gpt_review", genericReview)

	genericPlan := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "p", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	}
	exec.on("claude_plan", genericPlan)
	exec.on("gpt_plan", genericPlan)

	genericSynth := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"merged_summary": "s", "agreements": []string{}, "disagreements": []string{},
			"missing_items": []string{}, "final_steps": []string{}, "risk_notes": []string{},
		}, nil
	}
	exec.on("claude_plan_synthesis", genericSynth)
	exec.on("gpt_plan_synthesis", genericSynth)
	exec.on("final_plan_merge", genericPlan)

	exec.on("plan_compliance_check_initial", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})
	exec.on("technical_decision_gate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"needs_human_input": true, "summary": "need clarification",
			"decision_areas": []string{"architecture"}, "questions": []string{"which pattern?"},
			"rationales": []string{"ambiguous"}, "expected_decisions": []string{"DDD"},
		}, nil
	})
	exec.on("integrate_human_clarifications", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "updated plan", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	})
	exec.on("plan_compliance_check_post_human", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})
	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "done", "files_changed": []string{},
			"commands_run": []string{}, "tests_run": []string{},
			"remaining_risks": []string{},
		}, nil
	})

	genericFinalReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "summary": "ok", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_final_review", genericFinalReview)
	exec.on("gpt_final_review", genericFinalReview)
	exec.on("final_pr_compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	// Phase 1: Run should pause at human checkpoint.
	err := eng.Run(context.Background(), "e2e-comp-human", nil)
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused, got: %v", err)
	}

	run, _ := s.LoadRun("e2e-comp-human")
	if run.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status = %s, want paused_waiting_human", run.Status)
	}
	if run.Checkpoint == nil {
		t.Fatal("checkpoint should be set")
	}
	if run.Checkpoint.NodeID != "technical_decision_human_checkpoint" {
		t.Errorf("checkpoint node = %q, want technical_decision_human_checkpoint", run.Checkpoint.NodeID)
	}

	// Verify human_input_requested event.
	events, _ := s.LoadEvents("e2e-comp-human")
	if !hasEvent(events, store.EventHumanInputRequested) {
		t.Error("missing human_input_requested event")
	}
	if !hasEvent(events, store.EventRunPaused) {
		t.Error("missing run_paused event")
	}

	// Verify human decisions artifact is published.
	// (It will be written on resume.)

	// Phase 2: Resume with human answers.
	answers := map[string]interface{}{
		"answered":  true,
		"questions": []string{"which pattern?"},
		"answers":   []string{"use DDD"},
		"responder": "lead-dev",
		"notes":     "confirmed in team meeting",
	}
	err = eng.Resume(context.Background(), "e2e-comp-human", answers)
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}

	run, _ = s.LoadRun("e2e-comp-human")
	if run.Status != store.RunStatusFinished {
		t.Errorf("status after resume = %s, want finished", run.Status)
	}

	// Verify the human clarifications were integrated.
	if !exec.wasCalled("integrate_human_clarifications") {
		t.Error("integrate_human_clarifications was not called after resume")
	}

	// Verify post-resume events.
	events, _ = s.LoadEvents("e2e-comp-human")
	if !hasEvent(events, store.EventHumanAnswersRecorded) {
		t.Error("missing human_answers_recorded event")
	}
	if !hasEvent(events, store.EventRunResumed) {
		t.Error("missing run_resumed event")
	}
	if !hasEvent(events, store.EventRunFinished) {
		t.Error("missing run_finished event after resume")
	}

	// Verify human_decisions artifact was persisted.
	humanArt, err := s.LoadArtifact("e2e-comp-human", "technical_decision_human_checkpoint", 0)
	if err != nil {
		t.Fatalf("load human_decisions artifact: %v", err)
	}
	if humanArt.Data["responder"] != "lead-dev" {
		t.Errorf("human artifact responder = %v", humanArt.Data["responder"])
	}
}

// TestCompliance_RefineLoop — initial compliance fails, enters the
// alternating Claude/GPT refine loop.
func TestCompliance_RefineLoop(t *testing.T) {
	wf := compileFixtureStubSafe(t, "pr_refine_dual_model_parallel_compliance.iter")
	exec := newScenarioExecutor()

	exec.on("context_builder", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"pr_title": "feat", "pr_description": "", "base_ref": "main",
			"head_ref": "HEAD", "diff": "+", "changed_files": []string{},
			"repository_summary": "r", "implementation_notes": []string{},
			"risky_areas": []string{},
		}, nil
	})

	genericReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "summary": "r", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_review", genericReview)
	exec.on("gpt_review", genericReview)

	genericPlan := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "p", "goals": []string{}, "ordered_steps": []string{},
			"validation_steps": []string{}, "risks": []string{},
			"addressed_issues": []string{},
		}, nil
	}
	exec.on("claude_plan", genericPlan)
	exec.on("gpt_plan", genericPlan)

	genericSynth := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"merged_summary": "s", "agreements": []string{}, "disagreements": []string{},
			"missing_items": []string{}, "final_steps": []string{}, "risk_notes": []string{},
		}, nil
	}
	exec.on("claude_plan_synthesis", genericSynth)
	exec.on("gpt_plan_synthesis", genericSynth)
	exec.on("final_plan_merge", genericPlan)

	// Initial compliance FAILS → enters refine loop.
	exec.on("plan_compliance_check_initial", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "issues": []string{"gap"}, "blocking_reasons": []string{"x"},
			"recommended_fixes": []string{"fix"}, "confidence": "medium",
		}, nil
	})

	exec.on("refine_plan_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return genericPlan(nil)
	})

	// After Claude refine, compliance approves.
	exec.on("plan_compliance_check_after_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})

	exec.on("act_on_plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "done", "files_changed": []string{},
			"commands_run": []string{}, "tests_run": []string{},
			"remaining_risks": []string{},
		}, nil
	})

	genericFinalReview := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "summary": "ok", "issues": []string{},
			"blockers": []string{}, "compliance_gaps": []string{},
			"recommendations": []string{}, "confidence": "high",
		}, nil
	}
	exec.on("claude_final_review", genericFinalReview)
	exec.on("gpt_final_review", genericFinalReview)
	exec.on("final_pr_compliance_check", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "issues": []string{}, "blocking_reasons": []string{},
			"recommended_fixes": []string{}, "confidence": "high",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-comp-refine", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-comp-refine")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// Verify the refine loop was entered (Claude refine was called).
	if !exec.wasCalled("refine_plan_claude") {
		t.Error("refine_plan_claude should have been called")
	}

	// Verify loop edge events.
	events, _ := s.LoadEvents("e2e-comp-refine")
	loopEdges := 0
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data != nil {
			if _, ok := evt.Data["loop"]; ok {
				loopEdges++
			}
		}
	}
	if loopEdges == 0 {
		t.Error("expected loop edge events for plan_refine_loop")
	}
}

// ===========================================================================
// Scenario 4: ci_fix_until_green
// ===========================================================================

// TestCIFix_HappyPath — CI passes on first try after fix.
func TestCIFix_HappyPath(t *testing.T) {
	wf := compileFixture(t, "ci_fix_until_green.iter")
	exec := newScenarioExecutor()

	exec.on("diagnose", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"root_cause": "missing import", "affected_files": []string{"main.go"},
			"error_type": "compile", "confidence": "high",
			"suggested_approach": "add import", "details": []string{},
			"_tokens": 100, "_cost_usd": 0.01,
		}, nil
	})
	exec.on("plan_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "add import", "ordered_steps": []string{"add import"},
			"files_to_modify": []string{"main.go"}, "expected_outcome": "compiles",
			"risks": []string{}, "_tokens": 80, "_cost_usd": 0.008,
		}, nil
	})
	exec.on("act_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "import added",
			"files_changed": []string{"main.go"}, "commands_run": []string{},
			"_tokens": 150, "_cost_usd": 0.015,
		}, nil
	})
	exec.on("run_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"passed": true, "exit_code": 0, "logs": "all tests pass",
			"failed_tests": []string{},
		}, nil
	})
	exec.on("verify_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"green": true, "remaining_failures": []string{},
			"summary": "CI green", "confidence": "high",
			"_tokens": 60, "_cost_usd": 0.006,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-ci-happy", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-ci-happy")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// Verify the tool node (run_ci) was called.
	if !exec.wasCalled("run_ci") {
		t.Error("run_ci tool node should have been called")
	}

	// Verify published artifacts.
	diag, err := s.LoadArtifact("e2e-ci-happy", "diagnose", 0)
	if err != nil {
		t.Fatalf("missing diagnosis artifact: %v", err)
	}
	if diag.Data["root_cause"] != "missing import" {
		t.Errorf("diagnosis.root_cause = %v", diag.Data["root_cause"])
	}

	fixReport, err := s.LoadArtifact("e2e-ci-happy", "act_fix", 0)
	if err != nil {
		t.Fatalf("missing fix_report artifact: %v", err)
	}
	if fixReport.Data["applied"] != true {
		t.Errorf("fix_report.applied = %v", fixReport.Data["applied"])
	}

	ciVerdict, err := s.LoadArtifact("e2e-ci-happy", "verify_ci", 0)
	if err != nil {
		t.Fatalf("missing ci_verdict artifact: %v", err)
	}
	if ciVerdict.Data["green"] != true {
		t.Errorf("ci_verdict.green = %v", ciVerdict.Data["green"])
	}

	// No loops should have been entered.
	events, _ := s.LoadEvents("e2e-ci-happy")
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data != nil {
			if _, ok := evt.Data["loop"]; ok {
				t.Error("no loop edges expected in happy path")
			}
		}
	}

	// Verify metrics.
	m, _ := benchmark.CollectMetrics(s, "e2e-ci-happy", "ci_fix", "green")
	if m.Status != "finished" {
		t.Errorf("metrics status = %q", m.Status)
	}
	if m.TotalTokens == 0 {
		t.Error("metrics tokens should be > 0")
	}
	if m.Verdict != "true" {
		t.Errorf("metrics verdict = %q, want true", m.Verdict)
	}
}

// TestCIFix_FixLoop — CI fails first try, loops back to diagnose, then
// succeeds on second attempt.
func TestCIFix_FixLoop(t *testing.T) {
	wf := compileFixture(t, "ci_fix_until_green.iter")
	exec := newScenarioExecutor()

	diagnoseCalls := 0
	exec.on("diagnose", func(_ map[string]interface{}) (map[string]interface{}, error) {
		diagnoseCalls++
		return map[string]interface{}{
			"root_cause": "error", "affected_files": []string{"main.go"},
			"error_type": "test", "confidence": "high",
			"suggested_approach": "fix test", "details": []string{},
		}, nil
	})
	exec.on("plan_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "fix", "ordered_steps": []string{},
			"files_to_modify": []string{}, "expected_outcome": "pass",
			"risks": []string{},
		}, nil
	})
	exec.on("act_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "fixed",
			"files_changed": []string{}, "commands_run": []string{},
		}, nil
	})
	exec.on("run_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"passed": diagnoseCalls >= 2, "exit_code": 0,
			"logs": "logs", "failed_tests": []string{},
		}, nil
	})
	exec.on("verify_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		if diagnoseCalls >= 2 {
			return map[string]interface{}{
				"green": true, "remaining_failures": []string{},
				"summary": "CI green", "confidence": "high",
			}, nil
		}
		return map[string]interface{}{
			"green": false, "remaining_failures": []string{"test_x"},
			"summary": "still failing", "confidence": "high",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-ci-loop", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("e2e-ci-loop")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	// Diagnose should have been called twice (loop back).
	if diagnoseCalls != 2 {
		t.Errorf("diagnose calls = %d, want 2", diagnoseCalls)
	}

	// Verify loop edge events.
	events, _ := s.LoadEvents("e2e-ci-loop")
	loopEdges := 0
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data != nil {
			if _, ok := evt.Data["loop"]; ok {
				loopEdges++
			}
		}
	}
	if loopEdges == 0 {
		t.Error("expected loop edge events for fix_loop")
	}

	// Verify artifact versioning for diagnose (ran twice with publish).
	latestDiag, err := s.LoadLatestArtifact("e2e-ci-loop", "diagnose")
	if err != nil {
		t.Fatalf("load latest diagnose artifact: %v", err)
	}
	if latestDiag.Version != 1 {
		t.Errorf("diagnose latest version = %d, want 1", latestDiag.Version)
	}

	// Verify ci_verdict artifact shows green on final iteration.
	latestVerdict, err := s.LoadLatestArtifact("e2e-ci-loop", "verify_ci")
	if err != nil {
		t.Fatalf("load latest ci_verdict: %v", err)
	}
	if latestVerdict.Data["green"] != true {
		t.Errorf("final ci_verdict.green = %v", latestVerdict.Data["green"])
	}
}

// TestCIFix_LoopExhaustion — CI never goes green, loop exhausts after 5 iterations.
func TestCIFix_LoopExhaustion(t *testing.T) {
	wf := compileFixture(t, "ci_fix_until_green.iter")
	exec := newScenarioExecutor()

	exec.on("diagnose", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"root_cause": "deep bug", "affected_files": []string{},
			"error_type": "logic", "confidence": "low",
			"suggested_approach": "investigate", "details": []string{},
		}, nil
	})
	exec.on("plan_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "attempt", "ordered_steps": []string{},
			"files_to_modify": []string{}, "expected_outcome": "maybe",
			"risks": []string{},
		}, nil
	})
	exec.on("act_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "tried",
			"files_changed": []string{}, "commands_run": []string{},
		}, nil
	})
	exec.on("run_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"passed": false, "exit_code": 1,
			"logs": "FAIL", "failed_tests": []string{"test_hard"},
		}, nil
	})
	exec.on("verify_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"green": false, "remaining_failures": []string{"test_hard"},
			"summary": "still broken", "confidence": "high",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-ci-exhaust", nil)
	if err == nil {
		t.Fatal("expected error from loop exhaustion")
	}

	r, _ := s.LoadRun("e2e-ci-exhaust")
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable", r.Status)
	}

	// Diagnose should have been called 5 times (max iterations of fix_loop).
	diagCalls := exec.callCount("diagnose")
	if diagCalls > 6 { // 5 loop iterations + initial = up to 6
		t.Errorf("diagnose called %d times, expected <= 6", diagCalls)
	}

	// Verify run_failed event.
	events, _ := s.LoadEvents("e2e-ci-exhaust")
	if !hasEvent(events, store.EventRunFailed) {
		t.Error("missing run_failed event")
	}
}

// ===========================================================================
// Cross-cutting: All fixtures compile and validate successfully
// ===========================================================================

func TestAllFixturesCompile(t *testing.T) {
	fixtures := []string{
		"pr_refine_single_model.iter",
		"pr_refine_dual_model_parallel.iter",
		"pr_refine_dual_model_parallel_compliance.iter",
		"ci_fix_until_green.iter",
		"pr_review.iter",
		"pr_review_fix.iter",
		"llm_router_task_dispatch.iter",
		"session_fork.iter",
		"recipe_benchmark.iter",
		"feature_request_dual_model.iter",
		"dual_model_plan_implement_review.iter",
		"session_review_fix.iter",
		"rust_to_go_port.iter",
		"exhaustive_dsl_coverage.iter",
	}

	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			wf := compileFixture(t, name)
			if wf == nil {
				t.Fatalf("nil workflow for %s", name)
			}
			if wf.Name == "" {
				t.Error("workflow has empty name")
			}
			if wf.Entry == "" {
				t.Error("workflow has empty entry")
			}
			if len(wf.Nodes) == 0 {
				t.Error("workflow has no nodes")
			}
			if len(wf.Edges) == 0 {
				t.Error("workflow has no edges")
			}

			// Verify entry node exists.
			if _, ok := wf.Nodes[wf.Entry]; !ok {
				t.Errorf("entry node %q not found in nodes", wf.Entry)
			}

			// Verify done/fail nodes exist.
			hasDone := false
			hasFail := false
			for _, n := range wf.Nodes {
				if n.NodeKind() == ir.NodeDone {
					hasDone = true
				}
				if n.NodeKind() == ir.NodeFail {
					hasFail = true
				}
			}
			if !hasDone {
				t.Error("workflow missing done node")
			}
			if !hasFail {
				t.Error("workflow missing fail node")
			}

			// IR validation is done during Compile; if we reach here, it passed.
		})
	}
}

// ===========================================================================
// Cross-cutting: Event sequence coherence
// ===========================================================================

func TestEventSequenceCoherence(t *testing.T) {
	// Use the simplest workflow to validate event ordering rules.
	wf := compileFixture(t, "ci_fix_until_green.iter")
	exec := newScenarioExecutor()

	exec.on("diagnose", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"root_cause": "x", "affected_files": []string{},
			"error_type": "y", "confidence": "high",
			"suggested_approach": "z", "details": []string{},
		}, nil
	})
	exec.on("plan_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"summary": "p", "ordered_steps": []string{},
			"files_to_modify": []string{}, "expected_outcome": "ok",
			"risks": []string{},
		}, nil
	})
	exec.on("act_fix", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "d",
			"files_changed": []string{}, "commands_run": []string{},
		}, nil
	})
	exec.on("run_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"passed": true, "exit_code": 0, "logs": "ok", "failed_tests": []string{},
		}, nil
	})
	exec.on("verify_ci", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"green": true, "remaining_failures": []string{},
			"summary": "green", "confidence": "high",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	if err := eng.Run(context.Background(), "e2e-events", nil); err != nil {
		t.Fatalf("run error: %v", err)
	}

	events, _ := s.LoadEvents("e2e-events")

	// Rule 1: First event must be run_started.
	if events[0].Type != store.EventRunStarted {
		t.Errorf("first event = %s, want run_started", events[0].Type)
	}

	// Rule 2: Last event must be run_finished or run_failed.
	last := events[len(events)-1]
	if last.Type != store.EventRunFinished && last.Type != store.EventRunFailed {
		t.Errorf("last event = %s, want run_finished or run_failed", last.Type)
	}

	// Rule 3: Every node_started has a matching node_finished.
	startedNodes := make(map[string]int)
	finishedNodes := make(map[string]int)
	for _, evt := range events {
		if evt.Type == store.EventNodeStarted {
			startedNodes[evt.NodeID]++
		}
		if evt.Type == store.EventNodeFinished {
			finishedNodes[evt.NodeID]++
		}
	}
	for nodeID, startCount := range startedNodes {
		finishCount := finishedNodes[nodeID]
		if finishCount != startCount {
			t.Errorf("node %q: started %d times but finished %d times", nodeID, startCount, finishCount)
		}
	}

	// Rule 4: Sequence numbers are monotonically increasing.
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Errorf("event seq not monotonic: [%d].seq=%d <= [%d].seq=%d",
				i, events[i].Seq, i-1, events[i-1].Seq)
		}
	}

	// Rule 5: Node started/finished order is consistent (started before finished for same node).
	lastNodeEvent := make(map[string]store.EventType)
	for _, evt := range events {
		if evt.Type == store.EventNodeStarted {
			if prev, ok := lastNodeEvent[evt.NodeID]; ok && prev == store.EventNodeStarted {
				// This is OK for looped nodes (node starts again after finishing).
			}
			lastNodeEvent[evt.NodeID] = store.EventNodeStarted
		}
		if evt.Type == store.EventNodeFinished {
			if prev, ok := lastNodeEvent[evt.NodeID]; ok && prev != store.EventNodeStarted {
				t.Errorf("node %q finished without prior start (last event was %s)", evt.NodeID, prev)
			}
			lastNodeEvent[evt.NodeID] = store.EventNodeFinished
		}
	}
}

// ===========================================================================
// Suppress unused import warnings
// ===========================================================================

var _ = strings.Contains // used in assertions
