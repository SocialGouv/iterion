package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// Release-scoped runs of product-docs (Prody): a release trigger (a CI job on
// tags) passes `source_refs` (read the sources at the released ref),
// `source_since` (measure the delta from the previous release, whatever merge
// strategy carried the last run's trailer) and `release_label` (say what the
// version brings). These tests execute catalog_ingest and summary_check the
// way the engine does — the compiled command, real git fixtures, no LLM.

// releaseSource stands a source repository inside ws with a release history:
//
//	seed ── v0.1.0
//	  └─ feat: add the export screen
//	  └─ fix: reword the submit action ── v0.2.0
//	  └─ feat: unreleased draft screen            (main moves on)
//
// and returns its path plus the sha of the commit between the two tags.
func releaseSource(t *testing.T, ws string) (source, midSHA string) {
	t.Helper()
	source = newLocalSource(t, ws)
	gitIn(t, source, "tag", "v0.1.0")
	writeFile(t, source, "src/export.html", "<h1>Exporter mes demandes</h1>\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "feat: add the export screen")
	midSHA = strings.TrimSpace(gitIn(t, source, "rev-parse", "HEAD"))
	writeFile(t, source, "locales/fr.json", `{"submit":"Transmettre","status":"En instruction"}`+"\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "fix: reword the submit action")
	gitIn(t, source, "tag", "v0.2.0")
	writeFile(t, source, "src/draft.html", "<h1>Brouillon</h1>\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "feat: unreleased draft screen")
	return source, midSHA
}

// releaseDocsRepo seeds the docs workspace with a one-source catalog naming
// source by url (no ref: the catalog is committed before any release exists)
// and a docs commit whose trailer records stampSHA.
func releaseDocsRepo(t *testing.T, ws, source, stampSHA string) {
	t.Helper()
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\n"+
			"docs:\n  product_dir: docs/demo\n"+
			"repos:\n  - id: demo-src\n    url: "+source+"\n",
		`{"id":"demo","docs":{"product_dir":"docs/demo"},`+
			`"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	msg := "docs(demo): first pass\n\nBot: product-docs"
	if stampSHA != "" {
		msg += "\nProduct-Docs-Sources: demo-src@" + stampSHA
	}
	gitIn(t, ws, "commit", "-q", "-m", msg)
}

func stringsOf(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok && v != nil {
		t.Fatalf("expected a JSON array, got %T (%v)", v, v)
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		out = append(out, x.(string))
	}
	return out
}

// TestProductDocsReleaseRunReadsTheReleaseWindow pins the two release vars
// together: the source is read AT the released tag (not at the branch head,
// which has moved on), and the delta is the window between the two tags —
// not the window from the previous run's trailer, which here points INSIDE
// the release (a squash merge, a hand-made commit, a run on a branch: the
// trailer is whatever the last delivered commit carried).
//
// Mutations that redden it: drop the override assignment in catalog_ingest
// (`ref = ref_overrides[ref_key]`) → the head and the clone are main's; make
// the explicit base lose to the trailer (`if since_ref:` → `if False:`) →
// changed_files is the trailer's window and no `since` field is reported.
func TestProductDocsReleaseRunReadsTheReleaseWindow(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source, midSHA := releaseSource(t, ws)
	releaseDocsRepo(t, ws, source, midSHA)
	v1 := strings.TrimSpace(gitIn(t, source, "rev-parse", "v0.1.0^{commit}"))
	v2 := strings.TrimSpace(gitIn(t, source, "rev-parse", "v0.2.0^{commit}"))

	var got ingestOut
	runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
		"vars.source_refs":  "demo-src=v0.2.0",
		"vars.source_since": "demo-src=v0.1.0",
	}), &got)

	if got.OKCount != 1 {
		t.Fatalf("ok_count = %d, want 1: %s", got.OKCount, got.Log)
	}
	e := got.Inventory[0]
	if e["head"] != v2 {
		t.Fatalf("head = %v, want the released commit %s — source_refs must replace the catalog's ref for this run", e["head"], v2)
	}
	clone, _ := e["path"].(string)
	if _, err := os.Stat(filepath.Join(clone, "src/export.html")); err != nil {
		t.Fatalf("the released clone lacks a file the release shipped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "src/draft.html")); err == nil {
		t.Fatal("the clone carries src/draft.html, committed AFTER the release — the documented ref is not the released one")
	}
	if got.DeltaUnavai {
		t.Fatalf("delta_unavailable on a reachable base: %v", e["note"])
	}
	changed := stringsOf(t, e["changed_files"])
	sort.Strings(changed)
	if want := []string{"locales/fr.json", "src/export.html"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed_files = %v, want the v0.1.0..v0.2.0 window %v (the trailer's window would miss src/export.html)", changed, want)
	}
	if e["since"] != "v0.1.0" || e["since_sha"] != v1 {
		t.Fatalf("since = %v / since_sha = %v, want v0.1.0 / %s", e["since"], e["since_sha"], v1)
	}
	commits := stringsOf(t, e["commits"])
	if want := []string{"fix: reword the submit action", "feat: add the export screen"}; !reflect.DeepEqual(commits, want) {
		t.Fatalf("commits = %v, want the release's subjects newest first %v", commits, want)
	}
	if total, _ := e["commits_total"].(float64); total != 2 {
		t.Fatalf("commits_total = %v, want 2", e["commits_total"])
	}
	// The stamp the campaign writes records the RELEASED commit, so a later
	// run with no explicit base still measures from the release.
	if got.Stamp != "demo-src@"+v2 {
		t.Fatalf("sources_stamp = %q, want demo-src@%s", got.Stamp, v2)
	}
	for _, part := range []string{"ref overrides: demo-src=v0.2.0", "explicit delta bases: demo-src=v0.1.0"} {
		if !strings.Contains(got.Log, part) {
			t.Fatalf("log %q does not name %q — the operator reads back what the run was told", got.Log, part)
		}
	}
}

// TestProductDocsReleaseWindowThatCannotBeComputedSaysSo pins the two ways a
// window is not the release's, each named: a base no commit answers to is an
// UNAVAILABLE delta (never an empty one, which the campaign would read as "the
// release changed nothing"), and a base off the documented line of history
// still yields the tree delta but no commit subject — listing base..head from
// there would name commits the release never received.
//
// Mutations that redden it: return `False` for `unavailable` on the
// unreachable base; drop the merge-base ancestry check.
func TestProductDocsReleaseWindowThatCannotBeComputedSaysSo(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source, _ := releaseSource(t, ws)
	// A tag on a side line, never merged into the release.
	gitIn(t, source, "checkout", "-q", "-b", "side", "v0.1.0")
	writeFile(t, source, "src/side.html", "<h1>Expérimentation</h1>\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "feat: side experiment")
	gitIn(t, source, "tag", "side-tag")
	gitIn(t, source, "checkout", "-q", "main")
	releaseDocsRepo(t, ws, source, "")

	t.Run("unreachable base", func(t *testing.T) {
		var got ingestOut
		runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
			"vars.source_refs":  "demo-src=v0.2.0",
			"vars.source_since": "demo-src=v9.9.9",
		}), &got)
		e := got.Inventory[0]
		if !got.DeltaUnavai {
			t.Fatalf("delta_unavailable = false for a base no commit answers to — an empty window reads as 'nothing changed': %v", e["note"])
		}
		if note, _ := e["note"].(string); !strings.Contains(note, "source_since v9.9.9") {
			t.Fatalf("note %q does not name the base it could not reach", note)
		}
		if changed := stringsOf(t, e["changed_files"]); len(changed) != 0 {
			t.Fatalf("changed_files = %v on an unavailable window, want none", changed)
		}
	})

	t.Run("base off the line of history", func(t *testing.T) {
		var got ingestOut
		runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
			"vars.source_refs":  "demo-src=v0.2.0",
			"vars.source_since": "demo-src=side-tag",
		}), &got)
		e := got.Inventory[0]
		if got.DeltaUnavai {
			t.Fatalf("delta_unavailable on a reachable base — the tree delta is computable: %v", e["note"])
		}
		changed := stringsOf(t, e["changed_files"])
		if len(changed) == 0 {
			t.Fatal("changed_files is empty — the two trees differ")
		}
		if commits := stringsOf(t, e["commits"]); len(commits) != 0 {
			t.Fatalf("commits = %v from a base that is not an ancestor — those are not the release's commits", commits)
		}
		if note, _ := e["note"].(string); !strings.Contains(note, "is not an ancestor") {
			t.Fatalf("note %q does not say why no commit subject is reported", note)
		}
	})
}

// TestProductDocsInventoryKeepsItsShapeWithoutReleaseVars pins the
// non-regression: a run given no release var emits the inventory entry it
// always did — no window fields, the trailer's delta.
func TestProductDocsInventoryKeepsItsShapeWithoutReleaseVars(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source, midSHA := releaseSource(t, ws)
	releaseDocsRepo(t, ws, source, midSHA)

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", t.TempDir()), &got)
	e := got.Inventory[0]
	for _, k := range []string{"since", "since_sha", "commits", "commits_total"} {
		if _, ok := e[k]; ok {
			t.Fatalf("inventory entry carries %q with no release var given — the shape must be the one it always was: %v", k, e)
		}
	}
	changed := stringsOf(t, e["changed_files"])
	sort.Strings(changed)
	if want := []string{"locales/fr.json", "src/draft.html"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed_files = %v, want the trailer's window %v", changed, want)
	}
}

// TestProductDocsReleaseVarsAreRefusedBeforeAnythingIsSpent pins the
// pre-flight: every release var is read by a later node (the clone, the
// summary gate, the pull request tail), and a value found wrong there is
// found after the campaign has been paid for. Each refusal is named.
//
// Mutations that redden it: drop the unknown-id check after the clone loop;
// drop the release_label pattern check; drop the forge_kind membership check.
func TestProductDocsReleaseVarsAreRefusedBeforeAnythingIsSpent(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source, _ := releaseSource(t, ws)
	releaseDocsRepo(t, ws, source, "")
	if err := os.MkdirAll(filepath.Join(ws, "docs/demo/nav.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{"label with a space", map[string]string{"vars.release_label": "1.0 beta"}, "release_label"},
		{"label shaped like an option", map[string]string{"vars.release_label": "-rc1"}, "version-like token"},
		{"label ending a ref badly", map[string]string{"vars.release_label": "1.0.lock"}, "cannot end a branch name"},
		{"unknown forge", map[string]string{"vars.forge_kind": "bitbucket"}, "forge_kind"},
		{"override without a ref", map[string]string{"vars.source_refs": "demo-src"}, "is not <catalog id>=<ref>"},
		{"option-shaped ref", map[string]string{"vars.source_refs": "demo-src=--upload-pack=x"}, "not a plain git ref name"},
		{"range-shaped ref", map[string]string{"vars.source_since": "demo-src=v1..v2"}, "not a plain git ref name"},
		{"same id twice", map[string]string{"vars.source_refs": "demo-src=v0.1.0,demo-src=v0.2.0"}, "twice"},
		{"unknown id in source_refs", map[string]string{"vars.source_refs": "front=v0.2.0"}, "does not declare"},
		{"unknown id in source_since", map[string]string{"vars.source_since": "front=v0.1.0"}, "does not declare"},
		{"absolute summary file", map[string]string{"vars.summary_file": "/SUMMARY.md"}, "summary_file must be RELATIVE"},
		{"escaping summary file", map[string]string{"vars.summary_file": "../SUMMARY.md"}, "summary_file must be RELATIVE"},
		{"summary file the run cannot write", map[string]string{"vars.summary_file": "toc.txt"}, "is not a .md file"},
		{"summary file naming a directory", map[string]string{"vars.summary_file": "nav.md"}, "names a DIRECTORY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runExpectingFailure(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), tc.extra), tc.want)
		})
	}
}

// summaryCommand resolves summary_check against ws.
func summaryCommand(t *testing.T, ws, productDir, baseSHA, summaryFile string) string {
	t.Helper()
	return resolveCommand(t, toolCommand(t, "product-docs/main.bot", "summary_check"), map[string]string{
		"vars.workspace_dir": ws,
		"input.product_dir":  productDir,
		"input.base_sha":     baseSHA,
		"vars.summary_file":  summaryFile,
		"vars.editorial_dir": ".product-docs",
	})
}

type summaryOut struct {
	OK     bool     `json:"summary_ok"`
	Armed  bool     `json:"armed"`
	Causes []string `json:"causes"`
	Count  int      `json:"cause_count"`
	Pages  int      `json:"pages"`
	Listed int      `json:"listed"`
	Log    string   `json:"log"`
}

const summaryFence = "```"

// summaryFixture stands a GitBook-shaped product tree whose table of contents
// lists every page through each form GitBook reads: a plain path, a
// directory standing for its README.md, an anchor, a link title, a
// percent-encoded name — next to what is not a page entry (an external url,
// a link inside fenced code) and a dot directory holding no page.
func summaryFixture(t *testing.T) (ws, base string) {
	t.Helper()
	ws = t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "docs/demo/README.md", "# Accueil\n")
	writeFile(t, ws, "docs/demo/parcours/README.md", "# Parcours\n")
	writeFile(t, ws, "docs/demo/parcours/etape-1.md", "# Étape 1\n")
	writeFile(t, ws, "docs/demo/glossaire.md", "# Glossaire\n")
	writeFile(t, ws, "docs/demo/mon guide.md", "# Mon guide\n")
	writeFile(t, ws, "docs/demo/.gitbook/assets/notes.md", "asset, not a page\n")
	writeFile(t, ws, "docs/demo/SUMMARY.md", "# Table of contents\n\n"+
		"* [Accueil](README.md)\n"+
		"* [Parcours](parcours/)\n"+
		"  * [Étape 1](parcours/etape-1.md#debut)\n"+
		"* [Glossaire](glossaire.md \"Le glossaire\")\n"+
		"* [Mon guide](mon%20guide.md)\n"+
		"* [Le site](https://example.org/aide)\n\n"+
		summaryFence+"md\n* [Exemple](n-existe-pas.md)\n"+summaryFence+"\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs: seed")
	return ws, strings.TrimSpace(gitIn(t, ws, "rev-parse", "HEAD"))
}

// TestProductDocsSummaryCheck pins the navigation gate: armed only by a table
// of contents, it refuses a page the table does not list and an entry that
// names no page of the product, and a table deleted during the run does not
// disarm it.
//
// Mutations that redden it: empty the unlisted list (SUMMARY_UNLISTED never
// raised); skip the existence check of an entry (no SUMMARY_DEAD_ENTRY); drop
// the containment check (an entry outside the product passes); drop the
// at-base branch (a removed table reads as no table); stop skipping fenced
// code, or stop resolving a directory to its README.md (the clean tree goes
// red).
func TestProductDocsSummaryCheck(t *testing.T) {
	requireGitPython(t)

	t.Run("every page listed", func(t *testing.T) {
		ws, base := summaryFixture(t)
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if !got.OK || !got.Armed {
			t.Fatalf("summary_ok=%v armed=%v on a table listing every page: %v", got.OK, got.Armed, got.Causes)
		}
		if got.Pages != 5 || got.Listed != 5 {
			t.Fatalf("pages=%d listed=%d, want 5/5 (the dot directory holds no page, the table itself is not one)", got.Pages, got.Listed)
		}
	})

	t.Run("a page the table does not list", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/demo/parcours/etape-2.md", "# Étape 2\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if got.OK || !got.Armed {
			t.Fatalf("summary_ok=%v armed=%v with an unlisted page", got.OK, got.Armed)
		}
		if len(got.Causes) != 1 || !strings.HasPrefix(got.Causes[0], "SUMMARY_UNLISTED docs/demo/parcours/etape-2.md") {
			t.Fatalf("causes = %v, want exactly the unlisted page named", got.Causes)
		}
		if !strings.Contains(got.Log, "1 page(s) not listed") {
			t.Fatalf("log %q does not count what it refused", got.Log)
		}
	})

	t.Run("an entry naming no page", func(t *testing.T) {
		ws, base := summaryFixture(t)
		appendSummary(t, ws, "* [Fantôme](fantome.md)\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if got.OK || len(got.Causes) != 1 || !strings.Contains(got.Causes[0], "SUMMARY_DEAD_ENTRY docs/demo/SUMMARY.md:") || !strings.Contains(got.Causes[0], "fantome.md (no such page)") {
			t.Fatalf("summary_ok=%v causes=%v, want one dead entry naming fantome.md", got.OK, got.Causes)
		}
	})

	t.Run("an entry outside the product", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/other/page.md", "# Another product\n")
		appendSummary(t, ws, "* [Ailleurs](../other/page.md)\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if got.OK || len(got.Causes) != 1 || !strings.Contains(got.Causes[0], "(outside the product directory)") {
			t.Fatalf("summary_ok=%v causes=%v, want one entry refused as outside the product", got.OK, got.Causes)
		}
	})

	t.Run("a table removed during the run", func(t *testing.T) {
		ws, base := summaryFixture(t)
		gitIn(t, ws, "rm", "-q", "docs/demo/SUMMARY.md")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if got.OK || !got.Armed || len(got.Causes) != 1 || got.Causes[0] != "SUMMARY_REMOVED docs/demo/SUMMARY.md" {
			t.Fatalf("summary_ok=%v armed=%v causes=%v, want the removal refused by name", got.OK, got.Armed, got.Causes)
		}
	})

	t.Run("no table at all", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		writeFile(t, ws, "docs/demo/README.md", "# Accueil\n")
		writeFile(t, ws, "docs/demo/orpheline.md", "# Orpheline\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "docs: seed")
		base := strings.TrimSpace(gitIn(t, ws, "rev-parse", "HEAD"))
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if !got.OK || got.Armed || got.Log != "" {
			t.Fatalf("summary_ok=%v armed=%v log=%q with no table — the gate must be inert", got.OK, got.Armed, got.Log)
		}
	})

	t.Run("disabled by an empty summary_file", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/demo/orpheline.md", "# Orpheline\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, ""), &got)
		if !got.OK || got.Armed {
			t.Fatalf("summary_ok=%v armed=%v with summary_file empty — the gate must be off", got.OK, got.Armed)
		}
	})

	t.Run("a table that is a symbolic link", func(t *testing.T) {
		ws, base := summaryFixture(t)
		gitIn(t, ws, "mv", "docs/demo/SUMMARY.md", "docs/demo/toc.md")
		if err := os.Symlink("toc.md", filepath.Join(ws, "docs/demo/SUMMARY.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if got.OK || len(got.Causes) != 1 || !strings.HasPrefix(got.Causes[0], "SUMMARY_UNREADABLE") {
			t.Fatalf("summary_ok=%v causes=%v, want the link refused", got.OK, got.Causes)
		}
	})
}

