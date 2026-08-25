package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// product-docs (Prody) mirrors docs-refresh's v3 shape — one capable agent +
// a mission + TRUTH gates only — with three deltas this file pins:
//
//   - catalog_ingest runs ONCE, before the loop (the source clones do not
//     change during a run), and a repo it could not read reaches the campaign
//     as an explicit `degraded` inventory entry rather than as an absence;
//   - page_lint is a THIRD gate term: a published page carrying working notes
//     blocks convergence exactly like an out-of-scope write does;
//   - the PR tail opens a DRAFT by default, because functional documentation
//     is validated by the product owners on the forge.
//
// The bots/ suite executes the four deterministic node bodies against real git
// fixtures; this file executes the GRAPH — edges, loop, gate expression and
// tail — with every node stubbed, so a rewiring that no longer converges (or
// that converges while a gate is red) fails here.

// render flattens an edge-relayed value of any shape (a bool, a JSON array
// decoded to []any, a substituted string) so an assertion can look for what it
// carries. toStr, which the other bot suites share, is string-only by design —
// the mapped fields pinned here are typed.
func render(v any) string { return fmt.Sprintf("%v", v) }

// productDocsState drives the stubs across passes.
type productDocsState struct {
	alignedBy    int // campaign reports docs_aligned=true on/after this pass
	hintCount    int // advisory hint count reported every pass (never a gate)
	inventory    []any
	pass         int
	failLogsSeen []string
	campaignIn   map[string]any // the last campaign input map
	gateInputs   map[string]map[string]any
}

// stubProductDocs registers the baseline green-path stubs. Individual tests
// override nodes afterward (later .on wins).
func stubProductDocs(exec *scenarioExecutor, st *productDocsState) {
	if st.inventory == nil {
		st.inventory = []any{map[string]any{
			"id": "demo-src", "status": "ok", "path": "/scratch/sources/demo-src",
			"sha": "deadbeef", "redacted": 2,
		}}
	}
	st.gateInputs = map[string]map[string]any{}

	exec.on("catalog_ingest", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"product_id": "demo", "product_dir": "documentation_produits/demo",
			"surfaces":  []any{map[string]any{"name": "Espace gestionnaire"}},
			"inventory": st.inventory, "sources_stamp": "demo-src@deadbeef",
			"repo_count": 1, "ok_count": 1, "degraded_count": 0, "redacted_count": 2,
			"previous_stamp": "", "delta_unavailable": false,
			"log": "cloned 1 repo", "_tokens": 1,
		}, nil
	})
	exec.on("scan_hints", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"pages": []any{"README.md"}, "page_count": 1,
			"hints": []any{map[string]any{
				"doc": "README.md", "line": 12, "kind": "dead_link",
				"value": "gestionnaire/deposer.md", "note": "link target not found",
			}},
			"hint_count": st.hintCount, "dead_link_count": 1, "orphan_count": 0,
			"unmapped_surface_count": 0, "checked_links": 3, "ledger_excluded": 0,
			"truncated": false, "hints_note": "1 dead link(s)",
			"editorial_files": []any{".product-docs/modele.md"},
			"mode":            "full", "incremental_base": "",
			"recently_changed_pages": []any{}, "_tokens": 1,
		}, nil
	})
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		st.pass++
		st.campaignIn = in
		fl := ""
		if raw, ok := in["fail_log"]; ok {
			fl = strings.TrimSpace(toStr(raw))
		}
		st.failLogsSeen = append(st.failLogsSeen, fl)
		aligned := st.pass >= st.alignedBy
		remaining := "le parcours gestionnaire n'a pas encore de page d'étape"
		if aligned {
			remaining = ""
		}
		return map[string]any{
			"docs_aligned": aligned, "commits_this_pass": 3, "drift_remaining": remaining,
			"unread_sources": "", "is_product_bug": false, "needs_human": false,
			"human_note": "", "summary": "hub + étapes rédigés", "_tokens": 10,
		}, nil
	})
	exec.on("scope_check", func(in map[string]any) (map[string]any, error) {
		st.gateInputs["scope_check"] = in
		return map[string]any{"scope_ok": true, "out_of_scope": []any{}, "log": "", "_tokens": 1}, nil
	})
	exec.on("page_lint", func(in map[string]any) (map[string]any, error) {
		st.gateInputs["page_lint"] = in
		return map[string]any{
			"lint_ok": true, "violations": []any{}, "violation_count": 0,
			"pages_linted": 4, "log": "", "_tokens": 1,
		}, nil
	})
	// available=true keeps the opt-in PR path reachable; the probe only runs
	// behind the open_mr gate.
	exec.on("forge_auth_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"available": true, "reason": "env:GH_TOKEN", "_tokens": 1}, nil
	})
	exec.on("finalize_mr", func(in map[string]any) (map[string]any, error) {
		st.gateInputs["finalize_mr"] = in
		return map[string]any{
			"opened": true, "url": "https://forge/pr/7", "branch": "iterion/product-docs/demo",
			"draft": true, "back_linked": false, "skipped_reason": "",
			"summary": "draft PR opened", "_tokens": 5,
		}, nil
	})
}

