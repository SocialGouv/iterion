package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	BaseSHA     string           `json:"base_sha"`
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
	// Deleting a secret from the clone's WORKTREE leaves the same blob one
	// `git show` away in its history. The campaign has a shell.
	if _, err := os.Stat(filepath.Join(clonePath, ".git")); err == nil {
		t.Fatal("the clone kept its .git: every redacted secret is still readable from the pack")
	}
}

// TestProductDocsCatalogIngestRefusesHostilecatalog pins the front door
// against the catalog itself. The catalog is operator input that reaches a
// filesystem destination (rmtree + clone) and git argument lists — the two
// places where a benign-looking value stops being data.
func TestProductDocsCatalogIngestRefusesHostileCatalog(t *testing.T) {
	requireGitPython(t)

	source := newSourceRepo(t)

	newWS := func(t *testing.T) (string, string) {
		t.Helper()
		ws := t.TempDir()
		scratch := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		writeFile(t, ws, "documentation_produits/demo/README.md", "# Demo\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "seed")
		return ws, scratch
	}

	// An id is a directory NAME. Left raw it is a path: os.path.join with an
	// absolute id discards the scratch root, and the node rmtree's whatever it
	// lands on before cloning over it.
	t.Run("a path-shaped repo id cannot escape the scratch root", func(t *testing.T) {
		ws, scratch := newWS(t)
		victim := t.TempDir()
		writeFile(t, victim, "important/data.txt", "des données de l'hôte\n")
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: documentation_produits/demo\n"+
				"repos:\n  - id: "+victim+"\n    url: "+source+"\n",
			`{"id":"demo","docs":{"product_dir":"documentation_produits/demo"},`+
				`"repos":[{"id":"`+victim+`","url":"`+source+`"}]}`+"\n")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
		if _, err := os.Stat(filepath.Join(victim, "important/data.txt")); err != nil {
			t.Fatalf("a catalog id deleted a directory outside the scratch root: %v", err)
		}
		for _, e := range got.Inventory {
			p, _ := e["path"].(string)
			if p != "" && !strings.HasPrefix(p, scratch) {
				t.Fatalf("clone destination %q escaped the scratch root %q", p, scratch)
			}
		}
	})

	// `..` as an id would rmtree the scratch root itself — every other clone
	// and the ledgers with it.
	t.Run("a dot-dot repo id cannot wipe the scratch root", func(t *testing.T) {
		ws, scratch := newWS(t)
		sibling := filepath.Join(scratch, "sources", "autre-clone")
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatal(err)
		}
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: documentation_produits/demo\n"+
				"repos:\n  - id: '..'\n    url: "+source+"\n",
			`{"id":"demo","docs":{"product_dir":"documentation_produits/demo"},`+
				`"repos":[{"id":"..","url":"`+source+`"}]}`+"\n")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
		if _, err := os.Stat(sibling); err != nil {
			t.Fatalf("a '..' id wiped the scratch root, taking the other clones with it: %v", err)
		}
		_ = got
	})

	// A url or ref that starts with a dash is read by git as an OPTION;
	// ext::<command> is executed outright.
	t.Run("option-shaped and remote-helper sources are refused", func(t *testing.T) {
		for _, hostile := range []string{"--upload-pack=touch /tmp/product-docs-pwned", "ext::sh -c touch% /tmp/product-docs-pwned"} {
			ws, scratch := newWS(t)
			catalogFixture(t, ws, "catalog/demo",
				"id: demo\ndocs:\n  product_dir: documentation_produits/demo\n"+
					"repos:\n  - id: hostile\n    url: '"+hostile+"'\n",
				`{"id":"demo","docs":{"product_dir":"documentation_produits/demo"},`+
					`"repos":[{"id":"hostile","url":"`+hostile+`"}]}`+"\n")
			var got ingestOut
			runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
			if got.OKCount != 0 {
				t.Fatalf("a git-option / remote-helper url was treated as a repository: %s", got.Log)
			}
			if _, err := os.Stat("/tmp/product-docs-pwned"); err == nil {
				os.Remove("/tmp/product-docs-pwned")
				t.Fatalf("the catalog url executed a command")
			}
		}
	})

	// The stamp is read from a commit MESSAGE in the docs repo — anyone who
	// lands a commit writes it — and is handed to git as a REF.
	t.Run("a stamp that is not a sha is never handed to git", func(t *testing.T) {
		ws, scratch := newWS(t)
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: documentation_produits/demo\n"+
				"repos:\n  - id: demo-src\n    url: "+source+"\n",
			`{"id":"demo","docs":{"product_dir":"documentation_produits/demo"},`+
				`"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
		writeFile(t, ws, "documentation_produits/demo/page.md", "# Page\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m",
			"docs(demo): page\n\nBot: product-docs\nProduct-Docs-Sources: demo-src@--upload-pack=touch /tmp/product-docs-stamp-pwned")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
		if _, err := os.Stat("/tmp/product-docs-stamp-pwned"); err == nil {
			os.Remove("/tmp/product-docs-stamp-pwned")
			t.Fatal("a commit trailer executed a command through git")
		}
		if got.OKCount != 1 {
			t.Fatalf("the run degraded on a hostile stamp instead of ignoring it: %s", got.Log)
		}
	})

	// A url may carry inline credentials; the inventory reaches the campaign
	// prompt, the run events and the PR body.
	t.Run("inline credentials are stripped from the reported url", func(t *testing.T) {
		ws, scratch := newWS(t)
		hostile := "https://x-access-token:ghp_INLINECREDENTIAL987654321@github.invalid/org/repo.git"
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: documentation_produits/demo\n"+
				"repos:\n  - id: privee\n    url: "+hostile+"\n",
			`{"id":"demo","docs":{"product_dir":"documentation_produits/demo"},`+
				`"repos":[{"id":"privee","url":"`+hostile+`"}]}`+"\n")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
		if strings.Contains(got.Log, "ghp_INLINECREDENTIAL") {
			t.Fatalf("the log echoed an inline credential: %s", got.Log)
		}
		for _, e := range got.Inventory {
			for k, v := range e {
				if s, ok := v.(string); ok && strings.Contains(s, "ghp_INLINECREDENTIAL") {
					t.Fatalf("inventory[%q] echoed the inline credential verbatim: %s", k, s)
				}
			}
		}
	})

	// The whole python body lives inside a double-quoted `python3 -c "..."`
	// string that sh parses FIRST: a literal dollar is expanded by the shell
	// before python ever sees it. Here that erased both arguments of the
	// askpass helper, leaving a shell syntax error on disk — so every
	// credentialed clone failed and every private repo landed as `degraded`,
	// silently.
	t.Run("the credential helper survives the shell", func(t *testing.T) {
		ws, scratch := newWS(t)
		// The helper is host-scoped to the docs repo's own origin: declare
		// one so it exists, and probe it with that same host below.
		gitIn(t, ws, "remote", "add", "origin", "https://forge/team/docs.git")
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: documentation_produits/demo\n"+
				"repos:\n  - id: demo-src\n    url: "+source+"\n",
			`{"id":"demo","docs":{"product_dir":"documentation_produits/demo"},`+
				`"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
		cmd := exec.Command("sh", "-c", ingestCommand(t, ws, "catalog", "demo", scratch))
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GH_TOKEN=jeton-de-test-42",
		)
		if out, err := cmd.Output(); err != nil {
			t.Fatalf("ingest failed with a token present: %v (%s)", err, out)
		}
		helper := filepath.Join(scratch, "git-askpass.sh")
		body, err := os.ReadFile(helper)
		if err != nil {
			t.Fatalf("no askpass helper was written: %v", err)
		}
		if !strings.Contains(string(body), "case $1 in") || !strings.Contains(string(body), "$PRODUCT_DOCS_GIT_TOKEN") {
			t.Fatalf("the shell ate the helper's arguments before python wrote it:\n%s", body)
		}
		// ...and it actually answers git's two prompts.
		ask := exec.Command("sh", helper, "Username for 'https://forge':")
		ask.Env = append(os.Environ(), "PRODUCT_DOCS_GIT_TOKEN=jeton-de-test-42")
		userOut, err := ask.Output()
		if err != nil {
			t.Fatalf("the helper is not a runnable script: %v", err)
		}
		if strings.TrimSpace(string(userOut)) != "x-access-token" {
			t.Fatalf("helper answered %q to a Username prompt", userOut)
		}
		ask = exec.Command("sh", helper, "Password for 'https://forge':")
		ask.Env = append(os.Environ(), "PRODUCT_DOCS_GIT_TOKEN=jeton-de-test-42")
		pwOut, err := ask.Output()
		if err != nil {
			t.Fatalf("the helper failed on a Password prompt: %v", err)
		}
		if strings.TrimSpace(string(pwOut)) != "jeton-de-test-42" {
			t.Fatalf("helper answered %q to a Password prompt, want the token", pwOut)
		}
	})

	// docs.product_dir is compared as a literal prefix by scope_check: a
	// non-canonical or glob-shaped value passes here and then makes every pass
	// unfixably red.
	t.Run("a product_dir that the scope gate cannot match is refused", func(t *testing.T) {
		for _, bad := range []string{"/etc/passwd-docs", "*", "docs/../../etc"} {
			ws, scratch := newWS(t)
			catalogFixture(t, ws, "catalog/demo",
				"id: demo\ndocs:\n  product_dir: '"+bad+"'\n"+
					"repos:\n  - id: demo-src\n    url: "+source+"\n",
				`{"id":"demo","docs":{"product_dir":"`+bad+`"},`+
					`"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
			runExpectingFailure(t, ingestCommand(t, ws, "catalog", "demo", scratch), "product-docs:")
		}
	})

	// ...but a merely non-canonical form is CANONICALISED, not refused: an
	// operator writing ./docs/produit means the same directory.
	t.Run("a non-canonical product_dir is canonicalised", func(t *testing.T) {
		ws, scratch := newWS(t)
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: ./documentation_produits/demo/\n"+
				"repos:\n  - id: demo-src\n    url: "+source+"\n",
			`{"id":"demo","docs":{"product_dir":"./documentation_produits/demo/"},`+
				`"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
		if got.ProductDir != "documentation_produits/demo" {
			t.Fatalf("product_dir = %q, want the canonical form the scope gate matches", got.ProductDir)
		}
	})
}

// TestProductDocsCatalogIngestSourceDelta pins the git-native incremental
// contract: the delta since the last run is recovered from the
// `Product-Docs-Sources:` commit trailer in the DOCS repo, with no side-car
// state file — so a crashed run or a wiped scratch dir loses nothing.
// TestProductDocsSourceStampIsScopedToTheProduct pins the incremental base in
// the shape the design actually has: ONE docs repo, SEVERAL products. The
// `Product-Docs-Sources:` trailer carries no product identity, so an unscoped
// history lookup reads whichever product ran last — and a stamp that matches
// nothing yields an empty delta the campaign reads as "nothing changed", with
// delta_unavailable false. The weekly incremental run then silently does
// nothing and reports success.
func TestProductDocsSourceStampIsScopedToTheProduct(t *testing.T) {
	requireGitPython(t)

	source := newSourceRepo(t)
	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	for _, id := range []string{"alpha", "beta"} {
		catalogFixture(t, ws, "catalog/"+id,
			"id: "+id+"\ndocs:\n  product_dir: documentation_produits/"+id+"\n"+
				"repos:\n  - id: plateforme\n    url: "+source+"\n",
			`{"id":"`+id+`","docs":{"product_dir":"documentation_produits/`+id+`"},`+
				`"repos":[{"id":"plateforme","url":"`+source+`"}]}`+"\n")
		writeFile(t, ws, "documentation_produits/"+id+"/README.md", "# "+id+"\n")
	}
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	// alpha is documented against the source as it stands now.
	var first ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "alpha", scratch), &first)
	alphaStamp := first.Stamp
	writeFile(t, ws, "documentation_produits/alpha/page.md", "# Page alpha\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs(alpha): page\n\nBot: product-docs\nProduct-Docs-Sources: "+alphaStamp)

	// The source then moves on, and BETA runs last — recording ITS stamp.
	writeFile(t, source, "locales/fr.json", `{"submit":"Transmettre"}`+"\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "feat: renommer le bouton")
	var betaIngest ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "beta", scratch), &betaIngest)
	writeFile(t, ws, "documentation_produits/beta/page.md", "# Page beta\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs(beta): page\n\nBot: product-docs\nProduct-Docs-Sources: "+betaIngest.Stamp)

	// Re-running ALPHA must recover ALPHA's own base, not beta's.
	var again ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "alpha", scratch), &again)
	if again.PrevStamp != alphaStamp {
		t.Fatalf("previous_stamp = %q, want alpha's own %q — a sibling product's stamp was read as this product's incremental base",
			again.PrevStamp, alphaStamp)
	}
	var changed []any
	for _, e := range again.Inventory {
		if e["id"] == "plateforme" {
			changed, _ = e["changed_files"].([]any)
		}
	}
	if len(changed) == 0 && !again.DeltaUnavai {
		t.Fatal("the source moved but the delta is empty AND not reported unavailable: the campaign reads that as 'nothing changed' and the incremental pass becomes a silent no-op")
	}
}

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
	Pages           []string         `json:"pages"`
	PageCount       int              `json:"page_count"`
	Hints           []map[string]any `json:"hints"`
	DeadLinkCount   int              `json:"dead_link_count"`
	OrphanCount     int              `json:"orphan_count"`
	UnmappedCount   int              `json:"unmapped_surface_count"`
	HintsNote       string           `json:"hints_note"`
	EditorialFiles  []string         `json:"editorial_files"`
	Mode            string           `json:"mode"`
	IncrementalBase string           `json:"incremental_base"`
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