func appendSummary(t *testing.T, ws, line string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(ws, "docs/demo/SUMMARY.md"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestProductDocsReleaseWiring pins the graph around the two new contracts:
// the navigation gate sits on the path to the continuation decision and is a
// term of convergence (a gate nobody ANDs decides nothing), and what the
// campaign says the release brings reaches the pull request tail.
//
// Mutations that redden it: drop `outputs.summary_check.summary_ok` from the
// converged expression; route coverage_check straight to gate; drop the
// release_highlights mapping on the finalize_mr edge.
func TestProductDocsReleaseWiring(t *testing.T) {
	wf := compilePlanPhaseBot(t, "product-docs")

	if conv := computeExprRaw(wf, "gate", "converged"); !strings.Contains(conv, "outputs.summary_check.summary_ok") {
		t.Fatalf("gate.converged = %q, want summary_check.summary_ok ANDed in", conv)
	}
	if log := computeExprRaw(wf, "gate", "fail_log"); !strings.Contains(log, "outputs.summary_check.log") {
		t.Fatalf("gate.fail_log = %q, want the navigation refusals relayed to the next pass", log)
	}
	edges := map[string]bool{}
	var toFinalize []string
	for _, e := range wf.Edges {
		edges[e.From+"->"+e.To] = true
		if e.To == "finalize_mr" {
			if m := mappingOf(e, "release_highlights"); m != nil {
				toFinalize = append(toFinalize, m.Raw)
			}
		}
	}
	for _, want := range []string{"coverage_check->summary_check", "summary_check->gate"} {
		if !edges[want] {
			t.Fatalf("edge %s missing — the navigation gate is not on the path to the decision", want)
		}
	}
	if edges["coverage_check->gate"] {
		t.Fatal("coverage_check still reaches gate directly — summary_check would be skipped")
	}
	if len(toFinalize) != 1 || toFinalize[0] != "{{outputs.campaign.release_highlights}}" {
		t.Fatalf("finalize_mr release_highlights mappings = %v, want the campaign's output", toFinalize)
	}
}

// summaryCommandEditorial resolves summary_check with a given editorial dir.
func summaryCommandEditorial(t *testing.T, ws, productDir, baseSHA, summaryFile, editorial string) string {
	t.Helper()
	return resolveCommand(t, toolCommand(t, "product-docs/main.bot", "summary_check"), map[string]string{
		"vars.workspace_dir": ws,
		"input.product_dir":  productDir,
		"input.base_sha":     baseSHA,
		"vars.summary_file":  summaryFile,
		"vars.editorial_dir": editorial,
	})
}

// TestProductDocsSummaryCheckReadsLinksAsAReaderFollowsThem pins what counts
// as an ENTRY: a link whose title carries brackets (escaped or not) lists its
// page — refusing it ordered the campaign to list a listed page — while an
// image, a code span or an HTML comment lists nothing, since no reader follows
// them. And what counts as a PAGE: never a dot file, never the editorial
// charter when it sits under the product directory.
//
// Mutations that redden it: the link text back to `[^\]]*` (brackets refused);
// drop the `(?<!!)` lookbehind (an image lists); drop the code-span or the
// comment cut; drop the dot-file skip; drop the editorial-dir skip.
func TestProductDocsSummaryCheckReadsLinksAsAReaderFollowsThem(t *testing.T) {
	requireGitPython(t)

	t.Run("a title carrying brackets", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/demo/a.md", "# A\n")
		writeFile(t, ws, "docs/demo/b.md", "# B\n")
		appendSummary(t, ws, "* [Formulaire \\[v2\\]](a.md)\n* [Formulaire [v2]](b.md)\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if !got.OK {
			t.Fatalf("summary_ok=false for two listed pages whose titles carry brackets: %v", got.Causes)
		}
	})

	t.Run("what no reader follows", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/demo/image.md", "# Image\n")
		writeFile(t, ws, "docs/demo/code.md", "# Code\n")
		writeFile(t, ws, "docs/demo/comment.md", "# Comment\n")
		appendSummary(t, ws, "* ![Image](image.md)\n* `[Code](code.md)`\n* <!-- [Comment](comment.md) -->\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		want := map[string]bool{"image.md": false, "code.md": false, "comment.md": false}
		for _, c := range got.Causes {
			for page := range want {
				if strings.HasPrefix(c, "SUMMARY_UNLISTED docs/demo/"+page) {
					want[page] = true
				}
			}
		}
		for page, seen := range want {
			if !seen {
				t.Fatalf("%s counted as listed through a form no reader follows: causes %v", page, got.Causes)
			}
		}
	})

	t.Run("a dot file and the charter are no pages", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/demo/.brouillon.md", "# Brouillon\n")
		writeFile(t, ws, "docs/demo/charte/ton-et-style.md", "# Charte\n")
		var got summaryOut
		runJSON(t, summaryCommandEditorial(t, ws, "docs/demo", base, "SUMMARY.md", "docs/demo/charte"), &got)
		if !got.OK || got.Pages != 5 {
			t.Fatalf("summary_ok=%v pages=%d causes=%v — a dot file and the editorial charter must not be pages", got.OK, got.Pages, got.Causes)
		}
		// The same charter is a page once it is not the editorial dir.
		runJSON(t, summaryCommandEditorial(t, ws, "docs/demo", base, "SUMMARY.md", ".product-docs"), &got)
		if got.OK || len(got.Causes) != 1 || !strings.HasPrefix(got.Causes[0], "SUMMARY_UNLISTED docs/demo/charte/ton-et-style.md") {
			t.Fatalf("causes = %v, want only the non-editorial charter page unlisted", got.Causes)
		}
	})
}