func runProductDocs(t *testing.T, exec *scenarioExecutor, runID string, inputs map[string]any) *store.Run {
	t.Helper()
	wf := compileFixtureStubSafe(t, "product-docs/main.bot")
	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, inputs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	return run
}

// TestProductDocs_ConvergesFirstPass: aligned campaign + two green gates on
// pass 1 → one pass, and with open_mr defaulting false the run finishes
// without touching the forge.
func TestProductDocs_ConvergesFirstPass(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 1}
	stubProductDocs(exec, st)

	runProductDocs(t, exec, "run-pd-first", nil)

	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1", got)
	}
	if exec.wasCalled("forge_auth_probe") || exec.wasCalled("finalize_mr") {
		t.Errorf("the PR tail fired with open_mr=false — it must be opt-in")
	}
	if st.failLogsSeen[0] != "" {
		t.Errorf("pass 1 fail_log = %q, want empty (no gate has run yet)", st.failLogsSeen[0])
	}
}

// TestProductDocs_ClonesOnceAcrossPasses is the delta docs-refresh has no
// equivalent of: catalog_ingest sits BEFORE the loop head, so a second
// continuation pass re-scans and re-campaigns but must NOT re-clone the source
// repositories (they cannot change mid-run, and re-cloning N repos per pass
// would dominate the run).
func TestProductDocs_ClonesOnceAcrossPasses(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 2, hintCount: 1}
	stubProductDocs(exec, st)

	runProductDocs(t, exec, "run-pd-clone-once", nil)

	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2", got)
	}
	if got := exec.callCount("scan_hints"); got != 2 {
		t.Errorf("scan_hints called %d times, want 2 (the loop head re-scans each pass)", got)
	}
	if got := exec.callCount("catalog_ingest"); got != 1 {
		t.Errorf("catalog_ingest called %d times, want 1 — the clones happen once, before the loop", got)
	}
}

// TestProductDocs_LintViolationBlocksConvergence pins Prody's third gate term.
// The campaign claims the documentation aligned and the writeable set is
// clean, but a published page still carries working notes: the run must NOT
// converge, and the next pass must receive the lint complaint so the agent
// removes exactly those lines.
func TestProductDocs_LintViolationBlocksConvergence(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 0} // claims aligned every pass
	stubProductDocs(exec, st)
	lintCalls := 0
	exec.on("page_lint", func(_ map[string]any) (map[string]any, error) {
		lintCalls++
		if lintCalls == 1 {
			return map[string]any{
				"lint_ok": false,
				"violations": []any{map[string]any{
					"page": "documentation_produits/demo/gestionnaire/deposer.md",
					"line": 42, "rule": "sources_box",
				}},
				"violation_count": 1, "pages_linted": 4,
				"log":     "EDITORIAL LINT: documentation_produits/demo/gestionnaire/deposer.md:42 sources_box",
				"_tokens": 1,
			}, nil
		}
		return map[string]any{
			"lint_ok": true, "violations": []any{}, "violation_count": 0,
			"pages_linted": 4, "log": "", "_tokens": 1,
		}, nil
	})

	runProductDocs(t, exec, "run-pd-lint", nil)

	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 — a red editorial lint must block convergence even when the agent reports docs_aligned", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "EDITORIAL LINT") {
		t.Errorf("second campaign pass fail_log = %v, want the lint violation so the agent strips the working notes", st.failLogsSeen)
	}
}