const allLintRules = "html_comments,sources_box,clarify_section,technical_annex,secret_material"

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

	// This gate BLOCKS: a rule that fires on reader-facing prose orders the
	// campaign to delete legitimate content, and the run never converges. Every
	// line below is ordinary French product documentation.
	t.Run("reader-facing prose is not chrome", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, ws, "docs/demo/aides.md", ""+
			"# Aides\n\n"+
			"## Sources de financement\n\nLe barème est publié chaque année.\n\n"+
			"**Source :** arrêté du 3 mars.\n\n"+
			"## Points à clarifier avec votre conseiller\n\nPrenez rendez-vous.\n\n"+
			"Exemple de gabarit :\n\n"+
			"```html\n<!-- remplacez le nom du bénéficiaire -->\n```\n\n"+
			"Mot de passe : 8 caractères minimum, dont un chiffre.\n")
		var got lintOut
		runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
		if !got.LintOK {
			t.Fatalf("the editorial gate fired on reader-facing prose — the campaign would be told to delete it: %+v", got.Violations)
		}
	})

	// No document state may switch a rule off for the rest of a page: a hint
	// closed on its own line, or never closed, used to swallow every heading
	// rule to EOF.
	t.Run("a hint block cannot disarm the gate", func(t *testing.T) {
		for _, hint := range []string{
			"{% hint style=\"info\" %}Vérifiez vos pièces.{% endhint %}\n",
			"{% hint style=\"info\" %}\nVérifiez vos pièces.\n",
		} {
			ws := t.TempDir()
			writeFile(t, ws, "docs/demo/p.md", "# Déposer\n\n"+hint+"\n## Sources\n\n- app.ts\n\n## Points à clarifier\n\n- qui valide ?\n")
			var got lintOut
			runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
			if got.LintOK {
				t.Fatalf("a hint block disarmed the editorial gate for the rest of the page (hint %q)", hint)
			}
			if got.Count < 2 {
				t.Fatalf("violations = %d, want both forbidden sections after the hint: %+v", got.Count, got.Violations)
			}
		}
	})

	// The last deterministic gate between a source clone and a published page.
	t.Run("credential material never reaches a published page", func(t *testing.T) {
		for _, body := range []string{
			"# Config\n\nDB_PASSWORD=SUPERSECRET_TOKEN_42\n",
			"# Config\n\n```yaml\npostgres:\n  password: PLAINTEXT_PG_PW_99\n```\n",
			"# Config\n\nJeton : ghp_" + strings.Repeat("a", 36) + "\n",
			"# Clé\n\n-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----\n",
		} {
			ws := t.TempDir()
			writeFile(t, ws, "docs/demo/p.md", body)
			var got lintOut
			runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
			if got.LintOK {
				t.Fatalf("credential material passed the publication gate: %q", body)
			}
		}
	})

	// A gate that read zero pages has certified nothing.
	t.Run("no product dir fails closed", func(t *testing.T) {
		ws := t.TempDir()
		var got lintOut
		runJSON(t, lintCommand(t, ws, "", allLintRules, ""), &got)
		if got.LintOK {
			t.Fatalf("the lint certified an artifact it never read")
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
	// The run base catalog_ingest recorded, before the campaign wrote a line.
	if ingest.BaseSHA == "" {
		t.Fatal("catalog_ingest recorded no run base: scope_check would refuse to certify the pass")
	}

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
		"vars.editorial_dir": ".product-docs",
		"input.product_dir":  productDir,
		"input.base_sha":     ingest.BaseSHA,
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
	// The run base is what catalog_ingest recorded BEFORE the campaign wrote
	// anything — never a base re-derived from the campaign's own commit
	// messages, which would let an un-trailered commit pick the window it is
	// audited on.
	run := func(t *testing.T, ws, productDir, base string) scopeOutPD {
		t.Helper()
		var got scopeOutPD
		runJSON(t, resolveCommand(t, command, map[string]string{
			"vars.workspace_dir": ws,
			"vars.editorial_dir": ".product-docs",
			"input.product_dir":  productDir,
			"input.base_sha":     base,
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
	// headOf is the run base as catalog_ingest records it: the docs repo HEAD
	// at the moment the run starts.
	headOf := func(t *testing.T, ws string) string {
		t.Helper()
		out, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("git rev-parse HEAD: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	prodyCommit := func(t *testing.T, ws, msg string) {
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", msg+"\n\nBot: product-docs\nProduct-Docs-Sources: demo-src@deadbeef")
	}

	t.Run("pages under the product dir are in scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, "documentation_produits/demo/gestionnaire/deposer.md", "# Déposer\n")
		prodyCommit(t, ws, "docs(demo): déposer")
		got := run(t, ws, "documentation_produits/demo", base)
		if !got.ScopeOK {
			t.Fatalf("a page under the product dir was flagged out of scope: %v", got.OutOfScope)
		}
	})

	t.Run("another product's pages are out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, "documentation_produits/autre/README.md", "# Autre (touché à tort)\n")
		prodyCommit(t, ws, "docs(autre): oops")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("a page belonging to ANOTHER product passed the writeable-set gate")
		}
		if !strings.Contains(got.Log, "documentation_produits/autre/README.md") {
			t.Fatalf("the fail_log does not name the offending path: %q", got.Log)
		}
	})

	t.Run("the docs repo's own editorial skills are out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, ".product-docs/modele.md", "# Modèle (réécrit par le bot)\n")
		prodyCommit(t, ws, "docs: oops")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("the bot rewrote the product team's own editorial line and the gate approved it")
		}
	})

	// A charter at the repo root is protected by the product-dir prefix rule
	// alone. The guard that MATTERS is the one for a charter the operator
	// puts INSIDE the product directory: a `.md` there passes every other
	// rule, and only the editorial exclusion keeps the bot from rewriting the
	// line it is governed by.
	t.Run("an editorial charter inside the product dir is still out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		editorial := "documentation_produits/demo/.product-docs"
		writeFile(t, ws, editorial+"/ton-et-style.md", "# Ton\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "chore: charte du produit")
		base := headOf(t, ws)
		writeFile(t, ws, editorial+"/ton-et-style.md", "# Ton (réécrit par le bot)\n")
		prodyCommit(t, ws, "docs(demo): ton")
		var got scopeOutPD
		runJSON(t, resolveCommand(t, command, map[string]string{
			"vars.workspace_dir": ws,
			"vars.editorial_dir": editorial,
			"input.product_dir":  "documentation_produits/demo",
			"input.base_sha":     base,
		}), &got)
		if got.ScopeOK {
			t.Fatalf("the bot rewrote the charter that governs it and the gate approved it")
		}
	})

	t.Run("a non-markdown file under the product dir is out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, "documentation_produits/demo/script.sh", "#!/bin/sh\n")
		prodyCommit(t, ws, "docs(demo): oops")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("a non-markdown file passed the writeable-set gate")
		}
	})

	t.Run("changes present before the run are not attributed to it", func(t *testing.T) {
		ws := newDocsRepo(t)
		// Someone else's code commit, landed BEFORE the run started — so it
		// sits below the base catalog_ingest records at run start.
		writeFile(t, ws, "tooling/build.sh", "#!/bin/sh\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "chore: not the bot's work")
		base := headOf(t, ws)
		// Then the run's own page commit.
		writeFile(t, ws, "documentation_produits/demo/gestionnaire/deposer.md", "# Déposer\n")
		prodyCommit(t, ws, "docs(demo): déposer")
		got := run(t, ws, "documentation_produits/demo", base)
		if !got.ScopeOK {
			t.Fatalf("pre-existing changes were attributed to the run: %v", got.OutOfScope)
		}
	})

	// The engine mirrors the bundle's skills into <workspace>/.claude/ at run
	// start and again on every resume. Reading that as the campaign's work
	// fails the gate on EVERY pass, and the campaign cannot fix it: the next
	// pass re-creates the tree. Observed live on run 01a03a6a, where a clean
	// 16-commit pass was reported as a scope violation on `.claude/`.
	t.Run("the engine's own skill mirror is not the campaign's work", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		// The mirror is UNTRACKED — the engine materialises it, the campaign
		// never commits it (it stages by path, not `git add -A`).
		writeFile(t, ws, ".claude/skills/product-docs.md", "# skill mirrored by the engine\n")
		writeFile(t, ws, "documentation_produits/demo/gestionnaire/deposer.md", "# Déposer\n")
		gitIn(t, ws, "add", "--", "documentation_produits/demo/gestionnaire/deposer.md")
		gitIn(t, ws, "commit", "-q", "-m", "docs(demo): déposer\n\nBot: product-docs\nProduct-Docs-Sources: demo-src@deadbeef")
		got := run(t, ws, "documentation_produits/demo", base)
		if !got.ScopeOK {
			t.Fatalf("the engine's skill mirror was attributed to the campaign: %v — every pass would fail a gate the agent cannot satisfy", got.OutOfScope)
		}
	})

	// The exclusion above must stay surgical: an untracked directory that is
	// NOT the engine's mirror is still a violation.
	t.Run("an untracked directory outside the product tree still fails", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, ".claude-notes/scratch.md", "# not the engine's tree\n")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("an untracked tree outside the product dir passed the writeable-set gate")
		}
	})

	// ...and once a path under .claude/ is TRACKED, the run committed the
	// agent's own notes into the docs repo: they ship in the PR, so they are
	// the campaign's work and in scope.
	t.Run("a committed .claude path is the campaign's work", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, ".claude/notes-du-bot.md", "# raisonnement brut\n")
		gitIn(t, ws, "add", "-f", ".claude/notes-du-bot.md")
		prodyCommit(t, ws, "docs(demo): oops")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("the run committed .claude/ into the docs repo and the gate approved it: the agent's raw notes would ship in the PR")
		}
	})

	// The base is RECORDED before the campaign writes. Deriving it from commit
	// messages let one un-trailered commit become the base and empty its own
	// diff — the audited party choosing its audit window.
	t.Run("an untrailered commit cannot redefine the run base", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, "src/app.py", "print('code touched by the bot')\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "docs(demo): page (no trailer)")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("a commit that simply omitted the trailer disabled the writeable-set gate: %v", got.OutOfScope)
		}
	})

	// Rename detection reports only a rename's DESTINATION, so moving a
	// protected file onto an allowed .md path would delete it behind a green
	// gate.
	t.Run("renaming a protected file onto an allowed path is a violation", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		gitIn(t, ws, "mv", ".product-docs/modele.md", "documentation_produits/demo/modele.md")
		prodyCommit(t, ws, "docs(demo): déplacement")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("the editorial charter was deleted by a rename and the gate approved it: %v", got.OutOfScope)
		}
	})

	// Writing THROUGH a symlink escapes the repository entirely, and
	// committing one publishes a host path in the docs.
	t.Run("a symlink under the product dir is out of scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		outside := filepath.Join(t.TempDir(), "cible-hors-depot.txt")
		if err := os.WriteFile(outside, []byte("contenu privé\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(ws, "documentation_produits/demo/note.md")); err != nil {
			t.Skipf("symlinks unavailable here: %v", err)
		}
		prodyCommit(t, ws, "docs(demo): note")
		got := run(t, ws, "documentation_produits/demo", base)
		if got.ScopeOK {
			t.Fatalf("a symlink passed the writeable-set gate: the campaign can write outside the repository")
		}
	})

	// git collapses an untracked DIRECTORY into a single entry that never
	// ends in .md — so a brand-new journey folder (the bootstrap case, and
	// every mid-page stop) used to red the gate on the very path the campaign
	// must write.
	t.Run("a new page in a new directory is in scope", func(t *testing.T) {
		ws := newDocsRepo(t)
		base := headOf(t, ws)
		writeFile(t, ws, "documentation_produits/demo/gestionnaire/deposer.md", "# Déposer\n")
		got := run(t, ws, "documentation_produits/demo", base)
		if !got.ScopeOK {
			t.Fatalf("an untracked page inside the product dir was flagged out of scope: %v", got.OutOfScope)
		}
	})

	// A gate that cannot compute its own window must say so, not certify.
	t.Run("no recorded base fails closed", func(t *testing.T) {
		ws := newDocsRepo(t)
		got := run(t, ws, "documentation_produits/demo", "")
		if got.ScopeOK {
			t.Fatalf("the gate certified a writeable set it could not compute")
		}
		if !strings.Contains(got.Log, "SCOPE GATE UNAVAILABLE") {
			t.Fatalf("the log does not name the missing base: %q", got.Log)
		}
	})
}

