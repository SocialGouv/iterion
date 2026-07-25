package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
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
}

func runDocsRefresh(t *testing.T, exec *scenarioExecutor, runID string) *store.Run {
	t.Helper()
	wf := compileFixtureStubSafe(t, "docs-refresh/main.bot")
	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return run
}

// TestDocsRefresh_ConvergesFirstPass: aligned + green scope gate on
// pass 1 → one campaign pass, straight to the PR tail, done.
func TestDocsRefresh_ConvergesFirstPass(t *testing.T) {
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

// TestDocsRefresh_HintsAreAdvisoryNeverGate (v3, the paradigm pin): a
// large residual hint count must NOT block convergence. The v2 lineage
// gated on scanner metrics (coverage_pct, undocumented_count) and the
// agent ended up serving the scanner; the gate is scope ∧ docs_aligned
// — nothing else. If someone re-introduces a scanner count
// into the gate expression, this test fails.
func TestDocsRefresh_HintsAreAdvisoryNeverGate(t *testing.T) {
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
	wf := compileFixtureStubSafe(t, "docs-refresh/main.bot")

	if wf.Entry != "scan_hints" {
		t.Errorf("workflow entry = %q, want %q (the advisory scan leads)", wf.Entry, "scan_hints")
	}
	// the ONE adaptive agent
	if node, ok := wf.Nodes["campaign"]; !ok {
		t.Fatalf("workflow missing expected agent node %q", "campaign")
	} else if _, ok := node.(*ir.AgentNode); !ok {
		t.Errorf("node campaign is %T, want *ir.AgentNode (adaptive)", node)
	}
	for _, id := range []string{"scan_hints", "scope_check", "forge_auth_probe", "surface_pr_link"} {
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
