package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// docsRefreshState models docs-refresh's shape: one capable agent + a
// mission + a TRUTH gate only. A single deterministic scan_hints node
// hands the campaign an ADVISORY report (hints, telemetry — never
// obligations); the campaign aligns docs committing in stride; the
// deterministic scope gate re-checks writeable-set containment and the
// continuation loop re-hints until docs_aligned ∧ scope_ok — nothing
// else. A docs-only bot cannot break the build, so there is no build
// gate; the dispatcher owns the tracker-issue lifecycle natively, so
// there is no mark-issue node; the campaign authors from zero itself,
// so there is no author_docs bootstrap branch.
type docsRefreshState struct {
	alignedBy    int // campaign reports docs_aligned=true on/after this pass
	hintCount    int // hint_count reported by every scan_hints pass (advisory)
	pass         int
	failLogsSeen []string
}

// stubDocsRefresh registers the baseline green-path stubs. Individual
// tests override nodes afterward (later .on wins).
func stubDocsRefresh(exec *scenarioExecutor, st *docsRefreshState) {
	// The entry: the run base, recorded before any pass can write. The
	// delivery gate is measured on it, and an empty one refuses — so the
	// green path records a sha here.
	exec.on("record_run_base", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"base_sha": "0123456789abcdef0123456789abcdef01234567", "_tokens": 1}, nil
	})
	exec.on("scan_hints", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"doc_files": []any{"README.md"}, "doc_count": 1,
			"hints": []any{
				map[string]any{"doc": "README.md", "line": 3, "kind": "missing_path", "value": "pkg/gone.go", "note": "path cited in the doc not found on disk (pkg/ exists)"},
			},
			"hint_count": st.hintCount, "missing_path_count": 1, "dead_link_count": 0,
			"unmentioned_area_count": 0, "checked_paths": 1, "checked_links": 0,
			"ledger_excluded": 0, "truncated": false,
			"hints_note":                  "1 missing path(s)",
			"mode":                        "full",
			"incremental_base":            "",
			"recently_changed_code_files": []any{},
			"_tokens":                     1,
		}, nil
	})
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		st.pass++
		fl := ""
		if raw, ok := in["fail_log"]; ok {
			fl = strings.TrimSpace(toStr(raw))
		}
		st.failLogsSeen = append(st.failLogsSeen, fl)
		aligned := st.pass >= st.alignedBy
		remaining := "README.md still cites gone.go"
		if aligned {
			remaining = ""
		}
		return map[string]any{
			"docs_aligned": aligned, "commits_this_pass": 1, "drift_remaining": remaining,
			"is_code_bug": false, "needs_human": false, "human_note": "",
			"summary": "aligned docs this pass", "_tokens": 10,
		}, nil
	})
	exec.on("scope_check", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"scope_ok": true, "out_of_scope": []any{}, "log": "", "_tokens": 1}, nil
	})
	// available=true keeps the opt-in PR path reachable; the probe only runs
	// behind the open_mr gate, so the scenarios that leave open_mr at its
	// default never reach it.
	exec.on("forge_auth_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"available": true, "reason": "env:GH_TOKEN", "_tokens": 1}, nil
	})
	// The deterministic delivery bound on the only in-edge of finalize_mr.
	// It is stubbed on the green path because the edge is FAIL-CLOSED: an
	// output carrying no `delivery_ok` refuses, which is the behaviour the
	// refusal witness relies on and the reason an unstubbed node would take
	// every PR-tail scenario to the typed failure.
	exec.on("delivery_gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"delivery_ok": true, "out_of_scope": []any{}, "log": "", "_tokens": 1}, nil
	})
	// open_mr defaults false, so only the scenarios that ask for the tail
	// reach this one — and they must reach a node that ANSWERS: an unstubbed
	// agent returns no `opened`, and the run then dies of NO_OUTGOING_EDGE,
	// which would make every delivery assertion below read a graph error
	// instead of the behaviour it is about.
	exec.on("finalize_mr", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"opened": true, "url": "https://forge/pr/11", "branch": "iterion/docs-refresh/demo",
			"back_linked": false, "skipped_reason": "", "summary": "PR opened", "_tokens": 5,
		}, nil
	})
}