// TestProductDocsScopeCheckAcceptsAccentedPages pins core.quotePath: this
// bot's pages are FRENCH and their filenames come from UI wording. With
// git's default C-quoting, a committed "créer-un-dossier.md" comes back as
// a quoted octal-escaped string that never ends in .md — a permanent,
// unfixable scope violation on exactly the pages the bot exists to write.
func TestProductDocsScopeCheckAcceptsAccentedPages(t *testing.T) {
	requireGitPython(t)
	command := toolCommand(t, "product-docs/main.bot", "scope_check")

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "documentation_produits/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")
	baseOut, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	base := strings.TrimSpace(string(baseOut))

	// One accented page committed (the diff path), one still untracked
	// (the status -uall path) — both must stay in scope.
	writeFile(t, ws, "documentation_produits/demo/créer-un-dossier.md", "# Créer\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs(demo): créer\n\nBot: product-docs")
	writeFile(t, ws, "documentation_produits/demo/évolution.md", "# Évolution\n")

	var got scopeOutPD
	runJSON(t, resolveCommand(t, command, map[string]string{
		"vars.workspace_dir": ws,
		"vars.editorial_dir": ".product-docs",
		"input.product_dir":  "documentation_produits/demo",
		"input.base_sha":     base,
	}), &got)
	if !got.ScopeOK {
		t.Fatalf("accented page names were flagged out of scope: %v", got.OutOfScope)
	}
}