// TestProductDocsCoverageCheckReadsNoTableOfContents pins the navigation /
// documentation split: with a golden-master net armed, the table of contents'
// group headings are not chapters — ordering a GitBook part title to carry a
// citation would damage the published navigation, and deleting the table to
// clear the refusal is SUMMARY_REMOVED.
//
// Mutation that reddens it: drop `os.path.normpath(p) != nav_page` from
// coverage_check's page list.
func TestProductDocsCoverageCheckReadsNoTableOfContents(t *testing.T) {
	requireGitPython(t)

	ws := newCoverageFixture(t)
	if got := runCoverage(t, ws); !got.OK {
		t.Fatalf("baseline coverage_ok=false: %s", got.Log)
	}
	writeFile(t, ws, "docs/demo/SUMMARY.md", "# Table des matières\n\n## Parcours usager\n\n* [Accueil](README.md)\n\n## Référence\n\n## Annexes\n")
	if got := runCoverage(t, ws); !got.OK {
		t.Fatalf("coverage_ok=false once a table of contents exists — its group headings were judged as chapters: %s", got.Log)
	}
}

// TestProductDocsScanHintsReadsLinksAsTheTableDoes pins the agreement between
// the advisory scan and the navigation gate on the same link: a
// percent-encoded name, a `<…>` destination holding a space and a leading `/`
// (from the product directory) resolve in both, so the scan never orders the
// campaign to "fix" a link the gate requires — a ping-pong with no form that
// satisfies both for a file name holding a space.
//
// Mutations that redden it: drop the `unquote`; drop the `<…>` alternative of
// MD_LINK_RE; drop the leading-`/` branch.
func TestProductDocsScanHintsReadsLinksAsTheTableDoes(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "docs/demo/README.md", "# Accueil\n\nLe produit, en bref.\n")
	writeFile(t, ws, "docs/demo/mon guide.md", "# Mon guide\n\nComment déposer une demande.\n")
	writeFile(t, ws, "docs/demo/annexe.md", "# Annexe\n\nLes délais.\n")
	writeFile(t, ws, "docs/demo/aide.md", "# Aide\n\nNous contacter.\n")
	writeFile(t, ws, "docs/demo/SUMMARY.md", "# Table des matières\n\n"+
		"* [Accueil](/README.md)\n"+
		"* [Mon guide](mon%20guide.md)\n"+
		"* [Annexe](</annexe.md>)\n"+
		"* [Aide](<aide.md>)\n"+
		"* [Mon guide, encore](<mon guide.md>)\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs: seed")

	var summary summaryOut
	runJSON(t, summaryCommand(t, ws, "docs/demo", strings.TrimSpace(gitIn(t, ws, "rev-parse", "HEAD")), "SUMMARY.md"), &summary)
	if !summary.OK {
		t.Fatalf("summary_check refuses the fixture itself: %v", summary.Causes)
	}
	var got hintsOut
	runJSON(t, hintsCommand(t, ws, "docs/demo", "[]"), &got)
	for _, h := range got.Hints {
		kind, _ := h["kind"].(string)
		if kind == "dead_link" || kind == "dead_anchor" || kind == "orphan_page" {
			t.Fatalf("scan_hints reports %s %v on a table summary_check accepts — the two readers disagree: %v", kind, h["value"], got.Hints)
		}
	}
}

