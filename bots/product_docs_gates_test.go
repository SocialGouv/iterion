package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// product-docs (Prody) ships four deterministic nodes that decide the run:
// catalog_ingest (what the campaign is allowed to read), scan_hints (what it
// is advised to look at), scope_check (what it is allowed to write) and
// page_lint (what it is allowed to publish). Each is an embedded python body
// inside main.bot, so nothing but executing it proves it works — a change
// that silently truncates one would otherwise surface as a bot that quietly
// documents nothing, or a gate that approves anything.
//
// These tests extract each command from the COMPILED IR (the same string the
// engine runs), resolve its template refs the way the engine does, and run it
// against real git fixtures. No LLM, no network: the fixture "source repo" is
// a local git directory cloned over a filesystem path.

// shQuote wraps a value the way the engine's tool-command resolver does, so a
// value carrying spaces or JSON punctuation reaches python intact.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolveCommand substitutes {{vars.X}} / {{input.X}} refs in a tool command,
// then fails the test if any ref was left behind — an unresolved ref would
// otherwise reach the shell as literal braces and silently change behaviour.
func resolveCommand(t *testing.T, command string, refs map[string]string) string {
	t.Helper()
	out := command
	for k, v := range refs {
		out = strings.ReplaceAll(out, "{{"+k+"}}", shQuote(v))
	}
	if i := strings.Index(out, "{{"); i >= 0 {
		end := i + 40
		if end > len(out) {
			end = len(out)
		}
		t.Fatalf("unresolved template ref near %q", out[i:end])
	}
	return out
}

func runJSON(t *testing.T, command string, target any) {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("command failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	if uerr := json.Unmarshal(out, target); uerr != nil {
		t.Fatalf("output is not JSON: %v (out %q)", uerr, out)
	}
}

// runExpectingFailure asserts the command exits non-zero and that its stderr
// names the cause. A deterministic front door that swallows a bad catalog is
// worse than one that has none: it documents the wrong product.
func runExpectingFailure(t *testing.T, command, wantSubstr string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a loud failure, got success: %s", out)
	}
	if !strings.Contains(string(out), wantSubstr) {
		t.Fatalf("failure message does not name the cause %q:\n%s", wantSubstr, out)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireGitPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

// newSourceRepo builds a throwaway git repository standing in for one of the
// product's source repos, carrying a user-facing i18n catalog (functional
// signal) and a credential file (which must never survive into the clone).
func newSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "locales/fr.json", `{"submit":"Envoyer","status":"En instruction"}`+"\n")
	writeFile(t, dir, ".env", "SECRET_KEY=never-read-me\n")
	writeFile(t, dir, "config/app-secrets.yaml", "token: never-read-me\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "seed")
	return dir
}

type ingestOut struct {
	ProductID   string           `json:"product_id"`
	ProductDir  string           `json:"product_dir"`
	Surfaces    []any            `json:"surfaces"`
	Inventory   []map[string]any `json:"inventory"`
	Stamp       string           `json:"sources_stamp"`
	RepoCount   int              `json:"repo_count"`
	OKCount     int              `json:"ok_count"`
	Degraded    int              `json:"degraded_count"`
	Redacted    int              `json:"redacted_count"`
	PrevStamp   string           `json:"previous_stamp"`
	DeltaUnavai bool             `json:"delta_unavailable"`
	Log         string           `json:"log"`
}

// hasYAMLParser reports whether this host can parse a YAML catalog at all.
// catalog_ingest delegates YAML to PyYAML or to yq (declared in the bundle's
// devbox.json) and fails loudly when it has neither — so on a bare host the
// tests exercise the same logic through the dependency-free `.json` catalog
// form, and the YAML surface is covered wherever a parser exists.
func hasYAMLParser() bool {
	if err := exec.Command("python3", "-c", "import yaml").Run(); err == nil {
		return true
	}
	_, err := exec.LookPath("yq")
	return err == nil
}

// catalogFixture writes a product-catalog entry in the richest form this host
// can read back, and returns the file's extension so a test can assert on it.
func catalogFixture(t *testing.T, ws, rel string, yamlBody string, jsonBody string) string {
	t.Helper()
	if hasYAMLParser() {
		writeFile(t, ws, rel+".yml", yamlBody)
		return "yaml"
	}
	writeFile(t, ws, rel+".json", jsonBody)
	return "json"
}