// TestProductDocsScopeCheckFailsClosedWhenGitFails pins the truth oracle's
// failure mode: a workspace git cannot answer for (not a repository, git
// missing, a timeout) must FAIL the gate with a reason — never certify
// scope_ok:true over a tree it did not diff.
func TestProductDocsScopeCheckFailsClosedWhenGitFails(t *testing.T) {
	requireGitPython(t)
	command := toolCommand(t, "product-docs/main.bot", "scope_check")

	ws := t.TempDir() // NOT a git repository
	var got scopeOutPD
	runJSON(t, resolveCommand(t, command, map[string]string{
		"vars.workspace_dir": ws,
		"vars.editorial_dir": ".product-docs",
		"input.product_dir":  "documentation_produits/demo",
		"input.base_sha":     "deadbeefdeadbeef",
	}), &got)
	if got.ScopeOK {
		t.Fatal("the gate certified a workspace whose git calls all failed")
	}
	if !strings.Contains(got.Log, "SCOPE GATE UNAVAILABLE") {
		t.Fatalf("the failure carries no reason: %q", got.Log)
	}
}

// TestProductDocsScanHintsIncrementalBootstrapsToFull pins the promised
// degradation: a FIRST incremental run has no alignment commit to diff
// since, and relaying mode=incremental with an empty delta tells the
// campaign "nothing changed" — the opposite of a bootstrap. The honest
// answer is a full sweep, said out loud in the note.
func TestProductDocsScanHintsIncrementalBootstrapsToFull(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed") // no Bot: product-docs trailer anywhere

	var got hintsOut
	runJSON(t, resolveCommand(t, toolCommand(t, "product-docs/main.bot", "scan_hints"), map[string]string{
		"vars.workspace_dir":  ws,
		"input.product_dir":   "docs/demo",
		"input.surfaces":      `[]`,
		"vars.editorial_dir":  ".product-docs",
		"vars.dismissed_path": "",
		"vars.max_hints":      "120",
		"vars.mode":           "incremental",
		"vars.diff_since":     "",
	}), &got)
	if got.Mode != "full" {
		t.Fatalf("a first incremental run relayed mode=%q with base %q — the campaign will read the empty delta as 'nothing changed'", got.Mode, got.IncrementalBase)
	}
	if !strings.Contains(got.HintsNote, "BOOTSTRAP") {
		t.Fatalf("the degradation is silent: %q", got.HintsNote)
	}
}