func runDocsRefresh(t *testing.T, exec *scenarioExecutor, runID string) *store.Run {
	t.Helper()
	run, err := runDocsRefreshWith(t, exec, runID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return run
}

// runDocsRefreshWith drives the bot with the run's inputs and RETURNS the
// engine's error instead of ending the test on it: a typed refusal is a
// failed run, which is exactly what the delivery witness asserts.
func runDocsRefreshWith(t *testing.T, exec *scenarioExecutor, runID string, inputs map[string]any) (*store.Run, error) {
	t.Helper()
	wf := compileFixtureStubSafe(t, "docs-refresh/main.bot")
	s := tmpStore(t)
	eng := newEngine(t, wf, s, exec)
	runErr := eng.Run(context.Background(), runID, inputs)
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return run, runErr
}

// TestDocsRefresh_ConvergesFirstPass: aligned + green scope gate on
// pass 1 → one campaign pass, straight to the PR tail, done.
func TestDocsRefresh_ConvergesFirstPass(t *testing.T) {
	t.Parallel()
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 1}
	stubDocsRefresh(exec, st)

	run := runDocsRefresh(t, exec, "run-dr-first")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1", got)
	}
}

// TestDocsRefresh_ContinuesUntilAligned: work remains after pass 1 →
// the loop re-hints (fresh advisory report) and runs a second campaign
// pass before converging.
func TestDocsRefresh_ContinuesUntilAligned(t *testing.T) {
	t.Parallel()
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 2, hintCount: 1}
	stubDocsRefresh(exec, st)

	run := runDocsRefresh(t, exec, "run-dr-continue")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2", got)
	}
	if got := exec.callCount("scan_hints"); got != 2 {
		t.Errorf("scan_hints called %d times, want 2 (the loop head re-hints each pass)", got)
	}
}

// TestDocsRefresh_ScopeViolationRoutesBack: the campaign touched a
// non-doc file on pass 1 — the deterministic scope gate fails the pass
// and the campaign's second pass receives the violation in fail_log.
func TestDocsRefresh_ScopeViolationRoutesBack(t *testing.T) {
	t.Parallel()
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 1} // claims aligned every pass
	stubDocsRefresh(exec, st)
	scopeCalls := 0
	exec.on("scope_check", func(_ map[string]any) (map[string]any, error) {
		scopeCalls++
		if scopeCalls == 1 {
			return map[string]any{
				"scope_ok": false, "out_of_scope": []any{"pkg/foo/bar.go"},
				"log": "SCOPE VIOLATION: this run changed files outside the doc writeable-set: pkg/foo/bar.go", "_tokens": 1,
			}, nil
		}
		return map[string]any{"scope_ok": true, "out_of_scope": []any{}, "log": "", "_tokens": 1}, nil
	})

	run := runDocsRefresh(t, exec, "run-dr-scope")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (scope violation forces a revert pass)", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "SCOPE VIOLATION") {
		t.Errorf("second campaign pass fail_log = %v, want the scope violation so the agent reverts it", st.failLogsSeen)
	}
}

