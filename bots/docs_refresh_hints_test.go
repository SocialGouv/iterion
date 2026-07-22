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
		DocCount       int         `json:"doc_count"`
		NoDocs         bool        `json:"no_docs"`
		NoopSkip       bool        `json:"noop_skip"`
		NoopReason     string      `json:"noop_reason"`
		Hints          []hintEntry `json:"hints"`
		HintCount      int         `json:"hint_count"`
		CheckedPaths   int         `json:"checked_paths"`
		CheckedLinks   int         `json:"checked_links"`
		LedgerExcluded int         `json:"ledger_excluded"`
		HintsNote      string      `json:"hints_note"`
	}

	run := func(t *testing.T, ws, dismissedPath string) hintsOut {
		t.Helper()
		vars := map[string]string{
			"workspace_dir":    ws,
			"doc_globs":        "README.md,docs/**/*.md,**/README.md,CLAUDE.md,**/CLAUDE.md",
			"excluded_dirs":    ".iterion,.works,.claude,vendor,node_modules,.git,dist,build,out",
			"bundle_self_path": "",
			"diff_since":       "",
			"audit_cache_path": "",
			"issue_id":         "",
			"dismissed_path":   dismissedPath,
			"max_hints":        "120",
			"docs_dir":         "docs",
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

		res := run(t, ws, "")
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

		res := run(t, ws, "")
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

	t.Run("silent_degradation_when_nothing_scannable", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, "README.md", "# quiet\n\nJust prose. No paths, no links.\n")

		res := run(t, ws, "")
		if res.HintCount != 0 {
			t.Fatalf("nothing scannable must yield zero hints, got %+v", res.Hints)
		}
		if !strings.Contains(res.HintsNote, "explore the repo directly") {
			t.Errorf("hints_note must tell the campaign to explore directly, got %q", res.HintsNote)
		}
	})

	t.Run("no_docs_routes_to_bootstrap", func(t *testing.T) {
		ws := t.TempDir()
		res := run(t, ws, "")
		if !res.NoDocs || res.DocCount != 0 {
			t.Fatalf("empty repo must report no_docs=true, got %+v", res)
		}
		if res.NoopSkip {
			t.Errorf("no_docs and noop_skip are mutually exclusive")
		}
	})

	t.Run("unmentioned_area_is_advisory_zone", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, "README.md", "# fixture\n\nNothing about the backend here.\n")
		write(t, ws, "server/handler.py", "def h():\n    pass\n")

		res := run(t, ws, "")
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

		res := run(t, ws, ledger)
		for _, h := range res.Hints {
			if h.Kind == "missing_path" && h.Value == "pkg/gone.go" {
				t.Errorf("ledger-dismissed hint re-surfaced — the agent's adjudication memory is broken: %+v", h)
			}
		}
		if res.LedgerExcluded != 1 {
			t.Errorf("ledger_excluded = %d, want 1", res.LedgerExcluded)
		}
	})
}
