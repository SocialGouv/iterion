package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The deterministic half of wiki-gen (Wikky): scan_repo's noop state machine,
// gen_indexes' frontmatter-driven index regeneration, validate_wiki's
// truth-oracle gate (frontmatter + links + docs-only scope) and update_cache's
// inter-run persistence have no LLM in the path. The prose the author agent
// writes is the live layer's subject; everything that JUDGES that prose
// structurally is pinned here, at two layers: the real scripts extracted from
// the compiled bot, and the graph walk where the engine's own compute gate
// decides convergence from the validator's verdict.
//
// The scripts read their vars the way the engine substitutes them: bare
// strings, single-quote escaped.

// wikiGitRepo seeds a committed git repo with one README (identity configured,
// tree clean) and returns its path.
func wikiGitRepo(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	gitRun(t, ws, "init")
	gitRun(t, ws, "config", "user.email", "wikky-e2e@example.com")
	gitRun(t, ws, "config", "user.name", "wikky-e2e")
	writeWSFile(t, ws, "README.md", "seed\n")
	gitRun(t, ws, "add", "-A")
	gitRun(t, ws, "commit", "-m", "seed")
	return ws
}

// runWikiTool runs a wiki-gen tool command through the shared shield-family
// harness (runBotTool, in the supply-shield file beside its siblings).
func runWikiTool(t *testing.T, dir, cmd string, refs map[string]string) map[string]any {
	t.Helper()
	return runBotTool(t, dir, cmd, refs, "wiki-gen pipeline")
}

// wikiScanRefs is scan_repo's var block at its defaults for the given repo.
func wikiScanRefs(ws, cachePath string) map[string]string {
	return map[string]string{
		"vars.workspace_dir":    shqs(ws),
		"vars.wiki_dir":         shqs("wiki"),
		"vars.code_scope_globs": shqs(""),
		"vars.excluded_dirs":    shqs(".iterion,.works,.claude,vendor,node_modules,.git,dist,build,out,.next,target,coverage"),
		"vars.bundle_self_path": shqs(""),
		"vars.wiki_cache_path":  shqs(cachePath),
		"vars.issue_id":         shqs(""),
	}
}

// TestWikiGen_ScanRepo_NoopStateMachine walks scan_repo's decision table on a
// real repo: a repo without a wiki must generate; a wiki current for HEAD on
// a clean tree noop-skips at zero cost; a code change since the cached head
// names the changed files; an uncommitted tree refuses to skip. The skip is
// the bot's whole cost story — a scheduled run on an unchanged repo finishes
// without a single model call.
func TestWikiGen_ScanRepo_NoopStateMachine(t *testing.T) {
	t.Parallel()
	ws := wikiGitRepo(t)
	cachePath := filepath.Join(t.TempDir(), "wiki-cache.json")
	scan := func() map[string]any {
		return runWikiTool(t, ws, shieldNode(t, "wiki-gen/main.bot", "scan_repo").Command, wikiScanRefs(ws, cachePath))
	}

	// No wiki yet: the run must generate.
	first := scan()
	if first["wiki_exists"] != false || first["noop_skip"] != false {
		t.Fatalf("first scan = exists %v noop %v, want false/false (nothing to be current about)", first["wiki_exists"], first["noop_skip"])
	}
	if r, _ := first["noop_reason"].(string); !strings.Contains(r, "must generate") {
		t.Fatalf("first scan reason = %q, want the generate reason", r)
	}

	// A wiki appears, committed in stride (the author's mandate), and is
	// cached for the current HEAD.
	writeWSFile(t, ws, "wiki/quickstart.md", "---\ntype: Guide\n---\nstart here\n")
	writeWSFile(t, ws, "wiki/architecture.md", "---\ntype: Concept\n---\nhow it fits\n")
	gitRun(t, ws, "add", "-A")
	gitRun(t, ws, "commit", "-m", "docs: seed wiki")
	upd := shieldNode(t, "wiki-gen/main.bot", "update_cache").Command
	runWikiTool(t, ws, upd, map[string]string{
		"input.workspace_dir": shqs(ws),
		"input.wiki_dir":      shqs("wiki"),
		"input.cache_path":    shqs(cachePath),
	})
	head := testGitOut(t, ws, "rev-parse", "HEAD")

	current := scan()
	if current["noop_skip"] != true {
		t.Fatalf("current wiki on a clean tree must noop-skip: %v", current)
	}
	if r, _ := current["noop_reason"].(string); !strings.Contains(r, head[:12]) {
		t.Fatalf("noop reason = %q, want the cached HEAD %s named", r, head[:12])
	}

	// A committed code change: the scan names it and the run generates.
	writeWSFile(t, ws, "src/main.go", "package main\n")
	gitRun(t, ws, "add", "-A")
	gitRun(t, ws, "commit", "-m", "feat: src")
	changed := scan()
	if changed["noop_skip"] != false {
		t.Fatalf("scan after a code change must not skip: %v", changed)
	}
	files, _ := changed["changed_code_files"].([]any)
	if len(files) != 1 || files[0] != "src/main.go" {
		t.Fatalf("changed_code_files = %v, want [src/main.go]", files)
	}

	// Re-cache at the new head, then dirty the tree: an uncommitted change
	// holds the skip — the wiki would document a tree nobody committed.
	runWikiTool(t, ws, upd, map[string]string{
		"input.workspace_dir": shqs(ws),
		"input.wiki_dir":      shqs("wiki"),
		"input.cache_path":    shqs(cachePath),
	})
	writeWSFile(t, ws, "notes.txt", "draft thoughts\n")
	dirty := scan()
	if dirty["noop_skip"] != false {
		t.Fatalf("scan on a dirty tree must not skip: %v", dirty)
	}
	if r, _ := dirty["noop_reason"].(string); !strings.Contains(r, "uncommitted") {
		t.Fatalf("dirty reason = %q, want the uncommitted-changes reason", r)
	}
}