// TestDocsRefresh_ExhaustedLoopNeverDeliversAScopeViolation: the campaign
// claims the docs aligned and never reverts its out-of-scope write, so every
// pass is red on the writeable-set gate and the continuation loop runs out.
// The loop-exhaustion exit then hands the banked series to the PR tail — and
// the deterministic delivery gate refuses it there, typed, naming the path.
// Before the gate existed this run PUSHED: a `.github/workflows/**` path
// died at the forge as a raw 403, and anything else was delivered.
//
// Mutation that reddens it: delete `delivery_gate -> delivery_paths_refused`
// from bots/docs-refresh/main.bot and let the gate's output be ignored (or
// put back `forge_auth_probe -> finalize_mr when available`) — finalize_mr
// fires and the run finishes.
func TestDocsRefresh_ExhaustedLoopNeverDeliversAScopeViolation(t *testing.T) {
	t.Parallel()
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 1} // claims aligned every pass
	stubDocsRefresh(exec, st)
	const offender = "pkg/foo/bar.go"
	exec.on("scope_check", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"scope_ok": false, "out_of_scope": []any{offender},
			"log": "SCOPE VIOLATION: this run changed files outside the doc writeable-set: " + offender, "_tokens": 1,
		}, nil
	})
	// What the real node answers about that tree: the path is not a doc, so
	// the delivery does not own it (bots/delivery_gate_test.go runs the
	// program itself against a git fixture).
	exec.on("delivery_gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"delivery_ok": false, "out_of_scope": []any{offender},
			"log": "DELIVERY PATHS REFUSED: the delivery would carry 1 path(s) outside the doc writeable-set (.md anywhere, never under .github/workflows/, never a path the run committed under .claude/): " + offender, "_tokens": 1,
		}, nil
	})

	run, runErr := runDocsRefreshWith(t, exec, "run-dr-delivery-refused", map[string]any{"max_passes": 1, "open_mr": true})
	if runErr == nil {
		t.Fatalf("Run returned no error — a typed refusal ends the run failed, and this one delivered instead (status %s)", run.Status)
	}
	if run.Status != store.RunStatusFailed {
		t.Errorf("status = %s, want %s — the refusal is terminal: nothing about this run changes by resuming it", run.Status, store.RunStatusFailed)
	}
	if run.FailureCode != "DELIVERY_PATHS_REFUSED" {
		t.Errorf("failure_code = %q, want DELIVERY_PATHS_REFUSED — an untyped refusal is indistinguishable from every other fail node in the fleet (error: %s)", run.FailureCode, run.Error)
	}
	if !strings.Contains(run.Error, offender) {
		t.Errorf("error = %q, want it to name %q: the paths to revert are what the operator reads", run.Error, offender)
	}
	// The message must say where the commits ARE — an operator told "nothing
	// was pushed" never goes looking for them, and for a
	// `.github/workflows/**` offender the raw 403 this gate exists to replace
	// then arrives on that push instead. It must also say so TRUE in every
	// deployment: `iterion/run-<run id>` is built in ONE place, the cloud
	// runner's bank for a repo-targeted run (pkg/runner/loop_gitws.go, which
	// serves queued runs), so a local `iterion run` has no such branch and an
	// unqualified claim sends that operator looking for a ref nothing wrote.
	// The commits are on the run's own branch in both.
	if strings.Contains(run.Error, "nothing was pushed") {
		t.Errorf("error = %q claims nothing was pushed — the commits are on the run's own branch, so the claim is false and sends the operator looking in the wrong place", run.Error)
	}
	if !strings.Contains(run.Error, "storage branch") {
		t.Errorf("error = %q, want it to name where the commits ARE (the run's own branch, banked to its storage branch on a cloud run): reading or cherry-picking them is the operator's next act", run.Error)
	}
	if !strings.Contains(run.Error, "cloud run") {
		t.Errorf("error = %q names a storage branch with no deployment qualifier — only the cloud runner of a repo-targeted run banks one (pkg/runner/loop_gitws.go), so an unqualified sentence is false for every local run", run.Error)
	}
	if exec.wasCalled("finalize_mr") {
		t.Errorf("finalize_mr fired on a loop-exhausted scope violation — the banked commits must stay on the run's branch, un-delivered")
	}
	if exec.wasCalled("surface_pr_link") {
		t.Errorf("surface_pr_link fired on a refused delivery — nothing was opened, so there is no link to surface")
	}
	// N+1: the cap counts BACK-EDGE crossings, so max_passes=1 permits two
	// executions of the loop body — then the exhaustion exit fires.
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (max_passes=1 permits one crossing, hence two passes; the loop is then spent)", got)
	}
}

// TestDocsRefresh_HintsAreAdvisoryNeverGate (v3, the paradigm pin): a
// large residual hint count must NOT block convergence. The v2 lineage
// gated on scanner metrics (coverage_pct, undocumented_count) and the
// agent ended up serving the scanner; the gate is scope ∧ docs_aligned
// — nothing else. If someone re-introduces a scanner count
// into the gate expression, this test fails.
func TestDocsRefresh_HintsAreAdvisoryNeverGate(t *testing.T) {
	t.Parallel()
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 97}
	stubDocsRefresh(exec, st)

	run := runDocsRefresh(t, exec, "run-dr-advisory")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1 — 97 residual advisory hints must not block convergence (hints are telemetry, not a gate)", got)
	}
}