// TestProductDocsScanHintsIncrementalScopedToProduct pins the delta
// window's product identity: in a multi-product docs repo, a SIBLING
// product's newer alignment commit must not become this product's base —
// that narrows the window and hides real drift.
func TestProductDocsScanHintsIncrementalScopedToProduct(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	writeFile(t, ws, "docs/autre/README.md", "# Autre\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")
	// This product's alignment commit…
	writeFile(t, ws, "docs/demo/page.md", "# Page\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs(demo): align\n\nBot: product-docs")
	demoOut, _ := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	demoSHA := strings.TrimSpace(string(demoOut))
	// …then a SIBLING product's newer one, then drift on this product.
	writeFile(t, ws, "docs/autre/page.md", "# Autre page\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs(autre): align\n\nBot: product-docs")
	writeFile(t, ws, "docs/demo/drift.md", "# Drift\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "docs(demo): drift (human)")

	var got hintsOut
	runJSON(t, resolveCommand(t, toolCommand(t, "product-docs/main.bot", "scan_hints"), map[string]string{
		"vars.workspace_dir":  ws,
		"input.product_dir":   "docs/demo",
		"input.surfaces":      `[]`,
		"vars.editorial_dir":  ".product-docs",
		"vars.dismissed_path": "",
		"vars.max_hints":      "120",
		"vars.mode":           "incremental",
		"vars.diff_since":     "",
	}), &got)
	if got.IncrementalBase != demoSHA {
		t.Fatalf("the delta base is %q, not this product's own alignment %q — a sibling's commit narrowed the window", got.IncrementalBase, demoSHA)
	}
}

// TestProductDocsAskpassIsHostScoped pins the credential boundary: the
// forge token belongs to the docs repo's own origin, and the catalog is
// repo content — hostile-grade. The generated askpass helper must answer
// ONLY prompts naming the origin host; any other host (including a
// lookalike suffix domain) gets nothing and the clone fails loudly.
func TestProductDocsAskpassIsHostScoped(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	gitIn(t, ws, "remote", "add", "origin", "https://forge.example/team/docs.git")
	scratch := t.TempDir()
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\nname: Demo\nproduct_dir: docs/demo\nrepos:\n  - id: src\n    url: https://evil.invalid/src.git\n",
		`{"id":"demo","name":"Demo","product_dir":"docs/demo","repos":[{"id":"src","url":"https://evil.invalid/src.git"}]}`)
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	cmd := exec.Command("sh", "-c", ingestCommand(t, ws, "catalog", "demo", scratch))
	cmd.Env = append(os.Environ(), "GH_TOKEN=sekret-token-value",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	_, _ = cmd.Output() // the evil clone fails; the helper file is what we assert on

	helper := filepath.Join(scratch, "git-askpass.sh")
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("askpass helper was not written: %v", err)
	}
	run := func(prompt string) (string, error) {
		out, err := exec.Command("sh", helper, prompt).Output()
		return string(out), err
	}
	if out, err := run("Username for 'https://forge.example': "); err != nil || strings.TrimSpace(out) != "x-access-token" {
		t.Fatalf("the helper refused its own forge (out %q err %v)", out, err)
	}
	if out, err := run("Username for 'https://evil.invalid': "); err == nil {
		t.Fatalf("the helper answered a foreign host: %q", out)
	}
	if out, err := run("Password for 'https://x-access-token@evilforge.example': "); err == nil {
		t.Fatalf("the helper answered a lookalike suffix domain: %q", out)
	}
}