// TestWikiGen_GenIndexes_RegeneratesFromFrontmatter pins gen_indexes: indexes
// regenerate from the concept pages' frontmatter (sorted, reserved files
// never listed as concepts), the root index carries the OKF version, nested
// indexes carry none — and the regeneration COMMITS when it changed, so the
// next reader meets the indexes in the tree, not in a run artifact.
func TestWikiGen_GenIndexes_RegeneratesFromFrontmatter(t *testing.T) {
	t.Parallel()
	ws := wikiGitRepo(t)
	writeWSFile(t, ws, "wiki/quickstart.md", "---\ntype: Guide\ntitle: Start Here\ndescription: the entry point\n---\nhi\n")
	writeWSFile(t, ws, "wiki/concepts/zebra.md", "---\ntype: Concept\ntitle: Zebra Concept\ndescription: last alphabetically\n---\nz\n")
	writeWSFile(t, ws, "wiki/concepts/apple.md", "---\ntype: Concept\ntitle: Apple Concept\n---\na\n")
	writeWSFile(t, ws, "wiki/log.md", "work log, never a concept\n")
	gitRun(t, ws, "add", "-A")
	gitRun(t, ws, "commit", "-m", "docs: seed wiki")

	headBefore := testGitOut(t, ws, "rev-parse", "HEAD")
	out := runWikiTool(t, ws, shieldNode(t, "wiki-gen/main.bot", "gen_indexes").Command, map[string]string{
		"input.workspace_dir": shqs(ws),
		"input.wiki_dir":      shqs("wiki"),
		"input.okf_version":   shqs("0.1"),
	})
	if n, _ := out["indexes_written"].(float64); n != 2 {
		t.Fatalf("indexes_written = %v, want 2 (root + concepts/): %v", out["indexes_written"], out)
	}
	if out["committed"] != true {
		t.Fatalf("regeneration must commit the changed indexes: %v", out)
	}
	if testGitOut(t, ws, "rev-parse", "HEAD") == headBefore {
		t.Fatal("committed=true but HEAD did not move")
	}

	root := readWSFile(t, ws, "wiki/index.md")
	if !strings.Contains(root, `okf_version: "0.1"`) {
		t.Fatalf("root index missing the okf_version stamp:\n%s", root)
	}
	// The root index lists the quickstart by its frontmatter title with its
	// description, and the concepts/ dir under Directories.
	if !strings.Contains(root, "[Start Here](quickstart.md) — the entry point") {
		t.Fatalf("root index missing the titled quickstart entry:\n%s", root)
	}
	if !strings.Contains(root, "[concepts](concepts/)") {
		t.Fatalf("root index missing the concepts/ directory entry:\n%s", root)
	}
	if strings.Contains(root, "log.md") {
		t.Fatalf("reserved log.md listed in the index:\n%s", root)
	}
	// The nested index lists its concepts sorted by title, and carries no
	// frontmatter (OKF).
	concepts := readWSFile(t, ws, "wiki/concepts/index.md")
	if strings.Contains(concepts, "okf_version") {
		t.Fatalf("nested index must carry no frontmatter (OKF):\n%s", concepts)
	}
	apple := strings.Index(concepts, "[Apple Concept](apple.md)")
	zebra := strings.Index(concepts, "[Zebra Concept](zebra.md)")
	if apple == -1 || zebra == -1 || zebra < apple {
		t.Fatalf("concept entries not sorted by title:\n%s", concepts)
	}

	// A no-change regeneration writes and commits nothing.
	second := runWikiTool(t, ws, shieldNode(t, "wiki-gen/main.bot", "gen_indexes").Command, map[string]string{
		"input.workspace_dir": shqs(ws),
		"input.wiki_dir":      shqs("wiki"),
		"input.okf_version":   shqs("0.1"),
	})
	if n, _ := second["indexes_written"].(float64); n != 0 || second["committed"] != false {
		t.Fatalf("idempotent regeneration wrote %v / committed %v, want 0/false", second["indexes_written"], second["committed"])
	}
}