// TestProductDocsReleaseIngestHardening pins what a release window may carry
// without breaking the front door, one scenario per guard.
//
// Mutations that redden it, one per subtest: drop `errors='replace'` from
// run_git (a Latin-1 subject crashes the node); restore the update path that
// fetched into an existing scratch clone (a planted .git runs its hook and the
// head is not the release); drop the pinned_unread stop; drop the truncation
// note; split subjects with splitlines() instead of -z; restore the
// alphanumeric-only first character of REF_RE; let a label carry `..`; key
// the cloud-runner advice on the catalog ref instead of the launch ref.
func TestProductDocsReleaseIngestHardening(t *testing.T) {
	requireGitPython(t)

	t.Run("a subject that is not UTF-8", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source, _ := releaseSource(t, ws)
		// git re-encodes a message it can decode; one naming an encoding it
		// does not know is printed as the raw bytes it stores — what a forge
		// accepts from anyone who can push.
		tree := strings.TrimSpace(gitIn(t, source, "rev-parse", "HEAD^{tree}"))
		parent := strings.TrimSpace(gitIn(t, source, "rev-parse", "HEAD"))
		obj := "tree " + tree + "\nparent " + parent + "\n" +
			"author A <a@example.org> 1700000000 +0000\ncommitter A <a@example.org> 1700000000 +0000\n" +
			"encoding x-unknown\n\nfix: corrig\xe9 l'\xe9cran\n"
		cmd := gittest.Cmd(source, "hash-object", "-t", "commit", "-w", "--stdin")
		cmd.Stdin = strings.NewReader(obj)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hash-object: %v", err)
		}
		gitIn(t, source, "update-ref", "refs/heads/main", strings.TrimSpace(string(out)))
		gitIn(t, source, "tag", "v0.3.0", "main")
		if raw, _ := gittest.Cmd(source, "log", "-1", "--format=%s", "v0.3.0").Output(); utf8.Valid(raw) {
			t.Fatalf("fixture: git printed a valid UTF-8 subject %q — the case under test is raw bytes", raw)
		}
		releaseDocsRepo(t, ws, source, "")
		scratch := t.TempDir()
		var got ingestOut
		runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", scratch, map[string]string{
			"vars.source_refs":  "demo-src=v0.3.0",
			"vars.source_since": "demo-src=v0.2.0",
		}), &got)
		commits := stringsOf(t, got.Inventory[0]["commits"])
		if len(commits) != 2 || !strings.HasPrefix(commits[0], "fix: corrig") {
			t.Fatalf("commits = %q, want the two subjects of the window, the Latin-1 one decoded with replacement", commits)
		}
		clone, _ := got.Inventory[0]["path"].(string)
		if _, err := os.Stat(filepath.Join(clone, ".git")); err == nil {
			t.Fatal("the clone kept its .git — every redacted blob is still readable")
		}
	})

	t.Run("a history planted in the scratch", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source, _ := releaseSource(t, ws)
		releaseDocsRepo(t, ws, source, "")
		scratch := t.TempDir()
		planted := filepath.Join(scratch, "sources", "demo-src")
		if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, filepath.Dir(planted), "clone", "-q", source, "demo-src")
		marker := filepath.Join(t.TempDir(), "hook-ran")
		writeFile(t, planted, ".git/hooks/reference-transaction", "#!/bin/sh\ntouch "+marker+"\n")
		if err := os.Chmod(filepath.Join(planted, ".git/hooks/reference-transaction"), 0o755); err != nil {
			t.Fatal(err)
		}
		v2 := strings.TrimSpace(gitIn(t, source, "rev-parse", "v0.2.0^{commit}"))
		var got ingestOut
		runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", scratch, map[string]string{
			"vars.source_refs": "demo-src=v0.2.0",
		}), &got)
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a hook planted in the scratch clone RAN during ingest — with the forge token in its environment")
		}
		if got.Inventory[0]["head"] != v2 {
			t.Fatalf("head = %v, want the released %s — an existing clone was reused instead of the release", got.Inventory[0]["head"], v2)
		}
	})

	t.Run("a pinned source that cannot be read stops the run", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source, _ := releaseSource(t, ws)
		releaseDocsRepo(t, ws, source, "")
		runExpectingFailure(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
			"vars.source_refs": "demo-src=v9.9.9",
		}), "the release run cannot read demo-src")
	})

	t.Run("a window larger than the list says so", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source, _ := releaseSource(t, ws)
		for i := 0; i < 501; i++ {
			writeFile(t, source, "gen/f"+strings.Repeat("0", 4-len(itoa(i)))+itoa(i)+".txt", itoa(i)+"\n")
		}
		gitIn(t, source, "add", "-A")
		// 501 files plus the draft screen committed after v0.2.0: 502 paths.
		gitIn(t, source, "commit", "-q", "-m", "chore: many files")
		gitIn(t, source, "tag", "v0.3.0")
		releaseDocsRepo(t, ws, source, "")
		var got ingestOut
		runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
			"vars.source_refs":  "demo-src=v0.3.0",
			"vars.source_since": "demo-src=v0.2.0",
		}), &got)
		if note, _ := got.Inventory[0]["note"].(string); !strings.Contains(note, "changed_files truncated: 500 of 502") {
			t.Fatalf("note %q does not say the list was cut", note)
		}
	})

	t.Run("one subject per commit", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source, _ := releaseSource(t, ws)
		writeFile(t, source, "src/cr.html", "<p>cr</p>\n")
		gitIn(t, source, "add", "-A")
		gitIn(t, source, "commit", "-q", "-m", "feat: export\rimport")
		gitIn(t, source, "tag", "v0.3.0")
		releaseDocsRepo(t, ws, source, "")
		var got ingestOut
		runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
			"vars.source_refs":  "demo-src=v0.3.0",
			"vars.source_since": "demo-src=v0.2.0",
		}), &got)
		if total, _ := got.Inventory[0]["commits_total"].(float64); total != 2 {
			t.Fatalf("commits_total = %v for two commits — a subject carrying a carriage return was split: %v", got.Inventory[0]["commits_total"], got.Inventory[0]["commits"])
		}
	})

	t.Run("a scoped package tag", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source, _ := releaseSource(t, ws)
		gitIn(t, source, "tag", "@demo/app@0.1.0", "v0.1.0")
		releaseDocsRepo(t, ws, source, "")
		var got ingestOut
		runJSON(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
			"vars.source_refs":  "demo-src=v0.2.0",
			"vars.source_since": "demo-src=@demo/app@0.1.0",
		}), &got)
		if got.Inventory[0]["since"] != "@demo/app@0.1.0" || got.DeltaUnavai {
			t.Fatalf("since = %v unavailable = %v, want the scoped tag read as the base", got.Inventory[0]["since"], got.DeltaUnavai)
		}
	})

	t.Run("a label that is no branch name", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source, _ := releaseSource(t, ws)
		releaseDocsRepo(t, ws, source, "")
		runExpectingFailure(t, ingestCommandWith(t, ws, "catalog", "demo", t.TempDir(), map[string]string{
			"vars.release_label": "1.21..0",
		}), "cannot end a branch name")
	})

	t.Run("the cloud-runner advice is for a launch ref only", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source := newLocalSource(t, ws)
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: docs/demo\nrepos:\n  - id: demo-src\n    url: "+source+"\n    ref: no-such-branch\n",
			`{"id":"demo","docs":{"product_dir":"docs/demo"},"repos":[{"id":"demo-src","url":"`+source+`","ref":"no-such-branch"}]}`+"\n")
		writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "seed")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", t.TempDir()), &got)
		if note, _ := got.Inventory[0]["note"].(string); strings.Contains(note, "forge url") {
			t.Fatalf("note %q gives the release advice to a catalog ref no launch var set", note)
		}
	})
}