// TestDocsRefresh_ZeroDocsRoutesToCampaign: a repo with no docs in scope
// is NOT special-cased — scan_hints hands straight to the campaign, which
// authors the initial set itself. v3.4 dropped the author_docs bootstrap
// branch + author_rescan loop; the one adaptive agent covers it, exactly
// as a native session handed "align the docs" would.
func TestDocsRefresh_ZeroDocsRoutesToCampaign(t *testing.T) {
	t.Parallel()
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 0}
	stubDocsRefresh(exec, st)
	exec.on("scan_hints", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"doc_files": []any{}, "doc_count": 0,
			"hints": []any{}, "hint_count": 0,
			"missing_path_count": 0, "dead_link_count": 0, "unmentioned_area_count": 0,
			"checked_paths": 0, "checked_links": 0, "ledger_excluded": 0,
			"truncated": false, "hints_note": "no docs in scope",
			"mode": "full", "incremental_base": "",
			"recently_changed_code_files": []any{}, "_tokens": 1,
		}, nil
	})

	run := runDocsRefresh(t, exec, "run-dr-zerodocs")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if !exec.wasCalled("campaign") {
		t.Errorf("campaign never ran on a zero-doc repo — the campaign must author the initial set itself")
	}
}

// TestDocsRefresh_ModeWiredToCampaign: scan_hints' mode + incremental_base
// reach the campaign input via the edge mapping, so the campaign can scope
// its pass (full sweep vs delta-since-last-alignment). Guards the
// incremental-mode wiring.
func TestDocsRefresh_ModeWiredToCampaign(t *testing.T) {
	t.Parallel()
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 1}
	stubDocsRefresh(exec, st)
	var gotMode any
	var haveMode, haveBase bool
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		gotMode, haveMode = in["mode"]
		_, haveBase = in["incremental_base"]
		return map[string]any{
			"docs_aligned": true, "commits_this_pass": 0, "drift_remaining": "",
			"is_code_bug": false, "needs_human": false, "human_note": "",
			"summary": "already aligned", "_tokens": 1,
		}, nil
	})

	run := runDocsRefresh(t, exec, "run-dr-mode-wired")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if !haveMode || toStr(gotMode) != "full" {
		t.Errorf("campaign input mode = %v (present=%v), want \"full\" — the scan_hints→campaign edge must carry mode", gotMode, haveMode)
	}
	if !haveBase {
		t.Errorf("campaign input missing incremental_base — the edge must carry it (empty in full mode is fine)")
	}
}

