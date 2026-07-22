package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// docsRefreshState models docs-refresh's v2 shape (ADR-058): the
// deterministic audit machinery (scan_docs → scan_code_surface →
// build_manifest) hands ONE adaptive `campaign` agent a bounded drift
// manifest; the campaign aligns docs committing in stride; then the
// deterministic gates re-check (scope containment, build, manifest
// coverage) and the continuation loop re-manifests until
// docs_aligned ∧ scope_ok ∧ passed ∧ coverage ≥ target.
type docsRefreshState struct {
	alignedBy    int // campaign reports docs_aligned=true on/after this pass
	coverageBy   []int
	pass         int
	failLogsSeen []string
}

func (st *docsRefreshState) coverage() int {
	if len(st.coverageBy) == 0 {
		return 91
	}
	i := st.pass
	if i >= len(st.coverageBy) {
		i = len(st.coverageBy) - 1
	}
	return st.coverageBy[i]
}

// stubDocsRefresh registers the baseline green-path stubs. Individual
// tests override nodes afterward (later .on wins).
func stubDocsRefresh(exec *scenarioExecutor, st *docsRefreshState) {
	exec.on("scan_docs", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"doc_files": []any{"README.md"}, "doc_count": 1, "no_docs": false,
			"footprint_hash": "h", "scope_globs": []any{"README.md"},
			"recently_changed_code_files": []any{}, "bundle_self_path": "",
			"pre_verified_docs": []any{}, "cache_hits": 0, "cache_misses": 1,
			"_tokens": 1,
		}, nil
	})
	exec.on("scan_code_surface", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"cli_commands": []any{}, "cli_flags": []any{},
			"diagnostic_codes": []any{}, "scan_skipped": "no globs", "_tokens": 1,
		}, nil
	})
	exec.on("build_manifest", func(_ map[string]any) (map[string]any, error) {
		cov := st.coverage()
		return map[string]any{
			"total_docs": 1, "total_anchors": 10, "verified_anchors": cov / 10,
			"drifted_anchors": 1, "unverifiable_anchors": 0, "checkable_anchors": 10,
			"coverage_pct": cov,
			"drift_candidates": []any{
				map[string]any{"doc": "README.md", "line": 3, "kind": "file_ref", "value": "gone.go", "status": "drifted", "evidence": "missing", "excerpt": "see gone.go"},
			},
			"per_doc_anchor_counts": []any{}, "all_audited_docs": []any{"README.md"},
			"docs_with_drift_count": 1, "chunked": false, "chunk_doc_count": 1,
			"max_review_chunk_docs": 30,
			"verified_pairs":        []any{"README.md::cmd/app"},
			"_tokens":               1,
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
	// verify_precheck (2.4.0): the stub never reuses, so every pass walks
	// the full verify chain exactly as the pre-2.4.0 scenarios did.
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
		return map[string]any{"cache_path": "", "entries_written": 0, "skipped": "no cache_path", "_tokens": 1}, nil
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

// TestDocsRefresh_ConvergesFirstPass: aligned + green gates + coverage
// over target on pass 1 → one campaign pass, cache refreshed, done.
func TestDocsRefresh_ConvergesFirstPass(t *testing.T) {
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1}
	stubDocsRefresh(exec, st)

	run := runDocsRefresh(t, exec, "run-dr-first")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1", got)
	}
	if !exec.wasCalled("update_audit_cache") {
		t.Errorf("update_audit_cache never fired — the inter-run cache must refresh on convergence")
	}
	if exec.wasCalled("author_docs") {
		t.Errorf("author_docs fired although docs exist (no_docs=false)")
	}
}

// TestDocsRefresh_ContinuesUntilAligned: drift remains after pass 1 →
// the loop re-manifests (fresh drift set) and runs a second campaign
// pass before converging.
func TestDocsRefresh_ContinuesUntilAligned(t *testing.T) {
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 2}
	stubDocsRefresh(exec, st)

	run := runDocsRefresh(t, exec, "run-dr-continue")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2", got)
	}
	if got := exec.callCount("build_manifest"); got != 2 {
		t.Errorf("build_manifest called %d times, want 2 (the loop head re-manifests each pass)", got)
	}
}

// TestDocsRefresh_ScopeViolationRoutesBack: the campaign touched a
// non-doc file on pass 1 — the deterministic scope gate fails the pass
// and the campaign's second pass receives the violation in fail_log.
func TestDocsRefresh_ScopeViolationRoutesBack(t *testing.T) {
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1} // claims aligned every pass
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

// TestDocsRefresh_CoverageGateBlocksEarlyExit: the campaign claims
// aligned, gates are green, but the manifest's mechanical coverage is
// below target on pass 1 — the gate must NOT converge (anti
// rubber-stamp) until coverage meets the target on pass 2.
func TestDocsRefresh_CoverageGateBlocksEarlyExit(t *testing.T) {
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1, coverageBy: []int{50, 85}}
	stubDocsRefresh(exec, st)

	run := runDocsRefresh(t, exec, "run-dr-coverage")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (coverage 50%% < 80%% target must block the first-pass exit)", got)
	}
}

// TestDocsRefresh_DefaultCreateAuthorsThenRefreshes: a repo with zero
// docs routes through author_docs, rescans, then runs the normal
// campaign (v0.3.0 DEFAULT-CREATE, preserved in v2).
func TestDocsRefresh_DefaultCreateAuthorsThenRefreshes(t *testing.T) {
	exec := newScenarioExecutor()
	st := &docsRefreshState{alignedBy: 1}
	stubDocsRefresh(exec, st)
	scans := 0
	exec.on("scan_docs", func(_ map[string]any) (map[string]any, error) {
		scans++
		noDocs := scans == 1
		count := 0
		files := []any{}
		if !noDocs {
			count = 1
			files = []any{"docs/overview.md"}
		}
		return map[string]any{
			"doc_files": files, "doc_count": count, "no_docs": noDocs,
			"footprint_hash": "h", "scope_globs": []any{"docs/**/*.md"},
			"recently_changed_code_files": []any{}, "bundle_self_path": "",
			"pre_verified_docs": []any{}, "cache_hits": 0, "cache_misses": count,
			"_tokens": 1,
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
		t.Errorf("scan_docs ran %d times, want 2 (author_rescan re-scan)", scans)
	}
	if !exec.wasCalled("campaign") {
		t.Errorf("campaign never ran after the authoring rescan")
	}
}

// TestDocsRefresh_Structural pins the v2 IR shape: the deterministic
// audit machinery + one campaign + gates, and the ABSENCE of the
// retired v1 review/fix/commit machinery.
func TestDocsRefresh_Structural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "docs-refresh/main.bot")

	if wf.Entry != "scan_docs" {
		t.Errorf("workflow entry = %q, want %q (the deterministic footprint scan leads)", wf.Entry, "scan_docs")
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
	for _, id := range []string{"scan_docs", "scan_code_surface", "build_manifest", "scope_check", "verify_run", "mark_issue_for_review", "update_audit_cache"} {
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
		"alt", "reviewer_claude", "reviewer_gpt", "streak_check",
		"fix_claude", "fix_gpt", "prepare_commit", "commit_changes",
		"enforce_fix_scope", "detect_doc_changes",
	} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("retired v1 node %q is still present — v2 is one campaign against the deterministic manifest, not the review assembly line", id)
		}
	}
}
