package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// featureDevState models feature_dev's v2 "one agent ships the feature"
// shape (ADR-058, sibling of whole_improve_loop's campaignState): a single
// adaptive `campaign` agent implements + commits the feature slice by slice
// (git is the durable state), then the deterministic build/test gate
// re-checks the tree and the continuation loop runs another pass until the
// campaign reports feature_complete AND the tree is green. The stub drives
// the ONE property the control flow depends on: gate.converged =
// verify_run.passed ∧ campaign.feature_complete.
type featureDevState struct {
	// completeBy: the campaign reports feature_complete=true on/after this
	// pass (1-based). Earlier passes report false with work remaining.
	completeBy   int
	pass         int      // how many campaign passes have run
	failLogsSeen []string // the input.fail_log the campaign saw on each pass
}

// stubFeatureDevCampaign registers the baseline stubs for a green
// continuation: campaign (the adaptive agent), the verify gate (verify_probe
// regenerate → verify_build → verify_run green), and the MR tail
// (forge_auth_probe credential-present → finalize_mr). Individual tests
// override a node afterward (later .on wins) to exercise a red verify pass
// or the MR path.
func stubFeatureDevCampaign(exec *scenarioExecutor, st *featureDevState) {
	// The entry precondition passes and the plan phase (on by default)
	// authors a plan; plan_review is unresolved (auto → off) in this
	// harness, so the peer never runs.
	stubWorkspaceProbeOK(exec)
	stubPlanAuthor(exec)
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		st.pass++
		fl := ""
		if raw, ok := in["fail_log"]; ok {
			fl = strings.TrimSpace(toStr(raw))
		}
		st.failLogsSeen = append(st.failLogsSeen, fl)
		complete := st.pass >= st.completeBy
		commits := 2
		remaining := "wire the handler + its tests"
		if complete {
			commits = 1
			remaining = ""
		}
		return map[string]any{
			"feature_complete":  complete,
			"commits_this_pass": commits,
			"work_remaining":    remaining,
			"needs_human":       false,
			"human_note":        "",
			"summary":           "shipped feature slices this pass",
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

// TestVibeFeatureDev_ConvergesFirstPass pins the fast path: the campaign
// reports feature_complete=true on the first pass and the gate is green, so
// the run converges immediately — one campaign pass, straight to done
// (open_mr defaults false → no MR).
func TestVibeFeatureDev_ConvergesFirstPass(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature-dev/main.bot")
	exec := newScenarioExecutor()
	st := &featureDevState{completeBy: 1}
	stubFeatureDevCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fd-first", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-fd-first")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1 (feature_complete + green on the first pass converges immediately)", got)
	}
	if exec.wasCalled("finalize_mr") {
		t.Errorf("finalize_mr fired with open_mr=false — the MR path must be opt-in")
	}
}

// TestVibeFeatureDev_ReviewBlocksThenConverges pins the in-loop adversarial
// review wiring: pass 1 is green + feature_complete, but the review returns
// clean=false (a real invariant defect), so the gate does NOT converge and
// loops back to the campaign; pass 2 the review is clean and it converges.
// Two campaign passes — the review is a genuine convergence gate, not decorative.
func TestVibeFeatureDev_ReviewBlocksThenConverges(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature-dev/main.bot")
	exec := newScenarioExecutor()
	st := &featureDevState{completeBy: 1} // campaign claims complete every pass
	stubFeatureDevCampaign(exec, st)
	// Override the review stub: dirty on pass 1, clean on pass 2.
	var reviewCalls int
	exec.on("review", func(_ map[string]any) (map[string]any, error) {
		reviewCalls++
		if reviewCalls == 1 {
			return map[string]any{"clean": false, "findings": "handler X skips the tenant gate its sibling has", "_tokens": 1}, nil
		}
		return map[string]any{"clean": true, "findings": "", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fd-review", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-fd-review")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (review blocks pass 1, clean pass 2)", got)
	}
	if reviewCalls != 2 {
		t.Errorf("review called %d times, want 2", reviewCalls)
	}
}

// TestVibeFeatureDev_ContinuesUntilComplete is the canonical v2 flow: the
// campaign reports feature_complete=false on pass 1 (work remains) then true
// on pass 2, the deterministic gate is green both times, and the continuation
// loop runs a second campaign pass before converging.
func TestVibeFeatureDev_ContinuesUntilComplete(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature-dev/main.bot")
	exec := newScenarioExecutor()
	st := &featureDevState{completeBy: 2}
	stubFeatureDevCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fd-continue", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-fd-continue")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (pass 1 incomplete → continuation → pass 2 complete)", got)
	}
	if got := exec.callCount("verify_run"); got != 2 {
		t.Errorf("verify_run called %d times, want 2 (the deterministic gate runs each pass)", got)
	}
	// Both passes green → the continuation was driven by feature_complete=false,
	// not a red build; the fail_log must have stayed empty.
	for i, fl := range st.failLogsSeen {
		if fl != "" {
			t.Errorf("pass %d saw fail_log %q, want empty (green gate → continuation, not a red-fix)", i+1, fl)
		}
	}
}