// TestProductDocsInventoryStripsInlineURLCredentials pins the one leak
// path strip_creds did not cover: git echoes the CATALOG's own inline
// credential (https://user:pass@host/…) verbatim in its clone errors, and
// that note reaches the campaign prompt, the events and the PR body.
func TestProductDocsInventoryStripsInlineURLCredentials(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	gitIn(t, ws, "remote", "add", "origin", "https://forge.example/team/docs.git")
	scratch := t.TempDir()
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\nname: Demo\nproduct_dir: docs/demo\nrepos:\n  - id: src\n    url: https://leaky:hunter2@evil.invalid/src.git\n",
		`{"id":"demo","name":"Demo","product_dir":"docs/demo","repos":[{"id":"src","url":"https://leaky:hunter2@evil.invalid/src.git"}]}`)
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	blob, _ := json.Marshal(got.Inventory)
	if strings.Contains(string(blob), "hunter2") || strings.Contains(string(blob), "leaky:") {
		t.Fatalf("an inline url credential survived into the inventory: %s", blob)
	}
}

// TestProductDocsCatalogIngestRefusesCollidingIDs pins the clone-dir
// identity: two catalog ids that collide AFTER sanitisation (a/b and
// a-b both become a-b) would share one destination — the second
// rmtree-and-reclones over the first and the campaign reads the wrong
// repository under the first id. A collision is a catalog bug: refuse
// it loudly, never swap code silently.
func TestProductDocsCatalogIngestRefusesCollidingIDs(t *testing.T) {
	requireGitPython(t)

	source := newSourceRepo(t)
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	scratch := t.TempDir()
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\nname: Demo\nproduct_dir: docs/demo\nrepos:\n"+
			"  - id: a/b\n    url: "+source+"\n"+
			"  - id: a-b\n    url: "+source+"\n",
		`{"id":"demo","name":"Demo","product_dir":"docs/demo","repos":[{"id":"a/b","url":"`+source+`"},{"id":"a-b","url":"`+source+`"}]}`)
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	runExpectingFailure(t, ingestCommand(t, ws, "catalog", "demo", scratch), "collide after sanitisation")
}