// wikiPage writes a wiki page with the given body and returns its repo path.
func wikiPage(t *testing.T, ws, rel, body string) {
	t.Helper()
	writeWSFile(t, ws, filepath.Join("wiki", rel), body)
}

func readWSFile(t *testing.T, ws, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestWikiGen_ValidateWiki_GateRefusesAndWarns pins the truth oracle's three
// blocking arms (frontmatter, links, scope), the source-ref allowance and the
// two warnings that surface without blocking. The gate expression reads these
// flags verbatim — a flag that flipped without a real defect would either
// strand a good wiki in the loop or ship a broken one green.
func TestWikiGen_ValidateWiki_GateRefusesAndWarns(t *testing.T) {
	t.Parallel()
	validate := func(t *testing.T, ws string) map[string]any {
		return runWikiTool(t, ws, shieldNode(t, "wiki-gen/main.bot", "validate_wiki").Command, map[string]string{
			"input.workspace_dir": shqs(ws),
			"input.wiki_dir":      shqs("wiki"),
			"input.base":          shqs(""),
		})
	}

	t.Run("valid wiki passes", func(t *testing.T) {
		t.Parallel()
		ws := wikiGitRepo(t)
		wikiPage(t, ws, "quickstart.md", "---\ntype: Guide\n---\n[The concept](concepts/auth.md)\n")
		wikiPage(t, ws, "concepts/auth.md", "---\ntype: Concept\n---\nauth, see [source](../../internal/auth/auth.go)\n")
		writeWSFile(t, ws, "internal/auth/auth.go", "package auth\n")
		gitRun(t, ws, "add", "-A")
		gitRun(t, ws, "commit", "-m", "docs+code")
		out := validate(t, ws)
		for _, flag := range []string{"frontmatter_valid", "links_ok", "scope_ok"} {
			if out[flag] != true {
				t.Fatalf("%s = %v on a valid wiki: %v", flag, out[flag], out)
			}
		}
		if log, _ := out["log"].(string); log != "" {
			t.Fatalf("valid wiki logged %q, want silence", log)
		}
	})

	t.Run("missing type frontmatter blocks", func(t *testing.T) {
		t.Parallel()
		ws := wikiGitRepo(t)
		wikiPage(t, ws, "quickstart.md", "---\ntitle: no type here\n---\nhi\n")
		out := validate(t, ws)
		if out["frontmatter_valid"] != false {
			t.Fatalf("frontmatter_valid = %v on a type-less page: %v", out["frontmatter_valid"], out)
		}
		if log, _ := out["log"].(string); !strings.Contains(log, "FRONTMATTER") {
			t.Fatalf("log = %q, want the frontmatter complaint", log)
		}
	})

	t.Run("dead intra-wiki link blocks", func(t *testing.T) {
		t.Parallel()
		ws := wikiGitRepo(t)
		wikiPage(t, ws, "quickstart.md", "---\ntype: Guide\n---\n[ghost](concepts/ghost.md) and [bad anchor](concepts/auth.md#no-such-anchor)\n")
		wikiPage(t, ws, "concepts/auth.md", "---\ntype: Concept\n---\nreal page\n")
		gitRun(t, ws, "add", "-A")
		gitRun(t, ws, "commit", "-m", "docs")
		out := validate(t, ws)
		if out["links_ok"] != false {
			t.Fatalf("links_ok = %v with a missing target and a missing anchor: %v", out["links_ok"], out)
		}
		log, _ := out["log"].(string)
		if !strings.Contains(log, "concepts/ghost.md") {
			t.Fatalf("log = %q, want the missing target named", log)
		}
		if !strings.Contains(log, "(missing #anchor)") {
			t.Fatalf("log = %q, want the anchor complaint", log)
		}
	})

	t.Run("broken source ref blocks, a real one does not", func(t *testing.T) {
		t.Parallel()
		ws := wikiGitRepo(t)
		writeWSFile(t, ws, "internal/auth/auth.go", "package auth\n")
		wikiPage(t, ws, "quickstart.md", "---\ntype: Guide\n---\n[real](../internal/auth/auth.go) [broken](../internal/auth/missing.go)\n")
		gitRun(t, ws, "add", "-A")
		gitRun(t, ws, "commit", "-m", "docs+code")
		out := validate(t, ws)
		if out["links_ok"] != false {
			t.Fatalf("links_ok = %v with a broken source ref: %v", out["links_ok"], out)
		}
		if log, _ := out["log"].(string); !strings.Contains(log, "missing.go") || strings.Contains(log, "auth.go)") {
			t.Fatalf("log = %q, want only the broken ref flagged", log)
		}
	})

	t.Run("out-of-scope write blocks", func(t *testing.T) {
		t.Parallel()
		ws := wikiGitRepo(t)
		wikiPage(t, ws, "quickstart.md", "---\ntype: Guide\n---\nhi\n")
		gitRun(t, ws, "add", "-A")
		gitRun(t, ws, "commit", "-m", "docs")
		// The uncommitted note OUTSIDE the wiki tree: the author's mandate is
		// docs-only, and validate_wiki is what makes that true.
		writeWSFile(t, ws, "notes.txt", "uncommitted scratch\n")
		out := validate(t, ws)
		if out["scope_ok"] != false {
			t.Fatalf("scope_ok = %v with an out-of-tree write: %v", out["scope_ok"], out)
		}
		if log, _ := out["log"].(string); !strings.Contains(log, "SCOPE VIOLATION") || !strings.Contains(log, "notes.txt") {
			t.Fatalf("log = %q, want the scope violation naming notes.txt", log)
		}
	})

	t.Run("orphans and missing quickstart warn without blocking", func(t *testing.T) {
		t.Parallel()
		ws := wikiGitRepo(t)
		wikiPage(t, ws, "concepts/island.md", "---\ntype: Concept\n---\nnobody links here\n")
		gitRun(t, ws, "add", "-A")
		gitRun(t, ws, "commit", "-m", "docs")
		out := validate(t, ws)
		for _, flag := range []string{"frontmatter_valid", "links_ok", "scope_ok"} {
			if out[flag] != true {
				t.Fatalf("%s = %v, warnings must not block: %v", flag, out[flag], out)
			}
		}
		log, _ := out["log"].(string)
		if !strings.Contains(log, "WARNING") || !strings.Contains(log, "quickstart") || !strings.Contains(log, "island.md") {
			t.Fatalf("log = %q, want both warnings (no quickstart, orphan island.md)", log)
		}
	})
}

// TestWikiGen_UpdateCache_PersistsHeadAndPageShas pins the persistence the
// noop gate reads back: head + per-page shas in one JSON file, an empty
// cache_path disabling the write — the cache is what makes the SECOND run of
// a scheduled bot free.
func TestWikiGen_UpdateCache_PersistsHeadAndPageShas(t *testing.T) {
	t.Parallel()
	ws := wikiGitRepo(t)
	wikiPage(t, ws, "quickstart.md", "---\ntype: Guide\n---\nhi\n")
	cachePath := filepath.Join(t.TempDir(), "wiki-cache.json")
	upd := shieldNode(t, "wiki-gen/main.bot", "update_cache").Command
	out := runWikiTool(t, ws, upd, map[string]string{
		"input.workspace_dir": shqs(ws),
		"input.wiki_dir":      shqs("wiki"),
		"input.cache_path":    shqs(cachePath),
	})
	if n, _ := out["entries_written"].(float64); n != 1 {
		t.Fatalf("entries_written = %v, want 1", out["entries_written"])
	}
	b, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	var cache struct {
		V       int               `json:"v"`
		GitHead string            `json:"git_head"`
		Pages   map[string]string `json:"pages"`
	}
	if err := json.Unmarshal(b, &cache); err != nil {
		t.Fatalf("cache not valid JSON: %v", err)
	}
	if cache.V != 1 || cache.GitHead != testGitOut(t, ws, "rev-parse", "HEAD") {
		t.Fatalf("cache head/version = %s/%d, want the repo HEAD/1", cache.GitHead, cache.V)
	}
	if _, ok := cache.Pages[filepath.Join("wiki", "quickstart.md")]; !ok {
		t.Fatalf("pages = %v, want the quickstart page keyed by repo path", cache.Pages)
	}

	// No cache_path: the write is disabled, not an error.
	skipped := runWikiTool(t, ws, upd, map[string]string{
		"input.workspace_dir": shqs(ws),
		"input.wiki_dir":      shqs("wiki"),
		"input.cache_path":    shqs(""),
	})
	if skipped["skipped"] == "" {
		t.Fatalf("empty cache_path must skip, got %v", skipped)
	}
}

// runWikiGraph drives the compiled wiki-gen workflow through the engine with
// the four tools and the author stubbed at the scenario seam — the edges, the
// compute gate and the continuation loop are the engine's own, which is the
// point: the tools' verdicts are read by the tests above, this layer pins what
// the GRAPH does with them.
func runWikiGraph(t *testing.T, runID string, vars map[string]any, author func(pass int) map[string]any, validate func(pass int) map[string]any) (*scenarioExecutor, *store.Run) {
	t.Helper()
	wf := compileFixtureStubSafe(t, "wiki-gen/main.bot")
	wf.Worktree = "none"
	wf.Supervisors = nil
	s := tmpStore(t)
	exec := newScenarioExecutor()
	pass := 0
	exec.on("scan_repo", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"wiki_exists": false, "wiki_pages": []any{}, "wiki_page_count": 0,
			"repo_top": []any{"README.md"}, "changed_code_files": []any{},
			"noop_skip": false, "noop_reason": "no wiki yet (must generate)",
			"git_head": "0123456789abcdef", "_tokens": 1,
		}, nil
	})
	exec.on("author", func(map[string]any) (map[string]any, error) {
		pass++
		out := map[string]any{
			"wiki_complete": true, "pages_written": 1, "remaining": "",
			"findings_filed": false, "summary": "claims complete", "_tokens": 10,
		}
		for k, v := range author(pass) {
			out[k] = v
		}
		return out, nil
	})
	exec.on("gen_indexes", func(map[string]any) (map[string]any, error) {
		return map[string]any{"indexes_written": 0, "committed": false, "note": "", "_tokens": 1}, nil
	})
	exec.on("validate_wiki", func(map[string]any) (map[string]any, error) {
		return validate(pass), nil
	})
	exec.on("mark_issue_for_review", func(map[string]any) (map[string]any, error) {
		return map[string]any{"text": `{"skipped":"no issue_id"}`, "_tokens": 1}, nil
	})
	exec.on("update_cache", func(map[string]any) (map[string]any, error) {
		return map[string]any{"cache_path": "/tmp/wc.json", "entries_written": 1, "git_head": "0123456789abcdef", "skipped": "", "_tokens": 1}, nil
	})
	eng := newEngine(t, wf, s, exec)
	if err := eng.Run(context.Background(), runID, vars); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return exec, run
}