func ingestCommand(t *testing.T, ws, catalog, product, scratch string) string {
	t.Helper()
	return resolveCommand(t, toolCommand(t, "product-docs/main.bot", "catalog_ingest"), map[string]string{
		"vars.workspace_dir": ws,
		"vars.catalog_path":  catalog,
		"vars.product_id":    product,
		"vars.scratch_dir":   scratch,
		"vars.clone_depth":   "1",
		"vars.secret_globs":  "*.env,.env,.env.*,*secret*,*secrets*,*credential*,*.pem,*.key",
	})
}

// TestProductDocsCatalogIngest pins the front door's four promises: it
// resolves the product from the catalog, it clones the sources OUT of the
// docs worktree, it strips credential files from those clones, and a repo it
// cannot read becomes a visible `degraded` entry instead of a silent absence.
func TestProductDocsCatalogIngest(t *testing.T) {
	requireGitPython(t)

	source := newSourceRepo(t)
	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\n"+
			"docs:\n"+
			"  product_dir: documentation_produits/demo\n"+
			"  surfaces:\n"+
			"    - name: Espace gestionnaire\n"+
			"repos:\n"+
			"  - id: demo-src\n"+
			"    url: "+source+"\n"+
			"  - id: unreachable\n"+
			"    url: "+missing+"\n",
		`{"id":"demo","docs":{"product_dir":"documentation_produits/demo",`+
			`"surfaces":[{"name":"Espace gestionnaire"}]},`+
			`"repos":[{"id":"demo-src","url":"`+source+`"},`+
			`{"id":"unreachable","url":"`+missing+`"}]}`+"\n")
	writeFile(t, ws, "documentation_produits/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)

	if got.ProductDir != "documentation_produits/demo" {
		t.Fatalf("product_dir = %q, want the catalog's docs.product_dir", got.ProductDir)
	}
	if got.RepoCount != 2 || got.OKCount != 1 || got.Degraded != 1 {
		t.Fatalf("inventory = %d repos / %d ok / %d degraded, want 2/1/1 (a repo that cannot be cloned must be REPORTED, not dropped): %s",
			got.RepoCount, got.OKCount, got.Degraded, got.Log)
	}
	var okEntry, badEntry map[string]any
	for _, e := range got.Inventory {
		if e["status"] == "ok" {
			okEntry = e
		} else {
			badEntry = e
		}
	}
	if okEntry == nil || badEntry == nil {
		t.Fatalf("inventory does not carry one ok + one degraded entry: %+v", got.Inventory)
	}
	if note, _ := badEntry["note"].(string); !strings.Contains(note, "clone failed") {
		t.Fatalf("degraded entry does not say WHY it is degraded: %q", note)
	}

	// The clone lives out of the docs worktree: the scope gate would be
	// meaningless if the sources landed inside it.
	clonePath, _ := okEntry["path"].(string)
	if !strings.HasPrefix(clonePath, scratch) {
		t.Fatalf("source clone at %q is not under the scratch dir %q — sources must never land in the docs worktree", clonePath, scratch)
	}
	if _, err := os.Stat(filepath.Join(clonePath, "locales/fr.json")); err != nil {
		t.Fatalf("the clone is missing the source file the campaign needs to read: %v", err)
	}
	// Redaction: credential-bearing files are gone from the clone.
	for _, secret := range []string{".env", "config/app-secrets.yaml"} {
		if _, err := os.Stat(filepath.Join(clonePath, secret)); err == nil {
			t.Fatalf("%s survived into the clone — secret_globs redaction did not run", secret)
		}
	}
	if got.Redacted != 2 {
		t.Fatalf("redacted_count = %d, want 2", got.Redacted)
	}

	// The stamp names only readable repos, and is shaped for the commit
	// trailer the next run reads back.
	if !strings.HasPrefix(got.Stamp, "demo-src@") || strings.Contains(got.Stamp, "unreachable") {
		t.Fatalf("sources_stamp = %q, want <ok-repo>@<sha> and no degraded repo", got.Stamp)
	}
	if got.PrevStamp != "" {
		t.Fatalf("previous_stamp = %q on a repo with no prior product-docs commit, want empty", got.PrevStamp)
	}
}