// TestProductDocsSourceStampRecordsFullSHA pins the stamp's fetchability:
// the next incremental run repairs a shallow clone by fetching the
// stamped sha (`git fetch --depth 1 origin <prev>`), and git can only
// fetch a FULL object name. An abbreviated stamp made that repair
// permanently impossible — every incremental delta came back
// unavailable.
func TestProductDocsSourceStampRecordsFullSHA(t *testing.T) {
	requireGitPython(t)

	source := newSourceRepo(t)
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	scratch := t.TempDir()
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\nname: Demo\nproduct_dir: docs/demo\nrepos:\n  - id: demo-src\n    url: "+source+"\n",
		`{"id":"demo","name":"Demo","product_dir":"docs/demo","repos":[{"id":"demo-src","url":"`+source+`"}]}`)
	writeFile(t, ws, "docs/demo/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	parts := strings.SplitN(got.Stamp, "@", 2)
	if len(parts) != 2 || len(parts[1]) != 40 {
		t.Fatalf("sources_stamp %q does not record a full 40-char sha — the shallow-delta repair cannot fetch it", got.Stamp)
	}
}

// TestProductDocsPublishGate pins the publish tail's deterministic
// pre-flight, executing the real node body: the tail opens only on the
// explicit opt-in + a base URL + a named image repository + both
// credentials present at their TEMPLATE-resolved paths (host and sandbox
// runs resolve differently; a hardcoded mount path silently disabled
// publishing on host runs) + the operator's deploy-target playbook
// actually mirrored into the workspace.
func TestProductDocsPublishGate(t *testing.T) {
	requireGitPython(t)
	command := toolCommand(t, "product-docs/main.bot", "publish_gate")

	secretsDir := t.TempDir()
	paths := map[string]string{}
	for _, n := range []string{"deploy_credential", "registry_token"} {
		p := filepath.Join(secretsDir, n)
		if err := os.WriteFile(p, []byte("v"), 0o600); err != nil {
			t.Fatalf("write secret fixture: %v", err)
		}
		paths[n] = p
	}
	// A workspace with the deploy-target skill in one of the two shapes the
	// engine mirrors: <name>/SKILL.md (skill library, pkg/runtime/library_skills.go)
	// and the flat <name>.md (plugin contribution, pkg/runtime/contributions.go).
	// `rel` empty seeds no skill at all.
	workspace := func(t *testing.T, rel string) string {
		t.Helper()
		ws := t.TempDir()
		if rel == "" {
			return ws
		}
		p := filepath.Join(ws, ".claude", "skills", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("seed deploy-target skill: %v", err)
		}
		if err := os.WriteFile(p, []byte("# deploy-target\n"), 0o644); err != nil {
			t.Fatalf("seed deploy-target skill: %v", err)
		}
		return ws
	}
	wsReady := workspace(t, "deploy-target/SKILL.md")
	wsFlat := workspace(t, "deploy-target.md")
	wsBare := workspace(t, "")

	// The gate probes the RUN's cwd as well as vars.workspace_dir (a tool node
	// executes in the run workDir, which is also where the engine mirrors). Pin
	// the process cwd to an empty directory so only the workspace_dir half is
	// under test and the repo's own tree cannot answer for it.
	emptyCwd := t.TempDir()
	run := func(t *testing.T, publish, base, image, missing, ws string) publishGateOut {
		t.Helper()
		refs := map[string]string{
			"vars.publish":                   publish,
			"vars.publish_base_url":          base,
			"vars.publish_image":             image,
			"vars.workspace_dir":             ws,
			"secrets.deploy_credential.path": paths["deploy_credential"],
			"secrets.registry_token.path":    paths["registry_token"],
		}
		if missing != "" {
			refs["secrets."+missing+".path"] = filepath.Join(secretsDir, "absent")
		}
		var got publishGateOut
		runJSON(t, "cd "+shQuote(emptyCwd)+" && "+resolveCommand(t, command, refs), &got)
		return got
	}

	if got := run(t, "true", "https://docs.example", "registry.example/org/prody-demo", "", wsReady); !got.DoPublish || got.Reason != "ready" {
		t.Fatalf("all preconditions met yet the gate refused: %+v", got)
	}
	if got := run(t, "true", "https://docs.example", "registry.example/org/prody-demo", "", wsFlat); !got.DoPublish || got.Reason != "ready" {
		t.Fatalf("a plugin-contributed deploy-target.md is the shape app-dev and review-env document — the gate must accept it: %+v", got)
	}
	if got := run(t, "false", "https://docs.example", "registry.example/org/prody-demo", "", wsReady); got.DoPublish || !strings.Contains(got.Reason, "disabled") {
		t.Fatalf("publish=false must route the tail out with its reason: %+v", got)
	}
	if got := run(t, "true", "", "registry.example/org/prody-demo", "", wsReady); got.DoPublish || !strings.Contains(got.Reason, "publish_base_url") {
		t.Fatalf("an empty base URL must refuse the tail: %+v", got)
	}
	if got := run(t, "true", "https://docs.example", "", "", wsReady); got.DoPublish || !strings.Contains(got.Reason, "image") {
		t.Fatalf("an empty image repository must refuse the tail (no default names a deployment): %+v", got)
	}
	// The gate is platform-agnostic: it names the CREDENTIAL that is missing,
	// never a platform, and refuses with either one absent.
	for _, missing := range []string{"deploy_credential", "registry_token"} {
		if got := run(t, "true", "https://docs.example", "registry.example/org/prody-demo", missing, wsReady); got.DoPublish || !strings.Contains(got.Reason, missing) {
			t.Fatalf("a missing %s must be named in the refusal: %+v", missing, got)
		}
	}
	// `skills:` is a SOFT reference — the runtime skips one it cannot resolve
	// with a log line and never fails the run. Without this probe the publish
	// agent enters with a registry-write token, a cluster credential and no
	// platform playbook, and improvises a deployment for 60 steps.
	if got := run(t, "true", "https://docs.example", "registry.example/org/prody-demo", "", wsBare); got.DoPublish || !strings.Contains(got.Reason, "deploy-target") {
		t.Fatalf("an unattached deploy-target playbook must refuse the tail, naming it: %+v", got)
	}
}