// validWikiVerdict is the validator's all-green output.
func validWikiVerdict() map[string]any {
	return map[string]any{
		"frontmatter_valid": true, "links_ok": true, "scope_ok": true,
		"invalid_frontmatter": []any{}, "dead_links": []any{}, "orphans": []any{}, "out_of_scope": []any{},
		"page_count": 1, "has_quickstart": true, "log": "", "_tokens": 1,
	}
}

// TestWikiGen_Run_CurrentWikiSkipsAtZeroCost: the noop edge is the graph's
// highest-priority exit — a scan that says current finishes the run without
// the author, the validator, the issue transition or the cache write running.
func TestWikiGen_Run_CurrentWikiSkipsAtZeroCost(t *testing.T) {
	t.Parallel()
	wf := compileFixtureStubSafe(t, "wiki-gen/main.bot")
	wf.Worktree = "none"
	wf.Supervisors = nil
	s := tmpStore(t)
	exec := newScenarioExecutor()
	exec.on("scan_repo", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"wiki_exists": true, "wiki_pages": []any{"wiki/quickstart.md"}, "wiki_page_count": 1,
			"repo_top": []any{"README.md"}, "changed_code_files": []any{},
			"noop_skip": true, "noop_reason": "wiki already current for HEAD 0123456789ab",
			"git_head": "0123456789abcdef", "_tokens": 1,
		}, nil
	})
	for _, node := range []string{"author", "gen_indexes", "validate_wiki", "mark_issue_for_review", "update_cache"} {
		exec.on(node, func(map[string]any) (map[string]any, error) {
			return map[string]any{"_tokens": 1}, nil
		})
	}
	eng := newEngine(t, wf, s, exec)
	if err := eng.Run(context.Background(), "run-wikky-noop", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-wikky-noop")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", run.Status)
	}
	for _, node := range []string{"author", "gen_indexes", "validate_wiki", "mark_issue_for_review", "update_cache"} {
		if exec.wasCalled(node) {
			t.Fatalf("%s ran behind a noop skip — the zero-cost promise is broken", node)
		}
	}
}

