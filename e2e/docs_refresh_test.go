package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// docsRefreshState models docs-refresh's v3 shape (the Billy/Willy
// paradigm: one capable agent + a mission + TRUTH gates only). A single
// deterministic scan_hints node hands the campaign an ADVISORY report
// (hints, telemetry — never obligations); the campaign aligns docs
// committing in stride; the deterministic TRUTH gates re-check (scope
// containment, real build) and the continuation loop re-hints until
// docs_aligned ∧ scope_ok ∧ passed — nothing else.
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
			"doc_files": []any{"README.md"}, "doc_count": 1, "no_docs": false,
			"noop_skip": false, "noop_reason": "HEAD advanced since last alignment",
			"git_head": "abc123",
			"hints": []any{
				map[string]any{"doc": "README.md", "line": 3, "kind": "missing_path", "value": "pkg/gone.go", "note": "path cited in the doc not found on disk (pkg/ exists)"},
			},
			"hint_count": st.hintCount, "missing_path_count": 1, "dead_link_count": 0,
			"unmentioned_area_count": 0, "checked_paths": 1, "checked_links": 0,
			"ledger_excluded": 0, "truncated": false,
			"hints_note":                  "1 missing path(s)",
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
	// verify_precheck: the stub never reuses, so every pass walks the full
	// verify chain.
	exec.on("verify_precheck", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"reuse": false, "reason": "stub: always verify", "_tokens": 1}, nil
	})
	exec.on("verify_build", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"prepared": true, "summary": "verify.sh written", "_tokens": 1}, nil
	})
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})
	exec.on("mark_issue_for_review", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"text": "{\"skipped\":\"no issue_id\"}", "_tokens": 1}, nil
	})
	exec.on("update_audit_cache", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"cache_path": "", "git_head": "", "skipped": "no cache_path", "_tokens": 1}, nil
	})
	exec.on("author_docs", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"docs_created": true, "created_doc_files": []any{"docs/overview.md"},
			"summary": "authored initial docs", "_tokens": 5,
		}, nil
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

// TestDocsRefresh_ConvergesFirstPass: aligned + green truth gates on
// pass 1 → one campaign pass, noop cache refreshed, done.
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
	if !exec.wasCalled("update_audit_cache") {
		t.Errorf("update_audit_cache never fired — the noop cache must refresh on convergence")
	}
	if exec.wasCalled("author_docs") {
		t.Errorf("author_docs fired although docs exist (no_docs=false)")
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
// agent ended up serving the scanner; v3's gate is verify ∧ scope ∧
// docs_aligned — nothing else. If someone re-introduces a scanner count
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

// TestDocsRefresh_NoopSkipsCampaign: cache HEAD matches, clean tree, no
// explicit request → the whole campaign is skipped with zero LLM cost.
func TestDocsRefresh_NoopSkipsCampaign(t *testing.T) {
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 0}
	stubDocsRefresh(exec, st)
	exec.on("scan_hints", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"doc_files": []any{"README.md"}, "doc_count": 1, "no_docs": false,
			"noop_skip": true, "noop_reason": "docs already aligned to HEAD abc123",
			"git_head": "abc123", "hints": []any{}, "hint_count": 0,
			"missing_path_count": 0, "dead_link_count": 0, "unmentioned_area_count": 0,
			"checked_paths": 0, "checked_links": 0, "ledger_excluded": 0,
			"truncated": false, "hints_note": "noop",
			"recently_changed_code_files": []any{}, "_tokens": 1,
		}, nil
	})

	run := runDocsRefresh(t, exec, "run-dr-noop")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.wasCalled("campaign") {
		t.Errorf("campaign fired on a noop_skip run — the short-circuit must cost zero LLM")
	}
	if exec.wasCalled("verify_build") {
		t.Errorf("verify_build fired on a noop_skip run")
	}
}

// TestDocsRefresh_DefaultCreateAuthorsThenRefreshes: a repo with zero
// docs routes through author_docs, rescans, then runs the normal
// campaign (v0.3.0 DEFAULT-CREATE, preserved in v3).
func TestDocsRefresh_DefaultCreateAuthorsThenRefreshes(t *testing.T) {
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, hintCount: 0}
	stubDocsRefresh(exec, st)
	scans := 0
	exec.on("scan_hints", func(_ map[string]any) (map[string]any, error) {
		scans++
		noDocs := scans == 1
		count := 0
		files := []any{}
		note := "no docs in scope"
		if !noDocs {
			count = 1
			files = []any{"docs/overview.md"}
			note = "0 missing path(s)"
		}
		return map[string]any{
			"doc_files": files, "doc_count": count, "no_docs": noDocs,
			"noop_skip": false, "noop_reason": "no docs in scope",
			"git_head": "abc123", "hints": []any{}, "hint_count": 0,
			"missing_path_count": 0, "dead_link_count": 0, "unmentioned_area_count": 0,
			"checked_paths": 0, "checked_links": 0, "ledger_excluded": 0,
			"truncated": false, "hints_note": note,
			"recently_changed_code_files": []any{}, "_tokens": 1,
		}, nil
	})

	run := runDocsRefresh(t, exec, "run-dr-create")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if !exec.wasCalled("author_docs") {
		t.Errorf("author_docs did not fire on a zero-doc repo (DEFAULT-CREATE)")
	}
	if scans != 2 {
		t.Errorf("scan_hints ran %d times, want 2 (author_rescan re-scan)", scans)
	}
	if !exec.wasCalled("campaign") {
		t.Errorf("campaign never ran after the authoring rescan")
	}
}

// TestDocsRefresh_Structural pins the v3 IR shape: one advisory hints
// producer + one campaign + truth gates, and the ABSENCE of the retired
// v1 review machinery AND the retired v2 obligation machinery
// (scan_docs/scan_code_surface/build_manifest).
func TestDocsRefresh_Structural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "docs-refresh/main.bot")

	if wf.Entry != "scan_hints" {
		t.Errorf("workflow entry = %q, want %q (the advisory scan leads)", wf.Entry, "scan_hints")
	}
	for _, id := range []string{"campaign", "verify_build", "author_docs"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected agent node %q", id)
		}
		if _, ok := node.(*ir.AgentNode); !ok {
			t.Errorf("node %q is %T, want *ir.AgentNode (adaptive)", id, node)
		}
	}
	for _, id := range []string{"scan_hints", "scope_check", "verify_precheck", "verify_run", "mark_issue_for_review", "update_audit_cache"} {
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
	} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("retired node %q is still present — v3 is one campaign guided by an advisory scan + truth gates, not a scanner pipeline", id)
		}
	}
}