type publishGateOut struct {
	DoPublish bool   `json:"do_publish"`
	Reason    string `json:"reason"`
}

// TestProductDocsVerifyPublish pins the external truth gate: it verifies
// THE OPERATOR'S deployment — a 200 with a title under publish_base_url —
// never whatever live URL the agent happened to narrate.
func TestProductDocsVerifyPublish(t *testing.T) {
	requireGitPython(t)
	command := toolCommand(t, "product-docs/main.bot", "verify_publish")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/site/" {
			_, _ = w.Write([]byte("<html><head><title>Demo</title></head></html>"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var got verifyPublishOut
	runJSON(t, resolveCommand(t, command, map[string]string{
		"input.url":      srv.URL + "/site/",
		"input.base_url": srv.URL,
	}), &got)
	if !got.Verified {
		t.Fatalf("a live page under the base was not verified: %+v", got)
	}

	runExpectingFailure(t, resolveCommand(t, command, map[string]string{
		"input.url":      srv.URL + "/site/",
		"input.base_url": "https://elsewhere.example",
	}), "not under publish_base_url")
}

type verifyPublishOut struct {
	Verified bool   `json:"verified"`
	URL      string `json:"url"`
	Detail   string `json:"detail"`
}

// TestGitbookToMkdocsBlankLineStep pins the converter against the crash
// Revi reproduced: a blank line between {% step %} and its heading made
// the title promotion index into the wrong output line (ValueError).
func TestGitbookToMkdocsBlankLineStep(t *testing.T) {
	requireGitPython(t)
	script := filepath.Join("product-docs", "deploy", "gitbook_to_mkdocs.py")
	py := `
import importlib.util
spec = importlib.util.spec_from_file_location('g', '` + script + `')
g = importlib.util.module_from_spec(spec); spec.loader.exec_module(g)
out = g.convert_page('# P\n\n{% stepper %}\n{% step %}\n\n### Titre\ncontenu\n{% endstep %}\n{% endstepper %}\n', 't.md')
assert 'Étape 1 — Titre' in out, out
print('ok')
`
	cmd := exec.Command("python3", "-c", py)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("converter crashed on blank-line step: %v\n%s", err, out)
	}
}