// TestProductDocsAlternatesGuardReadsRawPathBytes pins the delegated-object-
// store guard against a store whose PATH is not UTF-8. Git follows the real
// bytes; a guard that decodes them with replacement looks for a directory
// that does not exist, reads no alternates, and lets the clone serve an
// out-of-workspace store — the escape the guard exists to refuse.
//
// Mutation that reddens it: the confine_local git calls and the alternates
// file read decoded with errors='replace' instead of 'surrogateescape'.
func TestProductDocsAlternatesGuardReadsRawPathBytes(t *testing.T) {
	requireGitPython(t)
	outside := t.TempDir()
	gitIn(t, outside, "init", "-q", "-b", "main")
	writeFile(t, outside, "private-notes.md", "# not for the campaign\n")
	gitIn(t, outside, "add", "-A")
	gitIn(t, outside, "commit", "-q", "-m", "seed")
	outsideHead := strings.TrimSpace(gitIn(t, outside, "rev-parse", "HEAD"))

	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	hop := filepath.Join(ws, "hop\xff.git")
	gitIn(t, ws, "init", "-q", "--bare", "-b", "main", hop)
	writeFile(t, hop, "objects/info/alternates", filepath.Join(outside, ".git", "objects")+"\n")
	mirror := filepath.Join(ws, "mirror.git")
	gitIn(t, ws, "init", "-q", "--bare", "-b", "main", mirror)
	writeFile(t, mirror, "objects/info/alternates", hop+"/objects\n")
	gitIn(t, mirror, "update-ref", "refs/heads/main", outsideHead)

	catalogFixture(t, ws, "catalog/demo",
		"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \"mirror.git\"\n",
		`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"mirror.git"}]}`+"\n")
	writeFile(t, ws, "docs/client/README.md", "# Demo\n")
	gitIn(t, ws, "add", "catalog", "docs")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	if _, err := os.Stat(filepath.Join(scratch, "sources", "demo", "private-notes.md")); err == nil {
		t.Fatal("an out-of-workspace store reached through a non-UTF-8 path was cloned into the scratch")
	}
	if got.OKCount != 0 || got.Degraded != 1 {
		t.Fatalf("%d ok / %d degraded, want the delegation refused — %s", got.OKCount, got.Degraded, got.Log)
	}
	if note, _ := got.Inventory[0]["note"].(string); !strings.Contains(note, "delegates its git objects to other stores") {
		t.Fatalf("the refusal does not name its cause: %q", note)
	}
}

