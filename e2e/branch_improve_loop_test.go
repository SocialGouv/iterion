package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// branchCampaignState models the v2 branch-improve-loop "one agent, its natural
// flow" shape: a single adaptive `campaign` agent reviews the branch diff,
// improves it, and commits each fix in stride (git is the durable state — there
// is no chunk/worklist file the e2e stub has to model any more), then a
// deterministic build/test gate re-checks the tree and the continuation loop
// runs another pass until the campaign reports branch_clean AND the tree is
// green. The stub drives the ONE property the control flow depends on: the
// gate's converged decision (green ∧ branch_clean) keyed on the campaign's
// termination output and the verify verdict.
type branchCampaignState struct {
	// cleanBy: the campaign reports branch_clean=true on/after this pass
	// (1-based). Earlier passes report false with commits_this_pass committed.
	cleanBy      int
	pass         int      // how many campaign passes have run
	failLogsSeen []string // the input.fail_log the campaign saw on each pass
}

// stubBranchCampaign registers the baseline stubs for a green continuation:
// campaign (the adaptive agent), the verify gate (verify_probe regenerate →
// verify_build → verify_run green), and the MR tail (forge_auth_probe
// credential-present → finalize_mr). Individual tests override a node
// afterward (later .on wins) to exercise a red verify pass or the MR path.
// toStr is shared with whole_improve_loop_test.go (same package).
func stubBranchCampaign(exec *scenarioExecutor, st *branchCampaignState) {
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		st.pass++
		fl := ""
		if raw, ok := in["fail_log"]; ok {
			fl = strings.TrimSpace(toStr(raw))
		}
		st.failLogsSeen = append(st.failLogsSeen, fl)
		clean := st.pass >= st.cleanBy
		commits := 2
		remaining := "more branch issues to fix"
		if clean {
			commits = 1
			remaining = ""
		}
		return map[string]any{
			"branch_clean":      clean,
			"commits_this_pass": commits,
			"issues_remaining":  remaining,
			"needs_human":       false,
			"human_note":        "",
			"summary":           "reviewed + improved the branch diff this pass",
			"_tokens":           10,
		}, nil
	})
	// fresh=false routes every pass through verify_build → verify_run, the
	// flow the per-test call-count assertions are written against.
	exec.on("verify_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"fresh": false, "reason": "no verify.sh yet", "_tokens": 1}, nil
	})
	exec.on("verify_build", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"prepared": true, "summary": "verify.sh written", "_tokens": 1}, nil
	})
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})
	// The in-loop adversarial review stubs clean by default so a
	// green+complete pass converges; the review-blocks path is exercised
	// by its own test.
	exec.on("review", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"clean": true, "findings": "", "_tokens": 1}, nil
	})
	// available=true keeps the opt-in MR path reachable (finalize_mr fires
	// when open_mr=true); the probe only runs behind the open_mr gate.
	exec.on("forge_auth_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"available": true, "reason": "env:GH_TOKEN", "_tokens": 1}, nil
	})
	exec.on("finalize_mr", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"opened": true, "url": "https://forge/mr/1", "branch": "iterion/improve/x",
			"back_linked": false, "skipped_reason": "", "summary": "opened", "_tokens": 5,
		}, nil
	})
}

// TestBranchImproveLoop_ContinuesUntilClean is the canonical v2 flow: the
// campaign reports branch_clean=false on pass 1 (issues remain) then true on
// pass 2, the deterministic gate is green both times, and the continuation loop
// runs a second campaign pass before converging. Asserts:
//   - campaign runs exactly twice (a second pass because pass 1 was not clean);
//   - the verify gate runs each pass (verify_run == 2);
//   - the run finishes (converged → mr_gate → done, open_mr default false).
func TestBranchImproveLoop_ContinuesUntilClean(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 2}
	stubBranchCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-continue", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-continue")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (pass 1 not clean → continuation → pass 2 clean)", got)
	}
	if got := exec.callCount("verify_run"); got != 2 {
		t.Errorf("verify_run called %d times, want 2 (the deterministic gate runs each pass)", got)
	}
	if exec.wasCalled("finalize_mr") {
		t.Errorf("finalize_mr fired with open_mr=false — the MR path must be opt-in")
	}
	// Both continuation passes carried an empty fail_log (both gates were green;
	// the loop back was driven by branch_clean=false, not a red build).
	for i, fl := range st.failLogsSeen {
		if fl != "" {
			t.Errorf("pass %d saw fail_log %q, want empty (green gate → continuation, not a red-fix)", i+1, fl)
		}
	}
}