// TestProductDocs_ScopeViolationRoutesBack: the campaign wrote outside
// `<product_dir>/**/*.md` — another product's tree, or the docs repo's own
// editorial skills. The writeable-set gate fails the pass and the violation
// reaches the next one.
func TestProductDocs_ScopeViolationRoutesBack(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 0}
	stubProductDocs(exec, st)
	scopeCalls := 0
	exec.on("scope_check", func(_ map[string]any) (map[string]any, error) {
		scopeCalls++
		if scopeCalls == 1 {
			return map[string]any{
				"scope_ok": false, "out_of_scope": []any{".product-docs/modele.md"},
				"log":     "SCOPE VIOLATION: this run changed files outside the product writeable-set: .product-docs/modele.md",
				"_tokens": 1,
			}, nil
		}
		return map[string]any{"scope_ok": true, "out_of_scope": []any{}, "log": "", "_tokens": 1}, nil
	})

	runProductDocs(t, exec, "run-pd-scope", nil)

	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (a scope violation forces a revert pass)", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "SCOPE VIOLATION") {
		t.Errorf("second campaign pass fail_log = %v, want the scope violation so the agent reverts it", st.failLogsSeen)
	}
}

// TestProductDocs_HintsAreAdvisoryNeverGate (the v3 paradigm pin): the
// deterministic scan is HELP, not an obligation. A large residual hint count —
// dead links the agent judged not worth chasing, surfaces it deliberately did
// not cover this pass — must not block convergence. If a scanner count ever
// re-enters the gate expression, this test fails.
func TestProductDocs_HintsAreAdvisoryNeverGate(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 97}
	stubProductDocs(exec, st)

	runProductDocs(t, exec, "run-pd-advisory", nil)

	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1 — 97 residual advisory hints must not block convergence", got)
	}
}

// TestProductDocs_DegradedSourceReachesTheCampaign: a repository the front
// door could not clone becomes an explicit `degraded` inventory entry, and
// that entry must reach the campaign — which then documents the hole instead
// of inferring the missing product surface. A silent skip would read, from
// inside the agent, exactly like a product with fewer features.
func TestProductDocs_DegradedSourceReachesTheCampaign(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 0, inventory: []any{
		map[string]any{"id": "demo-src", "status": "ok", "sha": "deadbeef"},
		map[string]any{"id": "demo-api", "status": "degraded", "reason": "authentication failed: no credential for gitlab.example.org"},
	}}
	stubProductDocs(exec, st)

	runProductDocs(t, exec, "run-pd-degraded", nil)

	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1 — a degraded source is a documented hole, not a failed run", got)
	}
	inv := render(st.campaignIn["inventory"])
	if !strings.Contains(inv, "degraded") || !strings.Contains(inv, "demo-api") {
		t.Errorf("campaign inventory = %q, want the degraded entry naming the repo it could not read", inv)
	}
}

// TestProductDocs_CatalogResolutionReachesEveryConsumer pins the wiring the
// whole bot hangs on: `product_dir` is resolved ONCE by catalog_ingest and
// must reach the campaign AND both deterministic gates (a tool command cannot
// read `{{outputs.*}}`, so it rides the edge mapping — a mapping that silently
// drops it would scope the gates to an empty path and approve anything).
func TestProductDocs_CatalogResolutionReachesEveryConsumer(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 0}
	stubProductDocs(exec, st)

	runProductDocs(t, exec, "run-pd-wiring", nil)

	const wantDir = "documentation_produits/demo"
	for _, field := range []string{"product_id", "product_dir", "surfaces", "inventory", "sources_stamp", "editorial_files", "hints", "hints_note", "mode", "incremental_base"} {
		if _, ok := st.campaignIn[field]; !ok {
			t.Errorf("campaign input missing %q — the scan_hints→campaign edge must carry it", field)
		}
	}
	if got := render(st.campaignIn["product_dir"]); got != wantDir {
		t.Errorf("campaign product_dir = %q, want %q", got, wantDir)
	}
	for _, gate := range []string{"scope_check", "page_lint"} {
		if got := render(st.gateInputs[gate]["product_dir"]); got != wantDir {
			t.Errorf("%s product_dir = %q, want %q — the gate would otherwise be scoped to nothing", gate, got, wantDir)
		}
	}
}

// TestProductDocs_MRPathOpensADraft pins the opt-in delivery tail AND its
// default: with open_mr=true the converged series reaches finalize_mr with
// draft=true (vars.mr_draft), and the opened URL is surfaced as the run's
// headline result link. Functional documentation is validated by the product
// owners on the forge — marking the PR ready is their act, not the bot's.
func TestProductDocs_MRPathOpensADraft(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 0}
	stubProductDocs(exec, st)

	runProductDocs(t, exec, "run-pd-mr", map[string]any{"open_mr": true})

	if !exec.wasCalled("finalize_mr") {
		t.Fatalf("finalize_mr did not fire with open_mr=true — the converged series must open a PR")
	}
	if got := render(st.gateInputs["finalize_mr"]["draft"]); got != "true" {
		t.Errorf("finalize_mr draft = %q, want \"true\" — mr_draft defaults to a DRAFT PR", got)
	}
	if !exec.wasCalled("surface_pr_link") {
		t.Errorf("surface_pr_link never ran — the opened PR must surface as the run's headline link")
	}
}