// TestProductDocsAskpassKeepsTheForgePort pins the credential guard on a forge
// served on a non-default port: git's prompt names the port, so a guard keyed
// on the bare host answered nothing and every private source — and with the
// release stop, every release run — failed on such an instance. The port is
// part of the origin: another port on the same host gets nothing.
//
// Mutation that reddens it: capture `([^/:]+)` (host only) in forge_host.
func TestProductDocsAskpassKeepsTheForgePort(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	gitIn(t, ws, "remote", "add", "origin", "https://forge.example:8443/gl/team/docs.git")
	scratch := t.TempDir()
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\nproduct_dir: docs/demo\nrepos:\n  - id: src\n    url: https://evil.invalid/src.git\n",
		`{"id":"demo","product_dir":"docs/demo","repos":[{"id":"src","url":"https://evil.invalid/src.git"}]}`)
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")
	cmd := exec.Command("sh", "-c", ingestCommand(t, ws, "catalog", "demo", scratch))
	cmd.Env = append(os.Environ(), "GH_TOKEN=sekret-token-value", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	_, _ = cmd.Output()
	helper := filepath.Join(scratch, "git-askpass.sh")
	run := func(prompt string) (string, error) {
		out, err := exec.Command("sh", helper, prompt).Output()
		return string(out), err
	}
	if out, err := run("Username for 'https://forge.example:8443': "); err != nil || strings.TrimSpace(out) != "x-access-token" {
		t.Fatalf("the helper refused its own forge on its port (out %q err %v)", out, err)
	}
	if out, err := run("Username for 'https://forge.example:9999': "); err == nil {
		t.Fatalf("the helper answered another port of the same host: %q", out)
	}
}

// TestProductDocsSummaryCheckReadsDestinationsAsCommonMark pins the
// destination grammar the table shares with scan_hints: a `<...>`
// destination may hold parentheses and spaces and be followed by a title,
// and a comment spanning lines hides every link inside it.
//
// Mutations that redden it: the destination back to `([^)]+)`; the comment
// cut back to one line at a time.
func TestProductDocsSummaryCheckReadsDestinationsAsCommonMark(t *testing.T) {
	requireGitPython(t)

	t.Run("angle destinations and titles", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/demo/Guide (v2).md", "# Guide\n")
		writeFile(t, ws, "docs/demo/aide en ligne.md", "# Aide\n")
		writeFile(t, ws, "docs/demo/espace.md", "# Espace\n")
		appendSummary(t, ws, "* [Guide (v2)](<Guide (v2).md>)\n* [Aide](<aide en ligne.md> \"Aide en ligne\")\n* [Espace]( espace.md)\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if !got.OK {
			t.Fatalf("summary_ok=false for entries CommonMark reads as links to listed pages: %v", got.Causes)
		}
	})

	t.Run("a comment spanning lines", func(t *testing.T) {
		ws, base := summaryFixture(t)
		writeFile(t, ws, "docs/demo/brouillon.md", "# Brouillon\n")
		appendSummary(t, ws, "<!--\n* [Brouillon](brouillon.md)\n-->\n")
		var got summaryOut
		runJSON(t, summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
		if got.OK || len(got.Causes) != 1 || !strings.HasPrefix(got.Causes[0], "SUMMARY_UNLISTED docs/demo/brouillon.md") {
			t.Fatalf("causes = %v, want the page listed only inside a comment refused as unlisted", got.Causes)
		}
	})
}