// TestBranchImproveLoop_ConvergesFirstPass pins the fast path: the campaign
// reports branch_clean=true on the first pass and the gate is green, so the run
// converges immediately — one campaign pass, straight to done.
func TestBranchImproveLoop_ConvergesFirstPass(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 1}
	stubBranchCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-first", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-first")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1 (branch_clean + green on the first pass converges immediately)", got)
	}
}

// TestBranchImproveLoop_RedVerifyRoutesBackToCampaign pins the tight
// real-feedback loop: the campaign reports branch_clean=true every pass, but the
// deterministic gate is RED on pass 1 (the campaign broke the build) and green
// on pass 2. The gate's converged = green ∧ branch_clean, so a red build must
// route back to campaign WITH the failure log so it fixes what it broke, even
// though the agent claimed the branch clean. Asserts:
//   - campaign runs twice (the red gate forced a fix pass despite branch_clean);
//   - the second campaign pass received the real build-failure log as input;
//   - the run finishes once the gate goes green.
func TestBranchImproveLoop_RedVerifyRoutesBackToCampaign(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 1} // the agent claims clean every pass
	stubBranchCampaign(exec, st)
	// Override the gate: red on the first run, green thereafter.
	verifyCalls := 0
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		verifyCalls++
		if verifyCalls == 1 {
			return map[string]any{
				"passed": false, "skipped": false, "exit_code": 1,
				"log_tail": "stub build failure: undefined symbol Foo", "_tokens": 1,
			}, nil
		}
		return map[string]any{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-red", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-red")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (a red gate forces a fix pass even though the agent claimed branch_clean)", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "stub build failure") {
		t.Errorf("second campaign pass fail_log = %v, want it to carry the real build failure so the agent fixes what it broke", st.failLogsSeen)
	}
}

// TestBranchImproveLoop_MRPathOnConverge pins the opt-in MR path: with
// open_mr=true a converged run opens the MR/PR (finalize_mr) before finishing.
func TestBranchImproveLoop_MRPathOnConverge(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 1}
	stubBranchCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	inputs := map[string]any{"open_mr": true}
	if err := eng.Run(context.Background(), "run-bil-mr", inputs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-mr")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if !exec.wasCalled("finalize_mr") {
		t.Errorf("finalize_mr did not fire with open_mr=true — the converged series must open an MR")
	}
}

// TestBranchImproveLoop_Structural pins the v2 IR shape: the campaign entry, the
// adaptive campaign/verify_build agents, the deterministic verify_run tool +
// gate/mr_gate computes, and the ABSENCE of every retired v1 node (the chunked
// cross-family review-fix-commit machinery). Drift here (e.g. reintroducing a
// blocking upfront plan_chunks, or a reviewer node) breaks the mechanism
// silently.
func TestBranchImproveLoop_Structural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")

	// Entry is the plan-phase gate (ADR-091), whose off branch routes
	// STRAIGHT to the campaign — the v2 "start working immediately" shape
	// is preserved whenever plan_review resolves off.
	if wf.Entry != "plan_topology" {
		t.Errorf("workflow entry = %q, want %q (the plan-phase gate; its off branch is the v2 immediate-campaign shape)", wf.Entry, "plan_topology")
	}
	if _, ok := wf.Nodes["plan_topology"].(*ir.ComputeNode); !ok {
		t.Errorf("plan_topology is %T, want *ir.ComputeNode (deterministic gate)", wf.Nodes["plan_topology"])
	}
	// Adaptive agents: campaign + verify_build.
	for _, id := range []string{"campaign", "verify_build"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected agent node %q", id)
		}
		if _, ok := node.(*ir.AgentNode); !ok {
			t.Errorf("node %q is %T, want *ir.AgentNode (adaptive)", id, node)
		}
	}
	// Deterministic (no-LLM): verify_run is a tool node; gate/mr_gate are computes.
	if node, ok := wf.Nodes["verify_run"]; !ok {
		t.Errorf("workflow missing expected tool node %q", "verify_run")
	} else if _, ok := node.(*ir.ToolNode); !ok {
		t.Errorf("node %q is %T, want *ir.ToolNode (deterministic gate)", "verify_run", node)
	}
	for _, id := range []string{"gate", "mr_gate"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected compute node %q", id)
		}
		if _, ok := node.(*ir.ComputeNode); !ok {
			t.Errorf("node %q is %T, want *ir.ComputeNode (deterministic)", id, node)
		}
	}
	// The retired v1 chunked cross-family review-fix-commit machinery must be gone.
	for _, id := range []string{
		"plan_chunks", "alt", "reviewer_claude", "reviewer_gpt", "streak_check",
		"fix_claude", "fix_gpt", "prepare_commit", "commit_changes",
	} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("retired v1 node %q is still present — v2 is one campaign agent, not the chunked review-fix assembly line", id)
		}
	}
}
