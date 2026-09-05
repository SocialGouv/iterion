package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// endyState models the e2e-coverage (Endy) v2 campaign: ONE adaptive agent
// inventories features into the committed matrix and closes gaps in stride;
// the deterministic gate re-runs the suite AND enforces the matrix contract.
// The stub drives the properties the control flow depends on: the
// campaign's coverage_complete claim and the gate's (passed, matrix_ok,
// uncovered_rows, scoped) verdict — convergence applies the scope rule (a
// whole-app run demands zero uncovered rows; a scoped run trusts
// scope-level completion). `scoped` is the GATE's verdict, not the raw
// var: it is computed there on the trimmed target so a whitespace-only
// target cannot pass for a scope.
type endyState struct {
	// completeBy: the campaign reports coverage_complete=true on/after this
	// pass (1-based).
	completeBy int
	// uncoveredByPass returns the gate's uncovered_rows for a given verify
	// pass (1-based); nil = always 0.
	uncoveredByPass func(pass int) int
	// scoped mirrors what the real gate computes from a non-blank target.
	scoped       bool
	pass         int
	verifyPass   int
	failLogsSeen []string
}

// stubEndyCampaign registers the baseline stubs for a green continuation:
// campaign (the adaptive agent), verify_build (script written) and
// verify_run (suite green + matrix contract satisfied). Individual tests
// override a node afterward (later .on wins).
func stubEndyCampaign(exec *scenarioExecutor, st *endyState) {
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
		remaining := "runtime.await-best-effort still uncovered"
		if complete {
			remaining = ""
		}
		return map[string]any{
			"coverage_complete":       complete,
			"commits_this_pass":       2,
			"coverage_gaps_remaining": remaining,
			"needs_human":             false,
			"human_note":              "",
			"summary":                 "closed feature gaps this pass",
			"_tokens":                 10,
		}, nil
	})
	exec.on("verify_build", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"prepared": true, "summary": "verify.sh written", "_tokens": 1}, nil
	})
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		st.verifyPass++
		uncovered := 0
		if st.uncoveredByPass != nil {
			uncovered = st.uncoveredByPass(st.verifyPass)
		}
		return map[string]any{
			"passed": true, "skipped": false, "matrix_ok": true,
			"matrix_rows": 12, "uncovered_rows": uncovered, "scoped": st.scoped,
			"new_test_code": true, "exit_code": 0, "log_tail": "", "_tokens": 1,
		}, nil
	})
}

// TestE2ECoverage_ContinuesUntilComplete is the canonical flow: pass 1 leaves
// uncovered rows (campaign reports incomplete), pass 2 closes them — the
// continuation loop runs a second campaign pass before converging, with an
// empty fail_log both times (green gates; the loop-back is remaining WORK,
// not a failure).
func TestE2ECoverage_ContinuesUntilComplete(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")
	exec := newScenarioExecutor()
	st := &endyState{
		completeBy: 2,
		uncoveredByPass: func(pass int) int {
			if pass == 1 {
				return 3
			}
			return 0
		},
	}
	stubEndyCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-endy-continue", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-endy-continue")
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
	for i, fl := range st.failLogsSeen {
		if fl != "" {
			t.Errorf("pass %d saw fail_log %q, want empty (green gate → continuation, not a red-fix)", i+1, fl)
		}
	}
}

// TestE2ECoverage_WholeAppRunBlocksOnUncoveredRows pins the completeness
// floor: on a whole-application run (target empty) the campaign CLAIMING
// coverage_complete does not converge the run while the matrix still counts
// uncovered rows — the deterministic count outranks the agent's claim.
func TestE2ECoverage_WholeAppRunBlocksOnUncoveredRows(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")
	exec := newScenarioExecutor()
	st := &endyState{
		completeBy: 1, // the agent claims done every pass
		uncoveredByPass: func(pass int) int {
			if pass == 1 {
				return 2 // ...but the matrix still counts uncovered rows
			}
			return 0
		},
	}
	stubEndyCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-endy-block", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-endy-block")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (uncovered rows must force another pass despite the completeness claim)", got)
	}
}

// TestE2ECoverage_ScopedRunConvergesWithUncoveredRows pins the scope rule's
// other half: a SCOPED run (target non-empty) converges on scope-level
// completion even though out-of-scope matrix rows remain uncovered — they
// are the next run's backlog, not this run's failure.
func TestE2ECoverage_ScopedRunConvergesWithUncoveredRows(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")
	exec := newScenarioExecutor()
	st := &endyState{
		completeBy:      1,
		scoped:          true,                       // the gate saw a real (non-blank) target
		uncoveredByPass: func(int) int { return 7 }, // out-of-scope backlog
	}
	stubEndyCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	inputs := map[string]any{"target": "persistence & resume lifecycle"}
	if err := eng.Run(context.Background(), "run-endy-scoped", inputs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-endy-scoped")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1 (a scoped run converges on scope completion; out-of-scope uncovered rows stay backlog)", got)
	}
}

// TestE2ECoverage_BlankTargetIsNotAScope pins the workflow half of the
// scope rule: the gate — not the raw var — decides whether a run is
// scoped, so a target that is only whitespace behaves like a whole-app
// run and cannot converge while uncovered rows remain. (The trimming
// itself is proven at the gate in bots/e2e_coverage_matrix_gate_test.go.)
func TestE2ECoverage_BlankTargetIsNotAScope(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")
	exec := newScenarioExecutor()
	st := &endyState{
		completeBy: 1,
		scoped:     false, // what the gate computes from a blank target
		uncoveredByPass: func(pass int) int {
			if pass == 1 {
				return 4
			}
			return 0
		},
	}
	stubEndyCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	inputs := map[string]any{"target": "   "}
	if err := eng.Run(context.Background(), "run-endy-blank", inputs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (a whitespace-only target is no scope: uncovered rows must still force a pass)", got)
	}
}