// TestVibeFeatureDev_RedVerifyRoutesBackToCampaign pins the tight
// real-feedback loop: the campaign claims feature_complete every pass, but
// the deterministic gate is RED on pass 1 (the campaign broke the build) and
// green on pass 2. converged = green ∧ feature_complete, so the red build
// must route back to campaign WITH the failure log even though the agent
// claimed completion.
func TestVibeFeatureDev_RedVerifyRoutesBackToCampaign(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature-dev/main.bot")
	exec := newScenarioExecutor()
	st := &featureDevState{completeBy: 1} // the agent claims done every pass
	stubFeatureDevCampaign(exec, st)
	verifyCalls := 0
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		verifyCalls++
		if verifyCalls == 1 {
			return map[string]any{
				"passed": false, "skipped": false, "exit_code": 1,
				"log_tail": "stub build failure: undefined symbol NewHandler", "_tokens": 1,
			}, nil
		}
		return map[string]any{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fd-red", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-fd-red")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (a red gate forces a fix pass even though the agent claimed feature_complete)", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "stub build failure") {
		t.Errorf("second campaign pass fail_log = %v, want it to carry the real build failure so the agent fixes what it broke", st.failLogsSeen)
	}
}

// TestVibeFeatureDev_MRPathOnConverge pins the opt-in MR path: with
// open_mr=true a converged run opens the MR/PR (finalize_mr) before
// finishing — the issue-label → PR lineage.
func TestVibeFeatureDev_MRPathOnConverge(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature-dev/main.bot")
	exec := newScenarioExecutor()
	st := &featureDevState{completeBy: 1}
	stubFeatureDevCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	inputs := map[string]any{"open_mr": true}
	if err := eng.Run(context.Background(), "run-fd-mr", inputs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-fd-mr")
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

// TestVibeFeatureDev_Structural pins the v2 IR shape: the campaign entry,
// the adaptive campaign/verify_build agents, the deterministic verify_run
// tool + gate/mr_gate computes, and the ABSENCE of every retired v1 node
// (the plan/act/simplify session chain and the cross-family review/fix/
// commit machinery). Drift here — e.g. reintroducing a blocking upfront
// plan node or a reviewer — breaks the ADR-058 mechanism silently.
func TestVibeFeatureDev_Structural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature-dev/main.bot")

	// Entry is the deterministic workspace precondition (a tool node, no
	// LLM), then the plan-phase gate (ADR-091) — on by default, its off
	// branch (plan_phase=off) being the v2 "start working immediately"
	// shape.
	if wf.Entry != "workspace_probe" {
		t.Errorf("workflow entry = %q, want %q (the deterministic precondition ahead of any LLM node)", wf.Entry, "workspace_probe")
	}
	if _, ok := wf.Nodes["workspace_probe"].(*ir.ToolNode); !ok {
		t.Errorf("workspace_probe is %T, want *ir.ToolNode (deterministic precondition)", wf.Nodes["workspace_probe"])
	}
	if _, ok := wf.Nodes["plan_topology"].(*ir.ComputeNode); !ok {
		t.Errorf("plan_topology is %T, want *ir.ComputeNode (deterministic gate)", wf.Nodes["plan_topology"])
	}
	for _, id := range []string{"campaign", "verify_build", "finalize_mr"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected agent node %q", id)
		}
		if _, ok := node.(*ir.AgentNode); !ok {
			t.Errorf("node %q is %T, want *ir.AgentNode (adaptive)", id, node)
		}
	}
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
	// The retired v1 dev-chain + review machinery must be gone. "plan" is
	// deliberately absent from this list since ADR-091: the cross-model
	// plan phase reintroduced a `plan` node in a DIFFERENT role (peer-
	// reviewed map, bypassed whole when plan_review resolves off), guarded
	// by bots/plan_phase_test.go — not the v1 blocking act-chain member.
	for _, id := range []string{
		"act", "simplify",
		"alt", "reviewer_claude", "reviewer_gpt", "streak_check",
		"fix_claude", "fix_gpt", "prepare_commit", "commit_changes",
	} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("retired v1 node %q is still present — v2 is one campaign agent, not the plan/act/review assembly line", id)
		}
	}
}

// TestVibeFeatureDev_ProbeSeesLoopIteration pins the LOAD-BEARING
// {{loop.continuation_loop.iteration}} edge mapping into verify_probe: the
// probe's reuse decision keys on it ("pass 1 always regenerates"), so a
// mapping that silently resolves to 0 every pass makes verify_build (an LLM
// agent, ~$0.30-0.70) re-run — and rewrite verify.sh — on EVERY pass
// (observed live on the 2026-07-22 treatment runs: probe reason stuck on
// "first pass of this run" across 4 passes).
func TestVibeFeatureDev_ProbeSeesLoopIteration(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature-dev/main.bot")
	exec := newScenarioExecutor()
	st := &featureDevState{completeBy: 3}
	stubFeatureDevCampaign(exec, st)

	var probeIters []int
	exec.on("verify_probe", func(in map[string]any) (map[string]any, error) {
		it := -1
		switch v := in["iteration"].(type) {
		case int:
			it = v
		case int64:
			it = int(v)
		case float64:
			it = int(v)
		case string:
			// A ref that failed to resolve arrives as its raw/empty string —
			// keep -1 so the assertion shows the failure shape.
		}
		probeIters = append(probeIters, it)
		return map[string]any{"fresh": false, "reason": "stub", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fd-iter", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []int{0, 1, 2}
	if len(probeIters) != len(want) {
		t.Fatalf("verify_probe called %d times (%v), want %d", len(probeIters), probeIters, len(want))
	}
	for i, w := range want {
		if probeIters[i] != w {
			t.Fatalf("verify_probe pass %d saw iteration=%d, want %d (full: %v)", i+1, probeIters[i], w, probeIters)
		}
	}
}
