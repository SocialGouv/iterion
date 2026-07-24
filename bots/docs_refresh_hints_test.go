package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsRefreshHintsProducer executes docs-refresh v3's scan_hints
// command (extracted from the compiled IR) for real against fixture
// repos. It pins the two properties the v3 realignment exists for:
//
//  1. NO FOREIGN-FLAG NOISE. v2.5's cli_flag anchor scanner surfaced
//     ~200 false-positive candidates from git/docker/gh flag examples
//     quoted in docs (~40 min of adjudication burned on one live run,
//     019f8b50). v3 does not scan CLI flags at all, and a cited path is
//     only checkable when its FIRST segment exists as a repo directory
//     — so foreign-tool example lines produce zero hints by
//     construction.
//  2. SILENT DEGRADATION. On a repo whose docs cite nothing checkable
//     (or with no docs at all), the producer emits an empty hints list
//     + an explicit note and exits 0 — advisory means it can never
//     block or noise up a run.
//
// Plus the mechanics the campaign relies on: missing-path / dead-link /
// dead-anchor detection, unmentioned-area zones, and the dismissals
// ledger exclusion (the agent's adjudication memory).
func TestDocsRefreshHintsProducer(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}

	command := toolCommand(t, "docs-refresh/main.bot", "scan_hints")

	type hintEntry struct {
		Doc   string `json:"doc"`
		Line  int    `json:"line"`
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Note  string `json:"note"`
	}
	type hintsOut struct {
		DocCount        int         `json:"doc_count"`
		Hints           []hintEntry `json:"hints"`
		HintCount       int         `json:"hint_count"`
		CheckedPaths    int         `json:"checked_paths"`
		CheckedLinks    int         `json:"checked_links"`
		LedgerExcluded  int         `json:"ledger_excluded"`
		HintsNote       string      `json:"hints_note"`
		Mode            string      `json:"mode"`
		IncrementalBase string      `json:"incremental_base"`
		RecentlyChanged []string    `json:"recently_changed_code_files"`
	}

	run := func(t *testing.T, ws, dismissedPath string, overrides map[string]string) hintsOut {
		t.Helper()
		vars := map[string]string{
			"workspace_dir":    ws,
			"doc_globs":        "README.md,docs/**/*.md,**/README.md,CLAUDE.md,**/CLAUDE.md",
			"excluded_dirs":    ".iterion,.works,.claude,vendor,node_modules,.git,dist,build,out",
			"bundle_self_path": "",
			"diff_since":       "",
			"mode":             "full",
			"dismissed_path":   dismissedPath,
			"max_hints":        "120",
			"docs_dir":         "docs",
		}
		for k, v := range overrides {
			vars[k] = v
		}
		cmd := command
		for k, v := range vars {
			cmd = strings.ReplaceAll(cmd, "{{vars."+k+"}}", v)
		}
		if i := strings.Index(cmd, "{{"); i >= 0 {
			t.Fatalf("unresolved template ref in scan_hints command near %q", cmd[i:min(i+40, len(cmd))])
		}
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("scan_hints failed to execute: %v (out %q)", err, out)
		}
		var res hintsOut
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("scan_hints output is not the hints_output JSON: %v (out %q)", uerr, out)
		}
		return res
	}

	write := func(t *testing.T, ws, rel, content string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("foreign_flag_examples_produce_zero_noise", func(t *testing.T) {
		// The exact noise class that burned ~40 min on live run 019f8b50:
		// docs quoting OTHER tools' flags. v3 must surface NOTHING here.
		ws := t.TempDir()
		write(t, ws, "README.md", `# mytool

Run the audit in a container:

    docker run --rm -v "$PWD":/src ghcr.io/example/scanner:edge --severity=high

Merge the PR through the queue with `+"`gh pr merge 42 --auto --squash`"+`.
Amend with `+"`git commit --amend --no-edit`"+` and push with
`+"`git push --force-with-lease`"+`. Sources live under src/.
`)
		write(t, ws, "src/main.c", "int main(void){return 0;}\n")

		res := run(t, ws, "", nil)
		if res.HintCount != 0 || len(res.Hints) != 0 {
			t.Fatalf("foreign-tool flag examples must produce ZERO hints, got %d: %+v", res.HintCount, res.Hints)
		}
		for _, h := range res.Hints {
			if strings.Contains(h.Value, "--") {
				t.Errorf("a CLI flag surfaced as a hint (%q) — v3 must not scan flags at all", h.Value)
			}
		}
	})

	t.Run("missing_path_dead_link_dead_anchor", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, "pkg/real.go", "package pkg\n")
		write(t, ws, "docs/guide.md", "# Guide\n\n## Setup\n\nwords\n")
		write(t, ws, "README.md", `# fixture

See `+"`pkg/real.go`"+` and `+"`pkg/gone.go`"+` for details.
Read the [guide](docs/guide.md), the [missing page](nope.md),
and the [dead anchor](docs/guide.md#not-there).
`)

		res := run(t, ws, "", nil)
		kinds := map[string]string{}
		for _, h := range res.Hints {
			kinds[h.Kind+"|"+h.Value] = h.Note
		}
		if _, ok := kinds["missing_path|pkg/gone.go"]; !ok {
			t.Errorf("missing_path hint for pkg/gone.go absent (pkg/ exists, file gone): %+v", res.Hints)
		}
		if _, ok := kinds["dead_link|nope.md"]; !ok {
			t.Errorf("dead_link hint for nope.md absent: %+v", res.Hints)
		}
		if _, ok := kinds["dead_anchor|docs/guide.md#not-there"]; !ok {
			t.Errorf("dead_anchor hint for docs/guide.md#not-there absent: %+v", res.Hints)
		}
		// pkg/real.go + pkg/gone.go (backtick prose) + docs/guide.md (the
		// link target is also a repo-rooted path token on its line).
		if res.CheckedPaths != 3 {
			t.Errorf("checked_paths = %d, want 3 (pkg/real.go, pkg/gone.go, docs/guide.md)", res.CheckedPaths)
		}
	})

	t.Run("git_tracked_rule_filters_example_paths", func(t *testing.T) {
		// In a git workspace a missing path is a drift signal ONLY if git
		// ever tracked it. Example/placeholder paths under real top dirs
		// (bots/my-bot/main.bot) and runtime-only files were 604 hints /
		// 43% noise on live run 019f8ba3 without this rule.
		ws := t.TempDir()
		git := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = ws
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		git("init", "-q")
		write(t, ws, "bots/real/tracked.go", "package real\n")
		git("add", "-A")
		git("commit", "-q", "-m", "init")
		git("rm", "-q", "bots/real/tracked.go")
		git("commit", "-q", "-m", "remove tracked file")
		write(t, ws, "README.md", `# fixture

The old entry point lived in `+"`bots/real/tracked.go`"+`.
Scaffold your own with `+"`bots/my-bot/main.bot`"+` as a starting name.
`)
		git("add", "-A")
		git("commit", "-q", "-m", "docs")

		res := run(t, ws, "", nil)
		var gotTracked, gotExample bool
		for _, h := range res.Hints {
			if h.Kind == "missing_path" && h.Value == "bots/real/tracked.go" {
				gotTracked = true
			}
			if h.Kind == "missing_path" && h.Value == "bots/my-bot/main.bot" {
				gotExample = true
			}
		}
		if !gotTracked {
			t.Errorf("deleted-but-once-tracked path must hint (real drift): %+v", res.Hints)
		}
		if gotExample {
			t.Errorf("never-tracked example path must NOT hint: %+v", res.Hints)
		}
	})

	t.Run("silent_degradation_when_nothing_scannable", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, "README.md", "# quiet\n\nJust prose. No paths, no links.\n")

		res := run(t, ws, "", nil)
		if res.HintCount != 0 {
			t.Fatalf("nothing scannable must yield zero hints, got %+v", res.Hints)
		}
		if !strings.Contains(res.HintsNote, "explore the repo directly") {
			t.Errorf("hints_note must tell the campaign to explore directly, got %q", res.HintsNote)
		}
	})

	t.Run("no_docs_degrades_silently", func(t *testing.T) {
		// A repo with zero docs in scope: the producer reports doc_count=0 +
		// empty hints + a "no docs in scope" note and exits 0. There is no
		// special routing (the campaign authors the initial set itself).
		ws := t.TempDir()
		res := run(t, ws, "", nil)
		if res.DocCount != 0 || res.HintCount != 0 || len(res.Hints) != 0 {
			t.Fatalf("empty repo must report doc_count=0 + zero hints, got %+v", res)
		}
		if !strings.Contains(res.HintsNote, "no docs in scope") {
			t.Errorf("hints_note must say 'no docs in scope', got %q", res.HintsNote)
		}
	})

	t.Run("unmentioned_area_is_advisory_zone", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, "README.md", "# fixture\n\nNothing about the backend here.\n")
		write(t, ws, "server/handler.py", "def h():\n    pass\n")

		res := run(t, ws, "", nil)
		found := false
		for _, h := range res.Hints {
			if h.Kind == "unmentioned_area" && h.Value == "server" {
				found = true
				if h.Doc != "" {
					t.Errorf("area hints carry an empty doc field (ledger key contract), got %q", h.Doc)
				}
			}
		}
		if !found {
			t.Errorf("unmentioned code area server/ not surfaced as an advisory zone: %+v", res.Hints)
		}
	})

	t.Run("dismissals_ledger_excludes_settled_adjudications", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, "pkg/real.go", "package pkg\n")
		write(t, ws, "README.md", "# fixture\n\nSee `pkg/gone.go` (historical example, kept on purpose).\n")

		ledgerDir := t.TempDir()
		ledger := filepath.Join(ledgerDir, "dismissed.json")
		entries := []map[string]string{{
			"doc": "README.md", "kind": "missing_path", "value": "pkg/gone.go",
			"reason": "historical example, kept on purpose",
		}}
		raw, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ledger, raw, 0o644); err != nil {
			t.Fatal(err)
		}

		res := run(t, ws, ledger, nil)
		for _, h := range res.Hints {
			if h.Kind == "missing_path" && h.Value == "pkg/gone.go" {
				t.Errorf("ledger-dismissed hint re-surfaced — the agent's adjudication memory is broken: %+v", h)
			}
		}
		if res.LedgerExcluded != 1 {
			t.Errorf("ledger_excluded = %d, want 1", res.LedgerExcluded)
		}
	})

	t.Run("incremental_auto_detects_last_alignment_base", func(t *testing.T) {
		// The core of the weekly incremental schedule: mode=incremental with
		// no explicit diff_since → scan_hints finds the last
		// Bot: docs-refresh commit and scopes recently_changed_code_files to
		// the code changed SINCE it. A periodic run re-aligns only the delta.
		ws := t.TempDir()
		git := func(args ...string) string {
			cmd := exec.Command("git", args...)
			cmd.Dir = ws
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		git("init", "-q")
		write(t, ws, "README.md", "# fixture\n")
		write(t, ws, "pkg/old.go", "package pkg\n")
		git("add", "-A")
		git("commit", "-q", "-m", "init")
		// A prior docs-refresh alignment commit — carries the trailer.
		write(t, ws, "README.md", "# fixture\n\naligned.\n")
		git("add", "-A")
		git("commit", "-q", "-m", "docs(readme): align\n\nBot: docs-refresh")
		base := git("rev-parse", "HEAD")
		// Code changed AFTER the alignment — the incremental delta.
		write(t, ws, "pkg/new.go", "package pkg\n\nfunc New() {}\n")
		git("add", "-A")
		git("commit", "-q", "-m", "feat: add New")

		res := run(t, ws, "", map[string]string{"mode": "incremental"})
		if res.IncrementalBase != base {
			t.Errorf("incremental_base = %q, want the last Bot: docs-refresh commit %q", res.IncrementalBase, base)
		}
		var gotNew, gotOld bool
		for _, f := range res.RecentlyChanged {
			if f == "pkg/new.go" {
				gotNew = true
			}
			if f == "pkg/old.go" {
				gotOld = true
			}
		}
		if !gotNew {
			t.Errorf("recently_changed_code_files must include pkg/new.go (changed since base): %+v", res.RecentlyChanged)
		}
		if gotOld {
			t.Errorf("recently_changed_code_files must NOT include pkg/old.go (predates base): %+v", res.RecentlyChanged)
		}
	})

	t.Run("incremental_first_run_degrades_to_full", func(t *testing.T) {
		// No prior alignment commit → incremental_base empty and
		// recently_changed_code_files empty → the campaign sweeps the whole
		// corpus. A clean degrade to full, never an error.
		ws := t.TempDir()
		git := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = ws
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		git("init", "-q")
		write(t, ws, "README.md", "# fixture\n")
		git("add", "-A")
		git("commit", "-q", "-m", "init")

		res := run(t, ws, "", map[string]string{"mode": "incremental"})
		if res.IncrementalBase != "" {
			t.Errorf("incremental_base = %q, want empty (no prior Bot: docs-refresh commit)", res.IncrementalBase)
		}
		if len(res.RecentlyChanged) != 0 {
			t.Errorf("recently_changed_code_files must be empty with no base, got %+v", res.RecentlyChanged)
		}
	})
}