// TestE2ECoverage_MatrixProblemsRouteBackToCampaign pins the claims floor:
// a red matrix contract (e.g. an ORPHAN CLAIM — a covered row citing a test
// that does not resolve) must route back to the campaign WITH the composed
// matrix-problems log, even though the suite itself is green and the agent
// claimed completion.
func TestE2ECoverage_MatrixProblemsRouteBackToCampaign(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")
	exec := newScenarioExecutor()
	st := &endyState{completeBy: 1}
	stubEndyCampaign(exec, st)
	verifyCalls := 0
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		verifyCalls++
		if verifyCalls == 1 {
			return map[string]any{
				"passed": true, "skipped": false, "matrix_ok": false,
				"matrix_rows": 12, "uncovered_rows": 0, "scoped": false, "new_test_code": true,
				"exit_code": 0,
				"log_tail":  "[gate] MATRIX PROBLEMS (1):\nruntime.resume: ORPHAN CLAIM -- none of the cited tests resolve in the tree: TestGhost (e2e/ghost_test.go)",
				"_tokens":   1,
			}, nil
		}
		return map[string]any{
			"passed": true, "skipped": false, "matrix_ok": true,
			"matrix_rows": 12, "uncovered_rows": 0, "scoped": false, "new_test_code": true,
			"exit_code": 0, "log_tail": "", "_tokens": 1,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-endy-orphan", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-endy-orphan")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (a red matrix forces a repair pass despite a green suite + completeness claim)", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "ORPHAN CLAIM") {
		t.Errorf("second campaign pass fail_log = %v, want it to carry the MATRIX PROBLEMS log so the agent repairs the claims", st.failLogsSeen)
	}
}

// TestE2ECoverage_RedSuiteRoutesBackWithFailLog pins the classic verify
// floor shared with the fleet: a red suite routes back to the campaign with
// the real failure log.
func TestE2ECoverage_RedSuiteRoutesBackWithFailLog(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")
	exec := newScenarioExecutor()
	st := &endyState{completeBy: 1}
	stubEndyCampaign(exec, st)
	verifyCalls := 0
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		verifyCalls++
		if verifyCalls == 1 {
			return map[string]any{
				"passed": false, "skipped": false, "matrix_ok": true,
				"matrix_rows": 12, "uncovered_rows": 0, "scoped": false, "new_test_code": true,
				"exit_code": 1, "log_tail": "stub suite failure: TestResume red", "_tokens": 1,
			}, nil
		}
		return map[string]any{
			"passed": true, "skipped": false, "matrix_ok": true,
			"matrix_rows": 12, "uncovered_rows": 0, "scoped": false, "new_test_code": true,
			"exit_code": 0, "log_tail": "", "_tokens": 1,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-endy-red", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-endy-red")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (a red suite forces a fix pass)", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "stub suite failure") {
		t.Errorf("second campaign pass fail_log = %v, want the real suite failure", st.failLogsSeen)
	}
}

// TestE2ECoverage_EventTrace establishes the event-coherence baseline: a
// happy-path run persists node lifecycle events for the core nodes.
func TestE2ECoverage_EventTrace(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")
	exec := newScenarioExecutor()
	st := &endyState{completeBy: 1}
	stubEndyCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-endy-events", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), "run-endy-events")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if !hasEvent(events, store.EventRunStarted) {
		t.Errorf("missing run_started event")
	}
	if !hasEvent(events, store.EventRunFinished) {
		t.Errorf("missing run_finished event")
	}
	finishedIDs := eventNodeIDs(events, store.EventNodeFinished)
	finishedSet := make(map[string]bool, len(finishedIDs))
	for _, id := range finishedIDs {
		finishedSet[id] = true
	}
	for _, want := range []string{"campaign", "verify_build", "verify_run", "gate"} {
		if !finishedSet[want] {
			t.Errorf("expected node_finished event for %q, got %v", want, finishedIDs)
		}
	}
}

// TestE2ECoverage_Structural pins the v2 IR shape: the deterministic
// workspace precondition as entry, then the plan-phase gate (ADR-091 — on
// by default; plan_phase=off routes straight to campaign, the inventory as
// its own first move), the two adaptive agents, the deterministic
// verify_run tool + gate compute, and the single bounded continuation loop.
func TestE2ECoverage_Structural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "e2e-coverage/main.bot")

	if wf.Entry != "workspace_probe" {
		t.Errorf("workflow entry = %q, want %q (the deterministic precondition ahead of any LLM node)", wf.Entry, "workspace_probe")
	}
	if _, ok := wf.Nodes["workspace_probe"].(*ir.ToolNode); !ok {
		t.Errorf("workspace_probe is %T, want *ir.ToolNode (deterministic precondition)", wf.Nodes["workspace_probe"])
	}
	if _, ok := wf.Nodes["plan_topology"].(*ir.ComputeNode); !ok {
		t.Errorf("plan_topology is %T, want *ir.ComputeNode (deterministic gate)", wf.Nodes["plan_topology"])
	}
	for _, id := range []string{"campaign", "verify_build"} {
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
	if node, ok := wf.Nodes["gate"]; !ok {
		t.Fatalf("workflow missing expected compute node %q", "gate")
	} else if _, ok := node.(*ir.ComputeNode); !ok {
		t.Errorf("node %q is %T, want *ir.ComputeNode (deterministic)", "gate", node)
	}
	if len(wf.Loops) != 1 {
		t.Errorf("declared loops = %d, want exactly 1 (the bounded continuation loop)", len(wf.Loops))
	}
}