// TestDocsRefresh_Structural pins the lean IR shape: one advisory hints
// producer + one campaign + a truth gate + the opt-in PR tail, and the
// ABSENCE of the retired review/obligation/verify machinery and the
// v3.4-removed noop cache / author_docs / mark-issue nodes.
func TestDocsRefresh_Structural(t *testing.T) {
	t.Parallel()
	wf := compileFixtureStubSafe(t, "docs-refresh/main.bot")

	if wf.Entry != "record_run_base" {
		t.Errorf("workflow entry = %q, want %q — the RUN BASE is read before anything can write, because the delivery gate cannot re-derive it from the campaign's own commit messages afterwards", wf.Entry, "record_run_base")
	}
	// the ONE adaptive agent
	if node, ok := wf.Nodes["campaign"]; !ok {
		t.Fatalf("workflow missing expected agent node %q", "campaign")
	} else if _, ok := node.(*ir.AgentNode); !ok {
		t.Errorf("node campaign is %T, want *ir.AgentNode (adaptive)", node)
	}
	// the context front door: one compute gate + one deterministic ingest
	if node, ok := wf.Nodes["context_gate"]; !ok {
		t.Fatalf("workflow missing compute node context_gate")
	} else if _, ok := node.(*ir.ComputeNode); !ok {
		t.Errorf("node context_gate is %T, want *ir.ComputeNode", node)
	}
	for _, id := range []string{"record_run_base", "context_ingest", "scan_hints", "scope_check", "forge_auth_probe", "delivery_gate", "surface_pr_link"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected tool node %q", id)
		}
		if _, ok := node.(*ir.ToolNode); !ok {
			t.Errorf("node %q is %T, want *ir.ToolNode (deterministic)", id, node)
		}
	}
	if node, ok := wf.Nodes["gate"]; !ok {
		t.Errorf("workflow missing compute node gate")
	} else if _, ok := node.(*ir.ComputeNode); !ok {
		t.Errorf("node gate is %T, want *ir.ComputeNode", node)
	}
	// v3.7 coverage audit — the goal-as-data loop: the deterministic
	// inventory feeds a READ-ONLY second-model auditor; gaps loop back
	// through the campaign (bounded); the no-context path is unchanged.
	inv, ok := wf.Nodes["coverage_inventory"]
	if !ok {
		t.Fatalf("workflow missing compute node coverage_inventory")
	} else if _, ok := inv.(*ir.ToolNode); !ok {
		t.Errorf("node coverage_inventory is %T, want *ir.ToolNode (deterministic goal extractor)", inv)
	}
	if audit, ok := wf.Nodes["coverage_audit"]; !ok {
		t.Fatalf("workflow missing agent node coverage_audit")
	} else if an, ok := audit.(*ir.AgentNode); !ok {
		t.Errorf("node coverage_audit is %T, want *ir.AgentNode (the second-model validator)", audit)
	} else {
		for _, tool := range an.Tools {
			switch strings.ToLower(tool) {
			case "bash", "edit", "write", "multiedit", "notebookedit":
				t.Errorf("the coverage auditor carries write-capable tool %q — the audit must never modify the docs it judges", tool)
			}
		}
	}
	for _, w := range []struct{ from, to, cond string }{
		// the base is recorded at the entry, once, before the loop
		{"record_run_base", "context_gate", ""},
		// the gate lifts the compound route decision into single bools
		// (C011 simple form): coverage runs only with reference context.
		{"gate", "coverage_inventory", "converged_with_context"},
		{"gate", "mr_gate", "converged_without_context"},
		{"coverage_inventory", "coverage_audit", "audit_enabled"},
		{"coverage_audit", "coverage_route", ""},
		{"coverage_route", "scan_hints", "needs_remediation"},
		{"coverage_route", "mr_gate", ""},
		// The delivery bound sits on the ONLY in-edge of finalize_mr, so
		// every route into the tail — converged, converged_without_context,
		// the coverage route, the spent loop — crosses its verdict, and the
		// bare exit is the refusal (bots/forge_delivery_class_test.go proves
		// the dominance mechanically).
		{"forge_auth_probe", "delivery_gate", "available"},
		{"delivery_gate", "finalize_mr", "delivery_ok"},
		{"delivery_gate", "delivery_paths_refused", ""},
	} {
		found := false
		for _, e := range wf.Edges {
			if e.From == w.from && e.To == w.to && !e.Negated &&
				((w.cond == "" && e.Condition == "") || e.Condition == w.cond) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing edge %s -> %s (when %q): the goal loop is not wired", w.from, w.to, w.cond)
		}
	}
	if l, ok := wf.Loops["coverage_loop"]; !ok {
		t.Error("missing bounded loop coverage_loop — an unbounded remediation can spin the graph")
	} else if l.MaxIterations != 2 {
		t.Errorf("coverage_loop max = %d, want 2", l.MaxIterations)
	}
	for _, id := range []string{
		// v1 review assembly line
		"alt", "reviewer_claude", "reviewer_gpt", "streak_check",
		"fix_claude", "fix_gpt", "prepare_commit", "commit_changes",
		"enforce_fix_scope", "detect_doc_changes",
		// v2 obligation machinery (scanner-as-obligation-generator)
		"scan_docs", "scan_code_surface", "build_manifest",
		// v3.3 removed the build-verify apparatus (a docs bot cannot
		// break the build): campaign + scope gate are the whole oracle.
		"verify_build", "verify_run", "verify_precheck",
		// v3.4 removed: the noop cache, the author_docs bootstrap branch
		// (the campaign authors from zero itself), and the mark-issue node
		// (the dispatcher owns the tracker-issue lifecycle natively).
		"author_docs", "update_audit_cache", "mark_issue_for_review",
	} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("retired node %q is still present — the lean bot is one campaign guided by an advisory scan + truth gate + PR tail, not a scanner pipeline", id)
		}
	}
}