// TestWikiGen_Run_ScopeViolationLoopsUntilTheGatePasses: the author CLAIMING
// complete never converges the run — only the validator's verdict does. A
// scope violation on pass 1 sends the run back to the author with the log,
// and only a green pass 2 finishes it; the cache refresh runs once, at the
// end, not per pass.
func TestWikiGen_Run_ScopeViolationLoopsUntilTheGatePasses(t *testing.T) {
	t.Parallel()
	exec, run := runWikiGraph(t, "run-wikky-scope-loop", map[string]any{"max_passes": 3},
		func(int) map[string]any { return nil }, // the author claims complete every pass
		func(pass int) map[string]any {
			if pass == 1 {
				return map[string]any{
					"frontmatter_valid": true, "links_ok": true, "scope_ok": false,
					"invalid_frontmatter": []any{}, "dead_links": []any{}, "orphans": []any{},
					"out_of_scope": []any{"notes.txt"},
					"page_count":   1, "has_quickstart": true,
					"log": "SCOPE VIOLATION (wrote outside wiki/ — revert): notes.txt", "_tokens": 1,
				}
			}
			return validWikiVerdict()
		})

	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s (%s), want finished after the green pass", run.Status, run.Error)
	}
	if got := exec.callCount("author"); got != 2 {
		t.Fatalf("author ran %d times, want 2 — a scope violation on pass 1 is not convergence, whatever the author claims", got)
	}
	if got := exec.callCount("validate_wiki"); got != 2 {
		t.Fatalf("validate_wiki ran %d times, want 2 (once per author pass)", got)
	}
	if got := exec.callCount("update_cache"); got != 1 {
		t.Fatalf("update_cache ran %d times, want 1 (the convergence tail, not per pass)", got)
	}
	if got := exec.callCount("mark_issue_for_review"); got != 1 {
		t.Fatalf("mark_issue_for_review ran %d times, want 1", got)
	}
}