// TestProductDocsScanHintsSurvivesARawPathInTheDocsDelta pins the advisory
// scan against a file name that is not UTF-8 in the docs repository's own
// history: the release include runs every release after the first in
// incremental mode, and a strict decode killed each of them before the
// campaign until somebody renamed the file.
//
// Mutation that reddens it: drop `errors='replace'` from scan_hints' git diff.
func TestProductDocsScanHintsSurvivesARawPathInTheDocsDelta(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "docs/demo/README.md", "# Accueil\n\nLe produit.\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs(demo): pass\n\nBot: product-docs")
	writeFile(t, ws, "assets/Proc\xe9dure.pdf", "%PDF\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "chore: legacy asset")
	var got hintsOut
	runJSON(t, resolveCommand(t, toolCommand(t, "product-docs/main.bot", "scan_hints"), map[string]string{
		"vars.workspace_dir":  ws,
		"input.product_dir":   "docs/demo",
		"input.surfaces":      "[]",
		"vars.editorial_dir":  ".product-docs",
		"vars.dismissed_path": "",
		"vars.max_hints":      "120",
		"vars.mode":           "incremental",
		"vars.diff_since":     "",
	}), &got)
	if got.Mode != "incremental" {
		t.Fatalf("mode = %q, want the incremental pass computed", got.Mode)
	}
}

// TestProductDocsAlternatesGuardReadsRawRevParsePaths is the guard's second
// door: the object directory git REPORTS (rev-parse --git-path objects) for a
// repository whose commondir names a store with a non-UTF-8 path. Decoded
// with replacement, the reported store does not exist, its alternates are
// never read, and git — following the real bytes — serves the outside store.
//
// Mutation that reddens it: the four confine_local rev-parse calls decoded
// with errors='replace'.
func TestProductDocsAlternatesGuardReadsRawRevParsePaths(t *testing.T) {
	requireGitPython(t)
	outside := t.TempDir()
	gitIn(t, outside, "init", "-q", "-b", "main")
	writeFile(t, outside, "private-notes.md", "# not for the campaign\n")
	gitIn(t, outside, "add", "-A")
	gitIn(t, outside, "commit", "-q", "-m", "seed")
	outsideHead := strings.TrimSpace(gitIn(t, outside, "rev-parse", "HEAD"))

	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	store := filepath.Join(ws, "st\xff.git")
	gitIn(t, ws, "init", "-q", "--bare", "-b", "main", store)
	writeFile(t, store, "objects/info/alternates", filepath.Join(outside, ".git", "objects")+"\n")
	gitIn(t, store, "update-ref", "refs/heads/main", outsideHead)
	writeFile(t, ws, "wrap.git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, ws, "wrap.git/commondir", "../st\xff.git\n")

	catalogFixture(t, ws, "catalog/demo",
		"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \"wrap.git\"\n",
		`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"wrap.git"}]}`+"\n")
	writeFile(t, ws, "docs/client/README.md", "# Demo\n")
	gitIn(t, ws, "add", "catalog", "docs")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	if _, err := os.Stat(filepath.Join(scratch, "sources", "demo", "private-notes.md")); err == nil {
		t.Fatal("an out-of-workspace store reached through a reported non-UTF-8 object directory was cloned")
	}
	if got.OKCount != 0 {
		t.Fatalf("%d ok, want the delegation refused — %s", got.OKCount, got.Log)
	}
}

// askpassHelperFor runs catalog_ingest in a docs repo whose origin is
// originURL and returns a function asking the generated helper one prompt.
func askpassHelperFor(t *testing.T, originURL string) func(string) (string, error) {
	t.Helper()
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	gitIn(t, ws, "remote", "add", "origin", originURL)
	scratch := t.TempDir()
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\nproduct_dir: docs/demo\nrepos:\n  - id: src\n    url: https://evil.invalid/src.git\n",
		`{"id":"demo","product_dir":"docs/demo","repos":[{"id":"src","url":"https://evil.invalid/src.git"}]}`)
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")
	cmd := exec.Command("sh", "-c", ingestCommand(t, ws, "catalog", "demo", scratch))
	cmd.Env = append(os.Environ(), "GH_TOKEN=sekret-token-value", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	_, _ = cmd.Output()
	helper := filepath.Join(scratch, "git-askpass.sh")
	return func(prompt string) (string, error) {
		out, err := exec.Command("sh", helper, prompt).Output()
		return string(out), err
	}
}

// TestProductDocsAskpassReadsTheHTTPSOrigin pins which origin forms key the
// guard on a port: only an https/http origin on a NON-default port. An ssh
// origin on its own port says nothing of the https port the forge's sources
// use — keying on it withheld the token from every https source of the same
// forge — and an explicit default port is the default port.
//
// Mutations that redden it: keep the port of any scheme; keep an explicit
// default port.
func TestProductDocsAskpassReadsTheHTTPSOrigin(t *testing.T) {
	requireGitPython(t)
	for _, tc := range []struct{ origin, prompt string }{
		{"ssh://git@forge.example:2222/gl/team/docs.git", "Username for 'https://forge.example': "},
		{"https://forge.example:443/gl/team/docs.git", "Username for 'https://forge.example': "},
		{"git@forge.example:gl/team/docs.git", "Username for 'https://forge.example': "},
	} {
		run := askpassHelperFor(t, tc.origin)
		if out, err := run(tc.prompt); err != nil || strings.TrimSpace(out) != "x-access-token" {
			t.Fatalf("origin %s: the helper refused its own forge's https prompt %q (out %q err %v)", tc.origin, tc.prompt, out, err)
		}
	}
}

// TestProductDocsSummaryCheckSurvivesARawPageName pins the gate's output
// against a page whose name is not UTF-8: the name reaches the causes with a
// lone surrogate, and a print that cannot encode it killed the node on a
// strict UTF-8 stdout instead of naming the page.
//
// Mutation that reddens it: print the verdict with ensure_ascii=False.
func TestProductDocsSummaryCheckSurvivesARawPageName(t *testing.T) {
	requireGitPython(t)
	ws, base := summaryFixture(t)
	writeFile(t, ws, "docs/demo/br\xffouillon.md", "# Brouillon\n")
	var got summaryOut
	runJSON(t, "PYTHONIOENCODING=utf-8:strict "+summaryCommand(t, ws, "docs/demo", base, "SUMMARY.md"), &got)
	if got.OK || len(got.Causes) != 1 || !strings.HasPrefix(got.Causes[0], "SUMMARY_UNLISTED docs/demo/br") {
		t.Fatalf("causes = %v, want the raw-named page named as unlisted", got.Causes)
	}
}

// TestProductDocsAnyObjectDelegationIsRefused pins the guard's promise since
// the alternates file stopped being parsed: a local source whose object store
// declares ANY delegation is refused — inside the workspace too, and whatever
// spelling git would accept (a quoted line was one of four forms the old
// reader followed somewhere else than git) — while an EMPTY file, which
// delegates nothing, refuses nothing.
//
// Mutations that redden it: drop the `if declared:` refusal (the in-workspace
// and the quoted delegations clone); refuse an empty file too (the plain
// source is refused).
func TestProductDocsAnyObjectDelegationIsRefused(t *testing.T) {
	requireGitPython(t)
	outside := t.TempDir()
	gitIn(t, outside, "init", "-q", "-b", "main")
	writeFile(t, outside, "private-notes.md", "# not for the campaign\n")
	gitIn(t, outside, "add", "-A")
	gitIn(t, outside, "commit", "-q", "-m", "seed")
	outsideHead := strings.TrimSpace(gitIn(t, outside, "rev-parse", "HEAD"))

	ingestOne := func(t *testing.T, ws, path string) ingestOut {
		t.Helper()
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \""+path+"\"\n",
			`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"`+path+`"}]}`+"\n")
		writeFile(t, ws, "docs/client/README.md", "# Demo\n")
		gitIn(t, ws, "add", "catalog", "docs")
		gitIn(t, ws, "commit", "-q", "-m", "seed")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", t.TempDir()), &got)
		return got
	}

	t.Run("a delegation that stays inside the workspace", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		hop := filepath.Join(ws, "hop.git")
		gitIn(t, ws, "clone", "-q", "--bare", outside, hop)
		mirror := filepath.Join(ws, "mirror.git")
		gitIn(t, ws, "init", "-q", "--bare", "-b", "main", mirror)
		writeFile(t, mirror, "objects/info/alternates", hop+"/objects\n")
		gitIn(t, mirror, "update-ref", "refs/heads/main", outsideHead)
		got := ingestOne(t, ws, "mirror.git")
		if got.OKCount != 0 {
			t.Fatalf("a delegating store was cloned: %v", got.Inventory)
		}
		if note, _ := got.Inventory[0]["note"].(string); !strings.Contains(note, "delegates its git objects to other stores") {
			t.Fatalf("the refusal does not name its cause: %q", note)
		}
	})

	t.Run("a quoted delegation", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		// The quoted line sits on the SECOND hop, the shape that escaped:
		// mirror -> hop (inside) -> "outside" (a form git unquotes).
		hop := filepath.Join(ws, "hop.git")
		gitIn(t, ws, "init", "-q", "--bare", "-b", "main", hop)
		writeFile(t, hop, "objects/info/alternates", "\""+filepath.Join(outside, ".git", "objects")+"\"\n")
		mirror := filepath.Join(ws, "mirror.git")
		gitIn(t, ws, "init", "-q", "--bare", "-b", "main", mirror)
		writeFile(t, mirror, "objects/info/alternates", hop+"/objects\n")
		gitIn(t, mirror, "update-ref", "refs/heads/main", outsideHead)
		got := ingestOne(t, ws, "mirror.git")
		if got.OKCount != 0 {
			t.Fatalf("a delegation chain ending on a quoted outside store was cloned: %v", got.Inventory)
		}
	})

	t.Run("an empty alternates file delegates nothing", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source := newLocalSource(t, ws)
		writeFile(t, source, ".git/objects/info/alternates", "\n")
		got := ingestOne(t, ws, localSourceRel)
		if got.OKCount != 1 {
			t.Fatalf("a source with an EMPTY alternates file was refused: %v", got.Inventory)
		}
	})
}