// TestProductDocsCatalogIngestSourceDelta pins the git-native incremental
// contract: the delta since the last run is recovered from the
// `Product-Docs-Sources:` commit trailer in the DOCS repo, with no side-car
// state file — so a crashed run or a wiped scratch dir loses nothing.
func TestProductDocsCatalogIngestSourceDelta(t *testing.T) {
	requireGitPython(t)

	source := newSourceRepo(t)
	firstSHA := strings.TrimSpace(gitIn(t, source, "rev-parse", "HEAD"))

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\n"+
			"docs:\n  product_dir: docs/demo\n"+
			"repos:\n  - id: demo-src\n    url: "+source+"\n",
		`{"id":"demo","docs":{"product_dir":"docs/demo"},`+
			`"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	// The docs commit records exactly which source commit it was written
	// against — the trailer IS the state.
	gitIn(t, ws, "commit", "-q", "-m", "docs(demo): first pass\n\nBot: product-docs\nProduct-Docs-Sources: demo-src@"+firstSHA[:12])

	// The source moves on.
	writeFile(t, source, "locales/fr.json", `{"submit":"Transmettre"}`+"\n")
	writeFile(t, source, "locales/en.json", `{"submit":"Send"}`+"\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "feat: reword the submit action")

	// A FRESH scratch dir: nothing carried over except what git holds.
	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", t.TempDir()), &got)

	if got.PrevStamp == "" {
		t.Fatalf("previous_stamp is empty — the Product-Docs-Sources trailer was not read back from the docs history")
	}
	if got.OKCount != 1 {
		t.Fatalf("ok_count = %d, want 1: %s", got.OKCount, got.Log)
	}
	changed, _ := got.Inventory[0]["changed_files"].([]any)
	// git ignores --depth on a local-path clone, so the recorded commit is
	// always reachable here: the delta MUST be computed. (When it genuinely
	// is not — a truly shallow remote clone — the contract is to set
	// delta_unavailable and say so in the note, never to report an empty
	// delta the campaign would read as "nothing changed".)
	if got.DeltaUnavai {
		note, _ := got.Inventory[0]["note"].(string)
		t.Fatalf("delta_unavailable on a full local clone — the recorded commit was reachable: %q", note)
	}
	if len(changed) == 0 {
		t.Fatalf("the source moved but changed_files is empty and delta_unavailable is false — a silently empty delta reads as 'nothing changed'")
	}
	found := map[string]bool{}
	for _, c := range changed {
		found[c.(string)] = true
	}
	if !found["locales/fr.json"] || !found["locales/en.json"] {
		t.Fatalf("changed_files = %v, want both edited source files", changed)
	}
}

// TestProductDocsCatalogIngestFailsLoudly pins the one behaviour a front door
// must never have: guessing. A missing catalog, an unknown product or an
// entry with no product_dir has to stop the run, because the alternative is
// documenting the wrong directory.
func TestProductDocsCatalogIngestFailsLoudly(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	catalogFixture(t, ws, "catalog/demo", "id: demo\nrepos: []\n", `{"id":"demo","repos":[]}`+"\n")
	writeFile(t, ws, "seed.md", "x\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	for _, tc := range []struct {
		name           string
		catalog        string
		product        string
		wantErrContain string
	}{
		{"missing catalog_path", "", "demo", "catalog_path is required"},
		{"missing product_id", "catalog", "", "product_id is required"},
		{"catalog does not exist", "nope", "demo", "does not exist"},
		{"unknown product", "catalog", "ghost", "no catalog entry for product"},
		{"entry without product_dir", "catalog", "demo", "declares no docs.product_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runExpectingFailure(t, ingestCommand(t, ws, tc.catalog, tc.product, t.TempDir()), tc.wantErrContain)
		})
	}
}

type hintsOut struct {
	Pages          []string         `json:"pages"`
	PageCount      int              `json:"page_count"`
	Hints          []map[string]any `json:"hints"`
	DeadLinkCount  int              `json:"dead_link_count"`
	OrphanCount    int              `json:"orphan_count"`
	UnmappedCount  int              `json:"unmapped_surface_count"`
	HintsNote      string           `json:"hints_note"`
	EditorialFiles []string         `json:"editorial_files"`
	Mode           string           `json:"mode"`
}

func hintsCommand(t *testing.T, ws, productDir, surfaces string) string {
	t.Helper()
	return resolveCommand(t, toolCommand(t, "product-docs/main.bot", "scan_hints"), map[string]string{
		"vars.workspace_dir":  ws,
		"input.product_dir":   productDir,
		"input.surfaces":      surfaces,
		"vars.editorial_dir":  ".product-docs",
		"vars.dismissed_path": "",
		"vars.max_hints":      "120",
		"vars.mode":           "full",
		"vars.diff_since":     "",
	})
}

// TestProductDocsScanHints pins the advisory producer's signals. In a
// hub-and-step model the navigation IS the product, so an unreachable page
// and a dead link are real defects — and a surface the catalog declares that
// no page covers is the gap the bot exists to close.
func TestProductDocsScanHints(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n\n- [Espace gestionnaire](gestionnaire/README.md)\n")
	writeFile(t, ws, "docs/demo/gestionnaire/README.md",
		"# Espace gestionnaire\n\n- [Déposer](deposer.md)\n- [Disparue](disparue.md)\n")
	writeFile(t, ws, "docs/demo/gestionnaire/deposer.md", "# Déposer\n\nCliquez sur **Envoyer**.\n")
	writeFile(t, ws, "docs/demo/gestionnaire/orpheline.md", "# Orpheline\n\nAucune page ne me lie.\n")
	writeFile(t, ws, ".product-docs/modele.md", "# Modèle local\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got hintsOut
	runJSON(t, hintsCommand(t, ws, "docs/demo", `[{"name":"Espace gestionnaire"},{"name":"Espace citoyen"}]`), &got)

	if got.PageCount != 4 {
		t.Fatalf("page_count = %d, want 4: %v", got.PageCount, got.Pages)
	}
	kinds := map[string][]string{}
	for _, h := range got.Hints {
		kinds[h["kind"].(string)] = append(kinds[h["kind"].(string)], h["value"].(string))
	}
	if got.DeadLinkCount != 1 || len(kinds["dead_link"]) != 1 || kinds["dead_link"][0] != "disparue.md" {
		t.Fatalf("dead_link hints = %v (count %d), want exactly disparue.md", kinds["dead_link"], got.DeadLinkCount)
	}
	if got.OrphanCount != 1 || !strings.HasSuffix(kinds["orphan_page"][0], "orpheline.md") {
		t.Fatalf("orphan_page hints = %v, want exactly orpheline.md (README/index pages are never orphans)", kinds["orphan_page"])
	}
	if got.UnmappedCount != 1 || kinds["unmapped_surface"][0] != "Espace citoyen" {
		t.Fatalf("unmapped_surface hints = %v, want exactly the surface no page mentions", kinds["unmapped_surface"])
	}
	if len(got.EditorialFiles) != 1 || !strings.HasSuffix(got.EditorialFiles[0], "modele.md") {
		t.Fatalf("editorial_files = %v, want the docs repo's own .product-docs/ override", got.EditorialFiles)
	}
	if !strings.Contains(got.HintsNote, "AUTHORITATIVE") {
		t.Fatalf("hints_note does not tell the campaign the docs repo's editorial line outranks the bundle defaults: %q", got.HintsNote)
	}
}

// TestProductDocsScanHintsBootstraps pins the empty-tree behaviour: a product
// with no page yet is not an error and not a silent no-op — it is the
// bootstrap pass, and the note has to say so or the campaign reads an empty
// hint list as "nothing to do".
func TestProductDocsScanHintsBootstraps(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "seed.md", "x\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got hintsOut
	runJSON(t, hintsCommand(t, ws, "docs/demo", "[]"), &got)

	if got.PageCount != 0 {
		t.Fatalf("page_count = %d on an empty product tree, want 0", got.PageCount)
	}
	if !strings.Contains(got.HintsNote, "BOOTSTRAP") {
		t.Fatalf("hints_note on an empty product tree = %q, want it to name the bootstrap pass", got.HintsNote)
	}
	if !strings.Contains(got.HintsNote, "no editorial override") {
		t.Fatalf("hints_note does not report that the bundle defaults apply: %q", got.HintsNote)
	}
}

type lintOut struct {
	LintOK      bool             `json:"lint_ok"`
	Violations  []map[string]any `json:"violations"`
	Count       int              `json:"violation_count"`
	PagesLinted int              `json:"pages_linted"`
	Log         string           `json:"log"`
}

func lintCommand(t *testing.T, ws, productDir, rules, extra string) string {
	t.Helper()
	return resolveCommand(t, toolCommand(t, "product-docs/main.bot", "page_lint"), map[string]string{
		"vars.workspace_dir":            ws,
		"input.product_dir":             productDir,
		"vars.lint_rules":               rules,
		"vars.extra_forbidden_headings": extra,
	})
}

const allLintRules = "html_comments,sources_box,clarify_section,technical_annex"

// TestProductDocsPageLint pins the EDITORIAL gate — the one truth oracle on
// the artifact this bot actually ships. A published page is read by the
// product's users; it carries no trace of how it was produced.
func TestProductDocsPageLint(t *testing.T) {
	requireGitPython(t)

	t.Run("clean page passes", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, ws, "docs/demo/README.md",
			"# Demo\n\nLe gestionnaire dépose une demande.\n\n"+
				"{% hint style=\"warning\" %}\nCette action est irréversible.\n{% endhint %}\n\n"+
				"Le délai est de 30 jours [à confirmer].\n")
		var got lintOut
		runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
		if !got.LintOK {
			t.Fatalf("a clean page was rejected: %+v", got.Violations)
		}
		if got.PagesLinted != 1 {
			t.Fatalf("pages_linted = %d, want 1", got.PagesLinted)
		}
		if got.Log != "" {
			t.Fatalf("a green lint must produce no log, got %q", got.Log)
		}
	})

	t.Run("each forbidden artefact is caught", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, ws, "docs/demo/notes.md", ""+
			"# Déposer\n\n"+
			"<!-- TODO: vérifier le délai -->\n\n"+
			"## Sources\n\n- locales/fr.json\n\n"+
			"## Points à clarifier\n\n- Qui valide ?\n\n"+
			"## Annexe : Correspondance technique\n\n- statut → enum Status\n")
		var got lintOut
		runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)

		if got.LintOK {
			t.Fatalf("a page carrying every forbidden artefact passed the lint")
		}
		byRule := map[string]int{}
		for _, v := range got.Violations {
			byRule[v["rule"].(string)]++
		}
		for _, rule := range []string{"html_comments", "sources_box", "clarify_section", "technical_annex"} {
			if byRule[rule] == 0 {
				t.Errorf("rule %s did not fire: %+v", rule, got.Violations)
			}
		}
		// The log is what reaches the next pass: it must name the page, the
		// line and the rule, or the campaign cannot remove exactly them.
		if !strings.Contains(got.Log, "notes.md:3") || !strings.Contains(got.Log, "html_comments") {
			t.Fatalf("the fail_log does not locate the violations precisely: %q", got.Log)
		}
	})

	t.Run("a Sources hint box is caught", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, ws, "docs/demo/box.md",
			"# Déposer\n\n{% hint style=\"info\" %}\nSources : locales/fr.json, routes.rb\n{% endhint %}\n")
		var got lintOut
		runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
		if got.LintOK {
			t.Fatalf("a hint box carrying a source reference passed the lint")
		}
		if got.Violations[0]["rule"] != "sources_box" {
			t.Fatalf("violation rule = %v, want sources_box", got.Violations[0]["rule"])
		}
	})

	t.Run("a rule dropped from lint_rules stops firing", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, ws, "docs/demo/notes.md", "# Déposer\n\n<!-- note -->\n\n## Sources\n\n- x\n")
		var got lintOut
		runJSON(t, lintCommand(t, ws, "docs/demo", "sources_box", ""), &got)
		if got.LintOK {
			t.Fatalf("the still-enabled sources_box rule did not fire")
		}
		for _, v := range got.Violations {
			if v["rule"] == "html_comments" {
				t.Fatalf("html_comments fired although it was dropped from lint_rules: %+v", got.Violations)
			}
		}
	})

	t.Run("extra_forbidden_headings extends the gate", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, ws, "docs/demo/p.md", "# Déposer\n\n## Notes internes\n\n- x\n")
		var got lintOut
		runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, "Notes internes"), &got)
		if got.LintOK {
			t.Fatalf("an operator-declared forbidden heading did not fire")
		}
		if got.Violations[0]["rule"] != "extra_forbidden_heading" {
			t.Fatalf("violation rule = %v, want extra_forbidden_heading", got.Violations[0]["rule"])
		}
	})
}

// TestProductDocsDeterministicPassConverges walks the whole deterministic
// chain on a fixture — catalog_ingest → scan_hints → (a simulated campaign
// pass) → scope_check → page_lint — and asserts every input the `gate`
// compute node ANDs is green.
//
// The gate is `scope_ok ∧ lint_ok ∧ campaign.docs_aligned`. Only the third
// term needs an LLM, so this test pins the other two end to end: a pass that
// writes a hub plus step sub-pages per the documentary model, links them, and
// commits each with BOTH trailers, converges. Without it, "the deterministic
// half converges" would be an assertion nobody had run.
func TestProductDocsDeterministicPassConverges(t *testing.T) {
	requireGitPython(t)

	source := newSourceRepo(t)
	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\n"+
			"docs:\n"+
			"  product_dir: documentation_produits/demo\n"+
			"  surfaces:\n    - name: Espace gestionnaire\n"+
			"repos:\n  - id: demo-src\n    url: "+source+"\n",
		`{"id":"demo","docs":{"product_dir":"documentation_produits/demo",`+
			`"surfaces":[{"name":"Espace gestionnaire"}]},`+
			`"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
	// The docs repo publishes its own editorial line — the gate must leave it
	// alone, and scan_hints must report it as in force.
	writeFile(t, ws, ".product-docs/modele.md", "# Modèle local\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "chore: seed the docs repo")

	// 1. catalog_ingest resolves the product and clones the source.
	var ingest ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &ingest)
	if ingest.OKCount != 1 || ingest.Degraded != 0 {
		t.Fatalf("ingest = %d ok / %d degraded, want 1/0: %s", ingest.OKCount, ingest.Degraded, ingest.Log)
	}
	productDir := ingest.ProductDir

	// 2. The first scan is a BOOTSTRAP: no page exists yet.
	var before hintsOut
	runJSON(t, hintsCommand(t, ws, productDir, `[{"name":"Espace gestionnaire"}]`), &before)
	if !strings.Contains(before.HintsNote, "BOOTSTRAP") {
		t.Fatalf("first pass on an empty product tree must announce the bootstrap: %q", before.HintsNote)
	}
	if !strings.Contains(before.HintsNote, "AUTHORITATIVE") {
		t.Fatalf("the docs repo's own editorial override was not reported as in force: %q", before.HintsNote)
	}

	// 3. Simulate the campaign pass: an accueil linking the hub, a hub
	//    linking its steps, one page per step — the documentary model — each
	//    committed alone with both trailers.
	stamp := ingest.Stamp
	page := func(rel, body string) {
		writeFile(t, ws, filepath.Join(productDir, rel), body)
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "docs("+rel+"): rédigé\n\nBot: product-docs\nProduct-Docs-Sources: "+stamp)
	}
	page("README.md", "# Demo\n\nDemo permet de suivre une demande.\n\n"+
		"- [Espace gestionnaire](gestionnaire/README.md)\n")
	page("gestionnaire/README.md", "# Espace gestionnaire\n\nCe rôle instruit les demandes.\n\n"+
		"1. [Déposer une demande](deposer.md)\n2. [Instruire la demande](instruire.md)\n")
	page("gestionnaire/deposer.md", "# Déposer une demande\n\nCliquez sur **Envoyer**.\n\n"+
		"{% hint style=\"warning\" %}\nCette action est irréversible.\n{% endhint %}\n")
	page("gestionnaire/instruire.md", "# Instruire la demande\n\n"+
		"La demande passe au statut **En instruction**. Le délai est de 30 jours [à confirmer].\n")

	// 4. Both deterministic gates must be green.
	var scope scopeOutPD
	runJSON(t, resolveCommand(t, toolCommand(t, "product-docs/main.bot", "scope_check"), map[string]string{
		"vars.workspace_dir": ws,
		"input.product_dir":  productDir,
	}), &scope)
	if !scope.ScopeOK {
		t.Fatalf("scope_check red after a well-behaved pass: %v — %s", scope.OutOfScope, scope.Log)
	}

	var lint lintOut
	runJSON(t, lintCommand(t, ws, productDir, allLintRules, ""), &lint)
	if !lint.LintOK {
		t.Fatalf("page_lint red on pages carrying no working notes: %+v", lint.Violations)
	}
	if lint.PagesLinted != 4 {
		t.Fatalf("pages_linted = %d, want the 4 pages the pass wrote", lint.PagesLinted)
	}

	// 5. The advisory scan must now be quiet: every page reachable, every
	//    link resolving, the declared surface covered.
	var after hintsOut
	runJSON(t, hintsCommand(t, ws, productDir, `[{"name":"Espace gestionnaire"}]`), &after)
	if after.PageCount != 4 {
		t.Fatalf("page_count = %d, want 4", after.PageCount)
	}
	if after.DeadLinkCount != 0 || after.OrphanCount != 0 || after.UnmappedCount != 0 {
		t.Fatalf("hints not quiet after a converging pass: %d dead / %d orphan / %d unmapped — %v",
			after.DeadLinkCount, after.OrphanCount, after.UnmappedCount, after.Hints)
	}

	// 6. The trailers are what make the NEXT run incremental: a fresh
	//    scratch dir must still recover the recorded source stamp.
	var reingest ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", t.TempDir()), &reingest)
	if reingest.PrevStamp != stamp {
		t.Fatalf("previous_stamp = %q, want the stamp the pass committed (%q) — the next run lost its incremental base",
			reingest.PrevStamp, stamp)
	}
}