// TestProductDocs_MRPathSkippedWithoutForgeAuth: no push credential → the
// deterministic probe short-circuits to done. finalize_mr is an LLM agent;
// entering it with nothing to push burns a turn to rediscover that in shell.
func TestProductDocs_MRPathSkippedWithoutForgeAuth(t *testing.T) {
	exec := newScenarioExecutor()
	st := &productDocsState{alignedBy: 1, hintCount: 0}
	stubProductDocs(exec, st)
	exec.on("forge_auth_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"available": false, "reason": "no forge_token secret, no *_TOKEN env, no gh host auth", "_tokens": 1,
		}, nil
	})

	runProductDocs(t, exec, "run-pd-noauth", map[string]any{"open_mr": true})

	if exec.wasCalled("finalize_mr") {
		t.Errorf("finalize_mr fired with no push credential — the deterministic probe must skip the tail")
	}
}

// TestProductDocs_Structural pins the IR shape: the deterministic front door
// as entry, ONE adaptive campaign, four deterministic gates/probes, the two
// computes — and the ABSENCE of both the retired reviewer/fixer relay (which
// re-litigates instead of converging) and any per-framework parser node (the
// campaign reads whatever stack the sources use; a `parse_openapi` node would
// be the closed enum this bot must not have).
func TestProductDocs_Structural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "product-docs/main.bot")

	if wf.Entry != "catalog_ingest" {
		t.Errorf("workflow entry = %q, want %q (the deterministic multi-repo front door leads)", wf.Entry, "catalog_ingest")
	}
	for _, id := range []string{"campaign", "finalize_mr"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected agent node %q", id)
		}
		if _, ok := node.(*ir.AgentNode); !ok {
			t.Errorf("node %q is %T, want *ir.AgentNode (adaptive)", id, node)
		}
	}
	for _, id := range []string{"catalog_ingest", "scan_hints", "scope_check", "page_lint", "forge_auth_probe", "surface_pr_link"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected tool node %q", id)
		}
		if _, ok := node.(*ir.ToolNode); !ok {
			t.Errorf("node %q is %T, want *ir.ToolNode (deterministic)", id, node)
		}
	}
	for _, id := range []string{"gate", "mr_gate"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected compute node %q", id)
		}
		if _, ok := node.(*ir.ComputeNode); !ok {
			t.Errorf("node %q is %T, want *ir.ComputeNode", id, node)
		}
	}
	for _, id := range []string{
		// the reviewer/fixer relay the v3 shape retired
		"reviewer_claude", "reviewer_gpt", "streak_check", "fix_claude", "fix_gpt",
		// a docs-only bot cannot break a build: no verify apparatus
		"verify_build", "verify_run", "verify_precheck",
		// per-framework parsing belongs to the agent, never to the graph
		"parse_openapi", "parse_i18n", "detect_framework",
	} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("node %q is present — the bot is one campaign guided by an advisory scan + truth gates + PR tail, with no per-framework parser in the graph", id)
		}
	}

	// The two catalog vars are REQUIRED: they default to empty so
	// catalog_ingest fails loudly rather than documenting the wrong product.
	for _, name := range []string{"catalog_path", "product_id"} {
		v, ok := wf.Vars[name]
		if !ok {
			t.Fatalf("workflow missing var %q", name)
		}
		if v.Default != "" {
			t.Errorf("var %q defaults to %v, want empty — a guessed catalog documents the wrong product", name, v.Default)
		}
	}
	if v, ok := wf.Vars["mr_draft"]; !ok {
		t.Errorf("workflow missing var mr_draft")
	} else if v.Default != true {
		t.Errorf("mr_draft defaults to %v, want true — the product owners mark the PR ready, not the bot", v.Default)
	}
	if v, ok := wf.Vars["open_mr"]; !ok {
		t.Errorf("workflow missing var open_mr")
	} else if v.Default != false {
		t.Errorf("open_mr defaults to %v, want false — delivery is opt-in", v.Default)
	}
}