// TestProductDocsLocalSourceIsJudgedAsTheCloneOpensIt pins two readings of a
// local source the guard must share with git clone. A subdirectory of a bare
// repository answers "bare" to rev-parse, yet git clone then opens the SIBLING
// <cand>.git, which nobody judged and which may delegate anywhere. And the
// docs workspace read by itself (`path: .`) from a linked worktree — every
// local `worktree: auto` run — keeps its objects in the main repository's
// common dir, outside the worktree: that is the workspace's own store, while
// another repository's worktree planted inside the workspace stays refused.
//
// Mutations that redden it: drop `gitdir == realpath(cand)` from the bare
// branch (the sibling is cloned); drop the workspace common-dir allowance
// (`path: .` is refused in a worktree).
func TestProductDocsLocalSourceIsJudgedAsTheCloneOpensIt(t *testing.T) {
	requireGitPython(t)
	outside := t.TempDir()
	gitIn(t, outside, "init", "-q", "-b", "main")
	writeFile(t, outside, "private-notes.md", "# not for the campaign\n")
	gitIn(t, outside, "add", "-A")
	gitIn(t, outside, "commit", "-q", "-m", "seed")
	outsideHead := strings.TrimSpace(gitIn(t, outside, "rev-parse", "HEAD"))

	seedCatalog := func(t *testing.T, ws, path string) {
		t.Helper()
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \""+path+"\"\n",
			`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"`+path+`"}]}`+"\n")
		writeFile(t, ws, "docs/client/README.md", "# Demo\n")
		gitIn(t, ws, "add", "catalog", "docs")
		gitIn(t, ws, "commit", "-q", "-m", "seed")
	}

	t.Run("a subdirectory of a bare repository", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		judged := filepath.Join(ws, "vendor", "b.git")
		gitIn(t, ws, "init", "-q", "--bare", "-b", "main", judged)
		writeFile(t, judged, "x/README", "a directory inside the bare repository\n")
		sibling := filepath.Join(judged, "x.git")
		gitIn(t, ws, "init", "-q", "--bare", "-b", "main", sibling)
		writeFile(t, sibling, "objects/info/alternates", filepath.Join(outside, ".git", "objects")+"\n")
		gitIn(t, sibling, "update-ref", "refs/heads/main", outsideHead)
		seedCatalog(t, ws, "vendor/b.git/x")
		scratch := t.TempDir()
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
		if _, err := os.Stat(filepath.Join(scratch, "sources", "demo", "private-notes.md")); err == nil || got.OKCount != 0 {
			t.Fatalf("the clone opened the sibling <cand>.git nobody judged (ok=%d): %v", got.OKCount, got.Inventory)
		}
	})

	t.Run("the workspace itself from a linked worktree", func(t *testing.T) {
		main := t.TempDir()
		gitIn(t, main, "init", "-q", "-b", "main")
		seedCatalog(t, main, ".")
		wt := filepath.Join(t.TempDir(), "run1")
		gitIn(t, main, "worktree", "add", "-q", "--detach", wt)
		var got ingestOut
		runJSON(t, ingestCommand(t, wt, "catalog", "demo", t.TempDir()), &got)
		if got.OKCount != 1 {
			t.Fatalf("path: . refused from a linked worktree: %v", got.Inventory)
		}
	})

	t.Run("another repository's worktree inside the workspace", func(t *testing.T) {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		gitIn(t, outside, "worktree", "add", "-q", "--detach", filepath.Join(ws, "vendor", "wt"))
		seedCatalog(t, ws, "vendor/wt")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", t.TempDir()), &got)
		if got.OKCount != 0 {
			t.Fatalf("a foreign repository's worktree was read as a source: %v", got.Inventory)
		}
	})
}