type scopeOutPD struct {
	ScopeOK    bool     `json:"scope_ok"`
	OutOfScope []string `json:"out_of_scope"`
	Log        string   `json:"log"`
}

// TestProductDocsScopeCheck pins the writeable set. Prody runs in a docs repo
// that may hold OTHER products and the team's own editorial skills: the gate
// has to be scoped to this product's directory, not merely to `.md`.
func TestProductDocsScopeCheck(t *testing.T) {
	requireGitPython(t)

	command := toolCommand(t, "product-docs/main.bot", "scope_check")
	run := func(t *testing.T, ws, productDir string) scopeOutPD {
		t.Helper()
		var got scopeOutPD
		runJSON(t, resolveCommand(t, command, map[string]string{
			"vars.workspace_dir": ws,
			"input.product_dir":  productDir,
		}), &got)
		return got
	}

	newDocsRepo := func(t *testing.T) string {
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		writeFile(t, ws, "documentation_produits/demo/README.md", "# Demo\n")
		writeFile(t, ws, "documentation_produits/autre/README.md", "# Autre\n")
		writeFile(t, ws, ".product-docs/modele.md", "# Modèle\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "seed")
		return ws
	}
	prodyCommit := func(t *testing.T, ws, msg string) {
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", msg+"\n\nBot: product-docs\nProduct-Docs-Sources: demo-src@deadbeef")
	}

	t.Run("pages under the product dir are in scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		writeFile(t, ws, "documentation_produits/demo/gestionnaire/deposer.md", "# Déposer\n")
		prodyCommit(t, ws, "docs(demo): déposer")
		got := run(t, ws, "documentation_produits/demo")
		if !got.ScopeOK {
			t.Fatalf("a page under the product dir was flagged out of scope: %v", got.OutOfScope)
		}
	})

	t.Run("another product's pages are out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		writeFile(t, ws, "documentation_produits/autre/README.md", "# Autre (touché à tort)\n")
		prodyCommit(t, ws, "docs(autre): oops")
		got := run(t, ws, "documentation_produits/demo")
		if got.ScopeOK {
			t.Fatalf("a page belonging to ANOTHER product passed the writeable-set gate")
		}
		if !strings.Contains(got.Log, "documentation_produits/autre/README.md") {
			t.Fatalf("the fail_log does not name the offending path: %q", got.Log)
		}
	})

	t.Run("the docs repo's own editorial skills are out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		writeFile(t, ws, ".product-docs/modele.md", "# Modèle (réécrit par le bot)\n")
		prodyCommit(t, ws, "docs: oops")
		got := run(t, ws, "documentation_produits/demo")
		if got.ScopeOK {
			t.Fatalf("the bot rewrote the product team's own editorial line and the gate approved it")
		}
	})

	t.Run("a non-markdown file under the product dir is out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		writeFile(t, ws, "documentation_produits/demo/script.sh", "#!/bin/sh\n")
		prodyCommit(t, ws, "docs(demo): oops")
		got := run(t, ws, "documentation_produits/demo")
		if got.ScopeOK {
			t.Fatalf("a non-markdown file passed the writeable-set gate")
		}
	})

	t.Run("changes present before the run are not attributed to it", func(t *testing.T) {
		ws := newDocsRepo(t)
		// Someone else's code commit, landed BEFORE the run started.
		writeFile(t, ws, "tooling/build.sh", "#!/bin/sh\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "chore: not the bot's work")
		// Then the run's own page commit.
		writeFile(t, ws, "documentation_produits/demo/gestionnaire/deposer.md", "# Déposer\n")
		prodyCommit(t, ws, "docs(demo): déposer")
		got := run(t, ws, "documentation_produits/demo")
		if !got.ScopeOK {
			t.Fatalf("pre-existing changes were attributed to the run: %v", got.OutOfScope)
		}
	})
}
