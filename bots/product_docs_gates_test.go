package bots

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
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
	return gittest.Run(t, dir, args...)
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

// seedSourceRepo fills dir with a throwaway git repository standing in for one
// of the product's source repos, carrying a user-facing i18n catalog
// (functional signal) and a credential file (which must never survive into the
// clone).
func seedSourceRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "locales/fr.json", `{"submit":"Envoyer","status":"En instruction"}`+"\n")
	writeFile(t, dir, ".env", "SECRET_KEY=never-read-me\n")
	writeFile(t, dir, "config/app-secrets.yaml", "token: never-read-me\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "seed")
	return dir
}

// newSourceRepo stands a source repository OUTSIDE any workspace — the shape a
// catalog may name but no longer reach.
func newSourceRepo(t *testing.T) string {
	t.Helper()
	return seedSourceRepo(t, t.TempDir())
}

// localSourceRel is where the fixtures stand a source repository that the
// catalog names by the FILESYSTEM. A value on disk is a local source whichever
// key carries it — `url` or `path` — so it is confined to the docs workspace,
// and a fixture standing one outside would exercise the refusal and nothing
// else.
const localSourceRel = "vendor/src"

// newLocalSource seeds that repository inside ws and returns its absolute path.
func newLocalSource(t *testing.T, ws string) string {
	t.Helper()
	return seedSourceRepo(t, filepath.Join(ws, localSourceRel))
}

type ingestOut struct {
	OraclePath  string           `json:"oracle_path"`
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
		"vars.oracle_dir":    ".golden-master",
	})
}

// TestProductDocsCatalogIngest pins the front door's four promises: it
// resolves the product from the catalog, it clones the sources OUT of the
// docs worktree, it strips credential files from those clones, and a repo it
// cannot read becomes a visible `degraded` entry instead of a silent absence.
func TestProductDocsCatalogIngest(t *testing.T) {
	requireGitPython(t)

	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source := newLocalSource(t, ws)
	// A local source the workspace holds but that is NOT a repository: a
	// visible `degraded` entry, never a silent absence.
	missing := filepath.Join(ws, "vendor", "does-not-exist")
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
	if note, _ := badEntry["note"].(string); !strings.Contains(note, "is not a git repository") {
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

	// The RELATIVE form of a local source: a url that resolves on disk is the
	// same filesystem read as a path, and takes the same confinement.
	source := localSourceRel

	newWS := func(t *testing.T) (string, string) {
		t.Helper()
		ws := t.TempDir()
		scratch := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		newLocalSource(t, ws)
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

	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source := newLocalSource(t, ws)
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

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source := newLocalSource(t, ws)
	firstSHA := strings.TrimSpace(gitIn(t, source, "rev-parse", "HEAD"))
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
	// A net is present by default: that is what arms the marker exemption.
	return lintCommandWithNet(t, ws, productDir, rules, extra, filepath.Join(ws, ".golden-master"))
}

// lintCommandWithNet exposes the one input that decides whether the anchorless
// marker is exempt at all. The exemption exists only to stop this gate
// contradicting coverage_check, and coverage_check is armed by a NET — so an
// empty oracle_path must give back the node as it was before either existed.
func lintCommandWithNet(t *testing.T, ws, productDir, rules, extra, oraclePath string) string {
	t.Helper()
	return resolveCommand(t, toolCommand(t, "product-docs/main.bot", "page_lint"), map[string]string{
		"vars.workspace_dir":             ws,
		"input.product_dir":              productDir,
		"input.oracle_path":              oraclePath,
		"vars.lint_rules":                rules,
		"vars.extra_forbidden_headings":  extra,
		"vars.coverage_no_anchor_marker": defaultNoAnchorMarker,
	})
}

// The declared identifiers the exhaustiveness gate matches on, in their
// shipped defaults. They are vars because the gate is language-neutral while
// the pages are not.
const (
	defaultNoAnchorMarker  = "<!--no-anchor-->"
	defaultExclusionsToken = "exclusions"
	defaultPlaceholders    = "TODO,FIXME,TBD,XXX,WIP,lorem ipsum"
)

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

	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	source := newLocalSource(t, ws)
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
		return gittest.Run(t, ws, "rev-parse", "HEAD")
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
	base := gittest.Run(t, ws, "rev-parse", "HEAD")

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
	demoSHA := gittest.Run(t, ws, "rev-parse", "HEAD")
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

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	scratch := t.TempDir()
	source := newLocalSource(t, ws)
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

	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	scratch := t.TempDir()
	source := newLocalSource(t, ws)
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
		"input.deployed": "true",
		"input.summary":  "built and deployed",
	}), &got)
	if !got.Verified {
		t.Fatalf("a live page under the base was not verified: %+v", got)
	}

	runExpectingFailure(t, resolveCommand(t, command, map[string]string{
		"input.url":      srv.URL + "/site/",
		"input.base_url": "https://elsewhere.example",
		"input.deployed": "true",
		"input.summary":  "built and deployed",
	}), "not under publish_base_url")
}

// TestProductDocsVerifyPublishRedirect pins the half of "verifies THE
// OPERATOR'S deployment" that the requested-url check alone cannot carry:
// urllib follows redirects silently, so an in-scope URL that 302s to an SSO
// login, a platform error page or an unrelated host used to answer 200 with a
// <title> and pass. Under the retired S3 static target a redirect was
// implausible; under an arbitrary operator ingress it is the common shape.
func TestProductDocsVerifyPublishRedirect(t *testing.T) {
	requireGitPython(t)
	command := toolCommand(t, "product-docs/main.bot", "verify_publish")

	// Stands in for whatever the deployment redirects AT — an SSO portal, a
	// platform 404, another tenant's app. It answers 200 with a title, which is
	// the whole point: the content heuristic cannot tell it apart.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>Sign in</title></head></html>"))
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/off/":
			http.Redirect(w, r, elsewhere.URL+"/login", http.StatusFound)
		case "/deep/", "/deep/index.html":
			// An in-scope redirect (the trailing-slash / index shape a healthy
			// static host performs) must still verify.
			if r.URL.Path == "/deep/" {
				http.Redirect(w, r, "/deep/index.html", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte("<html><head><title>Demo</title></head></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	runExpectingFailure(t, resolveCommand(t, command, map[string]string{
		"input.url":      srv.URL + "/off/",
		"input.base_url": srv.URL,
		"input.deployed": "true",
		"input.summary":  "built and deployed",
	}), "outside publish_base_url")

	var got verifyPublishOut
	runJSON(t, resolveCommand(t, command, map[string]string{
		"input.url":      srv.URL + "/deep/",
		"input.base_url": srv.URL,
		"input.deployed": "true",
		"input.summary":  "built and deployed",
	}), &got)
	if !got.Verified {
		t.Fatalf("a redirect that stays under the operator's base is how a healthy "+
			"host serves an index — it must not be refused: %+v", got)
	}
}

// TestProductDocsVerifyPublishCarriesTheAgentsReason pins the failure DETAIL
// on the path the branch made common. The publish agent is instructed to stop
// and report — no deploy-target playbook, a rejected registry token, an image
// the cluster cannot pull, a host outside the base URL — and every one of those
// arrives here as an empty url. Reporting that as "returned no URL" throws away
// the remedy the agent just worked out and points the operator at the wrong
// thing. The run must still FAIL (publish was asked for and did not happen).
func TestProductDocsVerifyPublishCarriesTheAgentsReason(t *testing.T) {
	requireGitPython(t)
	command := toolCommand(t, "product-docs/main.bot", "verify_publish")

	// Multi-line, quote-bearing agent prose: it reaches the shell through a
	// template ref, so this also pins that the value stays one shell word.
	const summary = "ImagePullBackOff: the ghcr package is PRIVATE.\nRemedy: flip it public once, then re-run."
	runExpectingFailure(t, resolveCommand(t, command, map[string]string{
		"input.url":      "",
		"input.base_url": "https://docs.example",
		"input.deployed": "false",
		"input.summary":  summary,
	}), "ImagePullBackOff")
	runExpectingFailure(t, resolveCommand(t, command, map[string]string{
		"input.url":      "",
		"input.base_url": "https://docs.example",
		"input.deployed": "false",
		"input.summary":  summary,
	}), "deployed=false")
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

// ─── coverage_check — the EXHAUSTIVENESS gate ────────────────────────────
//
// Prody's convergence used to rest on the campaign's own `docs_aligned` for
// the one claim an agent cannot honestly make about itself: is EVERYTHING
// documented, and does everything documented EXIST? coverage_check answers it
// from the product's golden-master net — `feature-coverage.json` (covered
// features with the corpus entries that exercise them, exclusions with their
// written reason) and `corpus.json` (the captured entries).
//
// A gate nobody has seen red proves nothing, so the falsification suite below
// breaks the fixture once per refusal cause and requires the gate to redden
// FOR THAT NAMED CAUSE — a red for another reason would prove nothing either.
// The fixture is synthetic and the vocabulary is its own: nothing here assumes
// three-digit entry ids, which is one campaign's convention.

type coverageOut struct {
	OK            bool             `json:"coverage_ok"`
	NetPresent    bool             `json:"net_present"`
	Causes        []map[string]any `json:"causes"`
	CauseCount    int              `json:"cause_count"`
	Total         int              `json:"features_total"`
	Documented    int              `json:"features_documented"`
	Exclusions    int              `json:"exclusions_total"`
	Named         int              `json:"exclusions_named"`
	Chapters      int              `json:"chapters"`
	Anchorless    int              `json:"chapters_unanchored_declared"`
	AnchorlessMax int              `json:"chapters_anchorless_max"`
	NetUnproven   bool             `json:"net_unproven"`
	CountsLine    string           `json:"counts_line"`
	RoutesTotal   int              `json:"routes_declared"`
	Degraded      bool             `json:"routes_degraded"`
	OracleUsed    string           `json:"oracle_dir_used"`
	Log           string           `json:"log"`
}

// shippedAnchorlessCeiling is the DEFAULT the bundle ships. The fixtures run
// at it, deliberately: a bench that has to loosen the shipped ceiling to pass
// is a bench measuring a product nobody runs.
const shippedAnchorlessCeiling = "2"

func coverageCommand(t *testing.T, ws, productDir, oraclePath string) string {
	t.Helper()
	return coverageCommandWith(t, ws, productDir, oraclePath, defaultExclusionsToken)
}

// coverageCommandWith exposes the declared exclusions-chapter token: the pages
// are not written in the gate's language.
func coverageCommandWith(t *testing.T, ws, productDir, oraclePath, exclToken string) string {
	t.Helper()
	return coverageCommandCapped(t, ws, productDir, oraclePath, exclToken, shippedAnchorlessCeiling)
}

func coverageCommandCapped(t *testing.T, ws, productDir, oraclePath, exclToken, maxAnchorless string) string {
	t.Helper()
	return coverageCommandFull(t, ws, productDir, oraclePath, exclToken, maxAnchorless, citeOpen, citeClose, "routes.txt")
}

func coverageCommandFull(t *testing.T, ws, productDir, oraclePath, exclToken, maxAnchorless, open, close, routes string) string {
	t.Helper()
	return resolveCommand(t, toolCommand(t, "product-docs/main.bot", "coverage_check"), map[string]string{
		"vars.workspace_dir":               ws,
		"input.product_dir":                productDir,
		"input.oracle_path":                oraclePath,
		"vars.coverage_exclusions_heading": exclToken,
		"vars.coverage_no_anchor_marker":   defaultNoAnchorMarker,
		"vars.coverage_routes_file":        routes,
		"vars.coverage_citation_open":      open,
		"vars.coverage_citation_close":     close,
		"vars.coverage_placeholders":       defaultPlaceholders,
		"vars.coverage_min_prose":          "60",
		"vars.coverage_max_anchorless":     maxAnchorless,
	})
}

// The shipped citation syntax. A reference is what the page SAYS is one; a
// code span is prose, whatever it looks like.
const (
	citeOpen  = "[[ref:"
	citeClose = "]]"
)

// ref writes a citation the way a page does.
func ref(token string) string { return citeOpen + token + citeClose }

const (
	coverageCorpus = `{"entries": [
  {"id": "001", "persona": "anon", "method": "GET", "path": "/", "surface": "http"},
  {"id": "026", "persona": "manager", "method": "GET", "path": "/dashboard/items", "surface": "http"},
  {"id": "027", "persona": "manager", "method": "GET", "path": "/dashboard/items?page=2", "surface": "http"},
  {"id": "039", "persona": "manager", "method": "GET", "path": "/dashboard/items/42", "surface": "http"}
]}`
	coverageInventory = `{"features": [
  {"feature": "home.landing", "entries": ["001"]},
  {"feature": "items.list", "entries": ["026"]},
  {"feature": "items.list.paging", "entries": ["027"]},
  {"feature": "items.detail", "entries": ["039"]}
 ],
 "exclusions": [
  {"feature": "menu.logout",
   "reason": "session teardown, not a screen: measured by the ops runbook, not this net"}
 ]}`
	coverageRoutes = "GET /\nGET /dashboard/items\nGET /dashboard/items/{id}\n# a comment line\nGET /{slug}\n"
)

// coverageHome and coverageExclusions are the INTACT documentation the gate
// must bless: every covered feature anchored on one of its own entries, every
// exclusion named under a chapter that says so, every chapter carrying a
// reference or declaring it carries none.
const (
	coverageHome = "# The product\n" +
		"\n" +
		"## The landing page — [[ref:/]] [[ref:001]]\n" +
		"\n" +
		"[[ref:home.landing]] [[ref:001]] is the first screen a visitor sees: it\n" +
		"carries the sign-in call to action and nothing else.\n" +
		"\n" +
		"## The item list — [[ref:/dashboard/items]] [[ref:026]]\n" +
		"\n" +
		"The manager lands here — [[ref:items.list]] [[ref:026]] — and reads twenty rows,\n" +
		"newest first, each one showing its owner, its status and its last change.\n" +
		"Paging is [[ref:items.list.paging]] [[ref:027]], twenty rows per page\n" +
		"([[ref:/dashboard/items?page=2]]), with a pager at the foot of the list to move between them.\n" +
		"\n" +
		"## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]\n" +
		"\n" +
		"[[ref:items.detail]] [[ref:039]] shows one item in full: every field the\n" +
		"manager filled in, the history of its changes and the actions still open\n" +
		"to them.\n" +
		"\n" +
		"## How this page was built <!--no-anchor-->\n" +
		"\n" +
		"A method chapter observes nothing and says so.\n"
	coverageExclusionsPage = "# What this documentation does not cover\n" +
		"\n" +
		"## Exclusions\n" +
		"\n" +
		"- [[ref:menu.logout]] — signing out tears the session down without showing a " +
		"screen of its own, so the net never captures it and this documentation " +
		"does not describe it.\n"
)

// newCoverageFixture writes a synthetic product: a golden-master net and the
// documentation that satisfies it.
func newCoverageFixture(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	writeFile(t, ws, ".golden-master/corpus.json", coverageCorpus)
	writeFile(t, ws, ".golden-master/feature-coverage.json", coverageInventory)
	writeFile(t, ws, ".golden-master/routes.txt", coverageRoutes)
	writeFile(t, ws, "docs/demo/README.md", coverageHome)
	writeFile(t, ws, "docs/demo/exclusions.md", coverageExclusionsPage)
	return ws
}

func runCoverage(t *testing.T, ws string) coverageOut {
	t.Helper()
	var got coverageOut
	runJSON(t, coverageCommand(t, ws, "docs/demo", filepath.Join(ws, ".golden-master")), &got)
	return got
}

// mutate rewrites a fixture file, refusing to proceed when the anchor it aims
// at has drifted: a mutation that silently lands elsewhere would declare the
// gate alive for nothing.
func mutate(t *testing.T, ws, rel, from, to string) {
	t.Helper()
	p := filepath.Join(ws, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if n := strings.Count(s, from); n != 1 {
		t.Fatalf("mutation anchor %q occurs %d times in %s, want exactly 1", from, n, rel)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(s, from, to, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProductDocsCoverageGateBlessesAnExhaustiveDocumentation is the positive
// control every falsification needs: without it a gate that refuses
// everything would score a perfect falsification run.
func TestProductDocsCoverageGateBlessesAnExhaustiveDocumentation(t *testing.T) {
	requireGitPython(t)
	got := runCoverage(t, newCoverageFixture(t))
	if !got.OK {
		t.Fatalf("the gate refused an exhaustive documentation:\n%s", got.Log)
	}
	if !got.NetPresent {
		t.Fatalf("net_present = false with both artifacts committed")
	}
	if got.Documented != 4 || got.Total != 4 || got.Named != 1 || got.Exclusions != 1 {
		t.Fatalf("counts = %d/%d features, %d/%d exclusions, want 4/4 and 1/1", got.Documented, got.Total, got.Named, got.Exclusions)
	}
	if got.Degraded || got.RoutesTotal != 4 {
		t.Fatalf("routes: degraded=%v declared=%d, want false and 4 (the comment line is not a route)", got.Degraded, got.RoutesTotal)
	}
	if got.Anchorless != 1 {
		t.Fatalf("declared-anchorless chapters = %d, want 1 — the exclusions chapter is anchored by its ROLE and needs no marker, the method chapter is the one that declares", got.Anchorless)
	}
	if !strings.Contains(got.Log, "chapter declared anchorless") {
		t.Fatalf("the log does not name the chapters that declared themselves anchorless:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateFalsification breaks the fixture once per
// refusal cause and requires the gate to redden FOR THAT CAUSE.
func TestProductDocsCoverageGateFalsification(t *testing.T) {
	requireGitPython(t)
	cases := []struct {
		name string
		want string
		// preflight: the repair lands OUTSIDE the writeable set
		// (`<product_dir>/**/*.md`), so the node must stop the RUN by name
		// rather than hand the campaign an order it is forbidden to obey.
		preflight bool
		sabotage  func(t *testing.T, ws string)
	}{
		{
			// A feature the net inventories and the pages no longer carry.
			name: "GAP: a covered feature drops out of the documentation",
			want: "GAP -- items.list.paging",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/README.md",
					"Paging is [[ref:items.list.paging]] [[ref:027]], twenty rows per page\n", "")
			},
		},
		{
			// Citing the id and citing an entry is not enough if they are not
			// on the SAME line: the anchor is the pairing, not the presence.
			name: "GAP: the feature and its entry drift onto separate lines",
			want: "GAP -- items.detail",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/README.md",
					"[[ref:items.detail]] [[ref:039]] shows one item in full:",
					"[[ref:items.detail]] shows one item in full:\nIts reference is [[ref:039]],")
			},
		},
		{
			name: "CONCEALED_EXCLUSION: the exclusion is no longer named at all",
			want: "CONCEALED_EXCLUSION -- menu.logout",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/exclusions.md", "- [[ref:menu.logout]] — signing out", "- signing out")
			},
		},
		{
			name: "CONCEALED_EXCLUSION: a bare identifier is silence under a label",
			want: "CONCEALED_EXCLUSION -- menu.logout",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/exclusions.md",
					"- [[ref:menu.logout]] — signing out tears the session down without showing a "+
						"screen of its own, so the net never captures it and this documentation "+
						"does not describe it.",
					"- [[ref:menu.logout]] — see above.")
			},
		},
		{
			name: "CONCEALED_EXCLUSION: a substitute is not writing",
			want: "CONCEALED_EXCLUSION -- menu.logout",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/exclusions.md",
					"- [[ref:menu.logout]] — signing out tears the session down without showing a "+
						"screen of its own, so the net never captures it and this documentation "+
						"does not describe it.",
					"- [[ref:menu.logout]] — TODO: write down why this one sits outside the perimeter, later on.")
			},
		},
		{
			// The exclusion is named, with prose, OUTSIDE the declared
			// chapter: mentioning a hole is not declaring it.
			name: "CONCEALED_EXCLUSION: named outside the declared exclusions chapter",
			want: "CONCEALED_EXCLUSION -- menu.logout",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/exclusions.md", "## Exclusions",
					"## Good to know <!--no-anchor-->")
			},
		},
		{
			name: "PHANTOM_DOC: an invented corpus entry",
			want: "the reference 999 is CITED and",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/README.md", "twenty rows per page", "twenty rows per page ([[ref:999]])")
			},
		},
		{
			name: "PHANTOM_DOC: an inventory id nobody inventoried",
			want: "the reference items.invented is CITED and",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/README.md", "the actions still open",
					"the actions [[ref:items.invented]] still open")
			},
		},
		{
			// In a TITLE: the least-read line of the document.
			name: "PHANTOM_DOC: a path the application does not serve, in a title",
			want: "the path /dashboard/invented is NEITHER a declared route NOR a corpus entry path",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/README.md", "## The item list — [[ref:/dashboard/items]] [[ref:026]]",
					"## The item list — [[ref:/dashboard/invented]] [[ref:026]]")
			},
		},
		{
			name: "PHANTOM_DOC: a filter the net never exercises",
			want: "the parameter sort of /dashboard/items?sort=name is observed on NO corpus entry",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/README.md", "([[ref:/dashboard/items?page=2]])",
					"([[ref:/dashboard/items?page=2]], [[ref:/dashboard/items?sort=name]])")
			},
		},
		{
			name: "UNANCHORED_CHAPTER: a title stripped of every reference",
			want: "UNANCHORED_CHAPTER",
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]", "## One item")
			},
		},
		{
			// THE EMPTY IS A NAMED REFUSAL. An `if collection and …` guard is
			// disabled on an empty collection; each emptiness below is decided
			// out loud instead.
			name: "NET_UNREADABLE: the documentation is emptied",
			want: "NO markdown file",
			sabotage: func(t *testing.T, ws string) {
				// Moved aside, not deleted: the product tree the gate reads
				// is left holding no page.
				if err := os.Rename(filepath.Join(ws, "docs/demo"), filepath.Join(ws, "docs/moved-aside")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(ws, "docs/demo"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:      "NET_UNREADABLE: the inventory declares no feature",
			want:      "features is absent, empty or not a list",
			preflight: true,
			sabotage: func(t *testing.T, ws string) {
				writeFile(t, ws, ".golden-master/feature-coverage.json", `{"features": [], "exclusions": []}`)
			},
		},
		{
			// An ABSENT key is an empty list (the producer reads it that way);
			// a key that is THERE and mistyped is the refusal.
			name:      "NET_UNREADABLE: the inventory declares its holes in an unreadable shape",
			want:      "exclusions is present and is not a list",
			preflight: true,
			sabotage: func(t *testing.T, ws string) {
				writeFile(t, ws, ".golden-master/feature-coverage.json",
					`{"features": [{"feature": "home.landing", "entries": ["001"]}], "exclusions": {"menu.logout": "gone"}}`)
			},
		},
		{
			name:      "NET_UNREADABLE: the corpus is emptied",
			want:      "entries is absent, empty or not a list",
			preflight: true,
			sabotage: func(t *testing.T, ws string) {
				writeFile(t, ws, ".golden-master/corpus.json", `{"entries": []}`)
			},
		},
		{
			name:      "NET_UNREADABLE: an exclusion loses its reason",
			want:      "EMPTY reason in the inventory",
			preflight: true,
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, ".golden-master/feature-coverage.json",
					`"reason": "session teardown, not a screen: measured by the ops runbook, not this net"`,
					`"reason": "   "`)
			},
		},
		{
			name:      "NET_UNREADABLE: the route table declares nothing",
			want:      "the declared route table is EMPTY",
			preflight: true,
			sabotage: func(t *testing.T, ws string) {
				writeFile(t, ws, ".golden-master/routes.txt", "# every route was commented out\n")
			},
		},
		{
			name:      "NET_UNREADABLE: the inventory both covers and excludes a feature",
			want:      "BOTH covered and excluded",
			preflight: true,
			sabotage: func(t *testing.T, ws string) {
				mutate(t, ws, ".golden-master/feature-coverage.json", `"feature": "menu.logout"`,
					`"feature": "items.detail"`)
			},
		},
		{
			// A route made of placeholders only matches every path and proves
			// none: for a path only such a route covers, the corpus is the
			// only evidence. The rule is CHECKED, not merely commented.
			name: "PHANTOM_DOC: a framework catch-all route proves nothing",
			want: "the path /dashboard/invented is NEITHER a declared route NOR a corpus entry path",
			sabotage: func(t *testing.T, ws string) {
				writeFile(t, ws, ".golden-master/routes.txt",
					"GET /\nGET /dashboard/items\nGET /dashboard/items/{id}\nGET /{slug}\nGET /**\n")
				mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]",
					"## One item — [[ref:/dashboard/invented]] [[ref:039]]")
			},
		},
		{
			// ONE predicate governs both sides. A DECLARED tail wildcard
			// carries a literal segment, so it clears the placeholders-only
			// rule — and still matches every screen below it while describing
			// none. What the gate refuses a page to cite, it refuses a route
			// table to prove.
			name: "PHANTOM_DOC: a declared tail wildcard proves nothing either",
			want: "the path /dashboard/invented is NEITHER a declared route NOR a corpus entry path",
			sabotage: func(t *testing.T, ws string) {
				writeFile(t, ws, ".golden-master/routes.txt",
					"GET /\nGET /dashboard/items\nGET /dashboard/items/{id}\nGET /dashboard/**\n")
				mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]",
					"## One item — [[ref:/dashboard/invented]] [[ref:039]]")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := newCoverageFixture(t)
			tc.sabotage(t, ws)
			if tc.preflight {
				runExpectingFailure(t, coverageCommand(t, ws, "docs/demo", filepath.Join(ws, ".golden-master")), tc.want)
				return
			}
			got := runCoverage(t, ws)
			if got.OK {
				t.Fatalf("the gate stayed GREEN under sabotage — it falsifies nothing:\n%s", got.Log)
			}
			if !strings.Contains(got.Log, tc.want) {
				t.Fatalf("the gate is red WITHOUT the expected cause %q — a refusal for another reason proves nothing:\n%s", tc.want, got.Log)
			}
		})
	}
}

// TestProductDocsCoverageGateRefusesAnIndexTable: CO-PRESENCE IS NOT
// DOCUMENTATION. The gap rule asked for one line carrying a feature id AND one
// of its entries — two code spans on one line. A table pairing every feature
// with its entry satisfies that, restitutes nothing, and converges.
func TestProductDocsCoverageGateRefusesAnIndexTable(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	// Every feature, every entry, every path — and not one sentence.
	writeFile(t, ws, "docs/demo/README.md", "# The product\n"+
		"\n"+
		"## Index — [[ref:/]] [[ref:001]]\n"+
		"\n"+
		"| feature | entry | path |\n"+
		"|---|---|---|\n"+
		"| [[ref:home.landing]] | [[ref:001]] | [[ref:/]] |\n"+
		"| [[ref:items.list]] | [[ref:026]] | [[ref:/dashboard/items]] |\n"+
		"| [[ref:items.list.paging]] | [[ref:027]] | [[ref:/dashboard/items?page=2]] |\n"+
		"| [[ref:items.detail]] | [[ref:039]] | [[ref:/dashboard/items/{id}]] |\n")
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("a table of anchors was blessed as an exhaustive documentation:\n%s", got.Log)
	}
	for _, name := range []string{"home.landing", "items.list", "items.list.paging", "items.detail"} {
		if !strings.Contains(got.Log, "GAP -- "+name+": the covered feature is CITED but not documented") {
			t.Fatalf("the refusal does not name %s as cited-but-undocumented:\n%s", name, got.Log)
		}
	}
	// ...and the intact fixture, whose chapters carry real sentences, is not
	// caught by the same rule.
	if got := runCoverage(t, newCoverageFixture(t)); !got.OK {
		t.Fatalf("the prose requirement fired on a documentation that reads:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateCreditsABlockItsOwnProse: prose is credited over
// the BLOCK that carries a citation, and a block is what markdown renders as
// one. Consecutive non-blank lines used to make one paragraph, so a table's
// header and every other row were credited to each feature listed in it: an
// index table with one descriptive column scored every feature documented.
// The same false green went through a bullet list, HTML rows, a heading glued
// to a paragraph and — with no markup at all — a paragraph of soft-wrapped
// index lines, whose words were credited whole to each feature it listed.
//
// Each case isolates ONE rule and reads BOTH directions on the same page:
// the features that must be refused, and the neighbour that must stay
// documented by its own block.
func TestProductDocsCoverageGateCreditsABlockItsOwnProse(t *testing.T) {
	requireGitPython(t)
	// One block's worth of real description: enough, on its own, for four
	// features at once — so a case only reddens if the rule under test keeps
	// its neighbours from borrowing it.
	const long = "shows one item in full: every field the manager filled in when creating it, " +
		"the complete history of its changes with the author and the date of each one, the comments " +
		"left by colleagues, the attachments uploaded along the way, and the actions still open to them today."
	// Five recognisable words in 39 characters: short of min_prose on its own.
	const short = "shows the item record for every manager"
	bare := []string{"home.landing", "items.list", "items.list.paging"}
	cases := []struct {
		name       string
		body       string
		gap        []string // features the gate must refuse
		documented []string // features on the same page it must credit
		want       string   // what the log must also say
	}{
		{
			name: "an index table with a descriptive column",
			body: "| Fonctionnalite | Reference | Description |\n" +
				"|---|---|---|\n" +
				"| [[ref:home.landing]] | [[ref:001]] | page accueil |\n" +
				"| [[ref:items.list]] | [[ref:026]] | liste paginee |\n" +
				"| [[ref:items.list.paging]] | [[ref:027]] | pagination vingt |\n" +
				"| [[ref:items.detail]] | [[ref:039]] | fiche article |\n",
			gap:  []string{"home.landing", "items.list", "items.list.paging", "items.detail"},
			want: "the table row citing it",
		},
		{
			name: "a table row that describes its feature documents it, and only it",
			body: "Feature | Reference | Description\n" +
				"--- | --- | ---\n" +
				"[[ref:home.landing]] | [[ref:001]] | page accueil\n" +
				"[[ref:items.list]] | [[ref:026]] | liste paginee\n" +
				"[[ref:items.list.paging]] | [[ref:027]] | pagination vingt\n" +
				"[[ref:items.detail]] | [[ref:039]] | " + long + "\n",
			gap:        bare,
			documented: []string{"items.detail"},
		},
		{
			name: "a list item is read on its own",
			body: "- [[ref:home.landing]] [[ref:001]] page accueil\n" +
				"- [[ref:items.list]] [[ref:026]] liste paginee\n" +
				"- [[ref:items.list.paging]] [[ref:027]] pagination vingt\n" +
				"- [[ref:items.detail]] [[ref:039]] " + long + "\n",
			gap:        bare,
			documented: []string{"items.detail"},
		},
		{
			name: "a list item wrapped over three lines is one item",
			body: "- [[ref:items.detail]] [[ref:039]] shows one item in full: every field the\n" +
				"  manager filled in, the history of its changes and the actions still open\n" +
				"  to them.\n",
			documented: []string{"items.detail"},
		},
		{
			name: "a line of block-level HTML is read on its own",
			body: "<table>\n" +
				"<tr><th>Feature</th><th>Reference</th><th>Description</th></tr>\n" +
				"<tr><td>[[ref:home.landing]]</td><td>[[ref:001]]</td><td>page accueil</td></tr>\n" +
				"<tr><td>[[ref:items.list]]</td><td>[[ref:026]]</td><td>liste paginee</td></tr>\n" +
				"<tr><td>[[ref:items.list.paging]]</td><td>[[ref:027]]</td><td>pagination vingt</td></tr>\n" +
				"<tr><td>[[ref:items.detail]]</td><td>[[ref:039]]</td><td>" + long + "</td></tr>\n" +
				"</table>\n",
			gap:        bare,
			documented: []string{"items.detail"},
		},
		{
			name: "a paragraph shares its prose between the features it documents",
			body: "[[ref:home.landing]] [[ref:001]] page accueil visiteur\n" +
				"[[ref:items.list]] [[ref:026]] liste paginee complete\n" +
				"[[ref:items.list.paging]] [[ref:027]] pagination vingt lignes\n" +
				"[[ref:items.detail]] [[ref:039]] fiche article detaillee\n",
			gap:  []string{"home.landing", "items.list", "items.list.paging", "items.detail"},
			want: "documents 4 feature(s)",
		},
		{
			name: "a paragraph with prose enough for each of its features documents all of them",
			body: "The manager opens the list — [[ref:items.list]] [[ref:026]] — and reads twenty rows,\n" +
				"newest first, each one showing its owner, its status and its last change. Paging\n" +
				"is [[ref:items.list.paging]] [[ref:027]]: twenty rows per page, with a pager at the\n" +
				"foot of the list to move from one page to the next.\n",
			documented: []string{"items.list", "items.list.paging"},
		},
		{
			name: "a heading documents nothing",
			body: "## [[ref:items.detail]] [[ref:039]] The item record, with every field the manager " +
				"filled in, its history and the actions still open\n",
			gap:  []string{"items.detail"},
			want: "ONLY IN A HEADING",
		},
		{
			name: "a heading is not part of the paragraph below it",
			body: "## The screen the manager opens every morning to follow each item — [[ref:/dashboard/items/{id}]]\n" +
				"[[ref:items.detail]] [[ref:039]] fiche article\n",
			gap: []string{"items.detail"},
		},
		{
			name: "an underlined title is a chapter heading",
			body: "The item record\n" +
				"---------------\n" +
				"\n" +
				"[[ref:items.detail]] [[ref:039]] " + long + "\n",
			documented: []string{"items.detail"},
			want:       "the chapter The item record names NO reference",
		},
		{
			name: "a quote is not part of the paragraph above it",
			body: "[[ref:items.detail]] [[ref:039]] fiche article\n" +
				"> " + long + "\n",
			gap: []string{"items.detail"},
		},
		{
			name: "a template tag line ends the block",
			body: "{% hint style=\"warning\" %}\n" +
				"[[ref:items.detail]] [[ref:039]] shows one item in full for the manager\n" +
				"{% endhint %}\n",
			gap: []string{"items.detail"},
		},
		{
			name: "table pipes are markup, not prose",
			body: "Reference | Entry | Description\n" +
				"--- | --- | ---\n" +
				"[[ref:items.detail]] | [[ref:039]] | " + short + strings.Repeat(" |", 20) + "\n",
			gap: []string{"items.detail"},
		},
		{
			name: "HTML tags are markup, not prose",
			body: "<tr><td>[[ref:items.detail]]</td><td>[[ref:039]]</td><td>" + short + "</td></tr>\n",
			gap:  []string{"items.detail"},
		},
		{
			name: "a link destination is not prose",
			body: "[[ref:items.detail]] [[ref:039]] [the item record](https://example.org/manager/reading/items/record/history/listing/)\n",
			gap:  []string{"items.detail"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := newCoverageFixture(t)
			writeFile(t, ws, "docs/demo/README.md", "# The product\n\n## Index — [[ref:/]] [[ref:001]]\n\n"+tc.body)
			got := runCoverage(t, ws)
			for _, name := range tc.gap {
				if !strings.Contains(got.Log, "GAP -- "+name+":") {
					t.Fatalf("%s was credited with prose that is not its own:\n%s", name, got.Log)
				}
			}
			for _, name := range tc.documented {
				if strings.Contains(got.Log, "GAP -- "+name+":") {
					t.Fatalf("%s is described by its own block and was refused:\n%s", name, got.Log)
				}
			}
			if tc.want != "" && !strings.Contains(got.Log, tc.want) {
				t.Fatalf("the log does not say %q:\n%s", tc.want, got.Log)
			}
		})
	}
}

// TestProductDocsCoverageGateReadsAnExclusionOverItsBlock: the exclusion rule
// measured its prose on the citing LINE, so a normally wrapped paragraph —
// the reason said once, across two lines — was refused, and the refusal asked
// for prose the page already carried. It now reads the block the gap rule
// reads; and one block still answers for ONE hole.
func TestProductDocsCoverageGateReadsAnExclusionOverItsBlock(t *testing.T) {
	requireGitPython(t)
	// The citing line alone carries 38 characters of prose; the paragraph
	// says the whole reason.
	ws := newCoverageFixture(t)
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"Signing out "+ref("menu.logout")+" tears the session down and\n"+
		"shows no screen of its own: the net never captures it, so it stays out.\n")
	if got := runCoverage(t, ws); !got.OK || got.Named != 1 {
		t.Fatalf("an exclusion explained by a paragraph wrapped over two lines was refused (named %d/1):\n%s", got.Named, got.Log)
	}
	// One paragraph naming two holes answers for the first only.
	ws = newCoverageFixture(t)
	writeFile(t, ws, ".golden-master/feature-coverage.json", strings.Replace(coverageInventory,
		`  {"feature": "menu.logout",
   "reason": "session teardown, not a screen: measured by the ops runbook, not this net"}`,
		`  {"feature": "menu.logout",
   "reason": "session teardown, not a screen: measured by the ops runbook, not this net"},
  {"feature": "menu.language",
   "reason": "the language switch changes no served content, so the corpus captures nothing"}`, 1))
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"The session teardown "+ref("menu.logout")+" and the language switch "+ref("menu.language")+" sit\n"+
		"outside: neither is a screen, and the corpus captures nothing of either.\n")
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("one paragraph answered for two different holes:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "CONCEALED_EXCLUSION -- menu.logout: the exclusion is named with the SAME prose as menu.language") {
		t.Fatalf("the refusal does not name the second hole as sharing the first one's paragraph:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateVerifiesAnInventedReferenceAnywhere: co-location
// answers "is this token a citation to CREDIT?", not "must this token be
// VERIFIED?". Conflating the two let three invented screens through, one per
// sentence, because no line cited anything else.
func TestProductDocsCoverageGateVerifiesAnInventedReferenceAnywhere(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	writeFile(t, ws, "docs/demo/more.md", "# More screens "+defaultNoAnchorMarker+"\n"+
		"\n"+
		"The reporting screen [[ref:items.reporting]] shows the month as the manager left it.\n"+
		"\n"+
		"The export screen [[ref:items.exporting]] writes the year to a file for the auditor.\n"+
		"\n"+
		"The archive screen [[ref:items.archiving]] hides what nobody consults any more.\n")
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("three invented screens, one per sentence, were blessed:\n%s", got.Log)
	}
	for _, name := range []string{"items.reporting", "items.exporting", "items.archiving"} {
		if !strings.Contains(got.Log, "the reference "+name+" is CITED and") {
			t.Fatalf("the invented reference %s was not verified on its own line:\n%s", name, got.Log)
		}
	}
}

// TestProductDocsCoverageGateCapsAnchorlessChapters: the anchor refusal NAMES
// the marker that satisfies it, so an agent reading `fail_log` answers every
// complaint by marking the chapter. An exception with no ceiling is the rule.
func TestProductDocsCoverageGateCapsAnchorlessChapters(t *testing.T) {
	requireGitPython(t)
	run := func(ws, cap string) coverageOut {
		t.Helper()
		var got coverageOut
		runJSON(t, coverageCommandCapped(t, ws, "docs/demo", filepath.Join(ws, ".golden-master"),
			defaultExclusionsToken, cap), &got)
		return got
	}
	ws := newCoverageFixture(t)
	// The shipped fixture declares ONE anchorless chapter (its method
	// chapter). At the production default it is one too many.
	got := run(ws, "0")
	if got.OK {
		t.Fatalf("the declared ceiling of 0 let a chapter through:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "DECLARE they restitute no reference and at most 0 may") {
		t.Fatalf("the refusal does not name the ceiling:\n%s", got.Log)
	}
	if got := run(ws, "1"); !got.OK {
		t.Fatalf("a ceiling raised deliberately to 1 still refused one declaration:\n%s", got.Log)
	}
	// The exclusions chapter is anchored by its ROLE and spends nothing.
	if got.Anchorless != 1 || got.AnchorlessMax != 0 {
		t.Fatalf("counts = %d declared / ceiling %d, want 1 and 0", got.Anchorless, got.AnchorlessMax)
	}
	// Marking one more chapter costs: at the same ceiling it is refused.
	mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]",
		"## One item "+defaultNoAnchorMarker)
	if got := run(ws, "1"); got.OK {
		t.Fatalf("a second declaration was free at a ceiling of 1:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateRefusesACopiedExclusionProse: `min_prose` is a
// length, and a length is an orthography anyone reaches — one generic sentence
// copied under every id cleared it. The net already carries the reason, so the
// page has to say what the net says, and say it once per hole.
func TestProductDocsCoverageGateRefusesACopiedExclusionProse(t *testing.T) {
	requireGitPython(t)
	newTwoHoleFixture := func(t *testing.T) string {
		t.Helper()
		ws := newCoverageFixture(t)
		writeFile(t, ws, ".golden-master/feature-coverage.json", strings.Replace(coverageInventory,
			`  {"feature": "menu.logout",
   "reason": "session teardown, not a screen: measured by the ops runbook, not this net"}`,
			`  {"feature": "menu.logout",
   "reason": "session teardown, not a screen: measured by the ops runbook, not this net"},
  {"feature": "menu.language",
   "reason": "the language switch changes no served content, so the corpus captures nothing"}`, 1))
		return ws
	}
	// A sentence long enough, and entirely its own: accepted.
	ws := newTwoHoleFixture(t)
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"- [[ref:menu.logout]] — signing out tears the session down without showing a screen "+
		"of its own, so the net never captures it.\n"+
		"- [[ref:menu.language]] — the language switch changes no served content, so the corpus "+
		"captures nothing of it either.\n")
	if got := runCoverage(t, ws); !got.OK {
		t.Fatalf("two exclusions, each with its own prose, were refused:\n%s", got.Log)
	}
	// ONE sentence for both: it describes neither.
	ws = newTwoHoleFixture(t)
	// One sentence that genuinely overlaps BOTH recorded reasons: it clears
	// the lexical check and is refused for what it is — a template.
	shared := " — this screen sits outside the perimeter: the corpus captures nothing of " +
		"it, and the session teardown it triggers changes nothing either.\n"
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"- [[ref:menu.logout]]"+shared+
		"- [[ref:menu.language]]"+shared)
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("one sentence answered for two different holes:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "named with the SAME prose as") {
		t.Fatalf("the refusal does not name the cause:\n%s", got.Log)
	}
	// A row of dots reaches any length, and says nothing.
	ws = newTwoHoleFixture(t)
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"- [[ref:menu.logout]] — "+strings.Repeat(".", 70)+"\n"+
		"- [[ref:menu.language]] — "+strings.Repeat("-_", 40)+"\n")
	got = runCoverage(t, ws)
	if got.OK {
		t.Fatalf("a row of punctuation passed for written prose:\n%s", got.Log)
	}
	// Prose of its own, but saying nothing the net says: still concealed.
	ws = newTwoHoleFixture(t)
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"- [[ref:menu.logout]] — pour mémoire, rien de particulier à signaler ici pour ce point précis.\n"+
		"- [[ref:menu.language]] — the language switch changes no served content, so the corpus "+
		"captures nothing of it either.\n")
	got = runCoverage(t, ws)
	if got.OK || !strings.Contains(got.Log, "CONCEALED_EXCLUSION -- menu.logout") {
		t.Fatalf("prose that shares not one word with the recorded reason was accepted:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateKeepsTheExclusionsChapterOpenUnderSubHeadings: a
// sub-heading repeating the declared token used to RE-ANCHOR the chapter at
// its own deeper level; the next sibling at that depth then satisfied
// `level <= exc_level` and closed the whole chapter, so every exclusion named
// after it read as concealed — a convergence term no rewrite of the prose can
// satisfy, and the run burns its remaining passes.
func TestProductDocsCoverageGateKeepsTheExclusionsChapterOpenUnderSubHeadings(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"### Exclusions\n"+
		"\n"+
		"A first group, split out for the reader.\n"+
		"\n"+
		"### Limites connues "+defaultNoAnchorMarker+"\n"+
		"\n"+
		"- "+ref("menu.logout")+" — signing out tears the session down without showing a "+
		"screen of its own, so the net never captures it and this documentation does not describe it.\n")
	got := runCoverage(t, ws)
	if !got.OK {
		t.Fatalf("a sub-heading repeating the declared token closed the exclusions chapter early:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateStopsOfferingASpentMarker: the anchor refusal
// NAMES the marker as its remedy, and the ceiling then refuses the marked
// chapter — two UNANCHORED_CHAPTER causes ping-ponging until `max_passes`, and
// the ceiling's own remedy (a launch var) is outside the writeable set. A
// refusal must never offer what the next gate takes back.
func TestProductDocsCoverageGateStopsOfferingASpentMarker(t *testing.T) {
	requireGitPython(t)
	run := func(ws, cap string) coverageOut {
		t.Helper()
		var got coverageOut
		runJSON(t, coverageCommandCapped(t, ws, "docs/demo", filepath.Join(ws, ".golden-master"),
			defaultExclusionsToken, cap), &got)
		return got
	}
	// The fixture already spends one declaration on its method chapter.
	// Strip a chapter's anchor: at a ceiling of 2 there is headroom, and the
	// marker is a legitimate remedy.
	ws := newCoverageFixture(t)
	mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]", "## One item")
	got := run(ws, "2")
	if got.OK {
		t.Fatalf("an unanchored chapter passed:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "declaration(s) left of 2") {
		t.Fatalf("with headroom, the refusal does not offer the marker:\n%s", got.Log)
	}
	// At a ceiling of 1 the single declaration is already spent: the refusal
	// must ask for an ANCHOR, name no marker, and never ask for a bigger
	// ceiling — that is an operator decision, not a repair this run can make.
	got = run(ws, "1")
	if got.OK {
		t.Fatalf("an unanchored chapter passed at a spent ceiling:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "ALL SPENT") {
		t.Fatalf("with no headroom, the refusal does not say the hatch is full:\n%s", got.Log)
	}
	for _, forbidden := range []string{"declare it restitutes none", "raise the ceiling"} {
		if strings.Contains(got.Log, forbidden) {
			t.Fatalf("the refusal still offers %q, which the ceiling takes back:\n%s", forbidden, got.Log)
		}
	}
}

// TestProductDocsCoverageGateFindsAnExclusionItsOwnLine: the rule broke on the
// FIRST qualifying line, then the one-prose-per-hole check refused it if an
// earlier exclusion had claimed it. A legitimate summary line naming two holes
// therefore made the second one UNFIXABLE — its own dedicated paragraph
// further down was never looked at, and the refusal asked for prose the page
// had already written.
func TestProductDocsCoverageGateFindsAnExclusionItsOwnLine(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	writeFile(t, ws, ".golden-master/feature-coverage.json", strings.Replace(coverageInventory,
		`  {"feature": "menu.logout",
   "reason": "session teardown, not a screen: measured by the ops runbook, not this net"}`,
		`  {"feature": "menu.logout",
   "reason": "session teardown, not a screen: measured by the ops runbook, not this net"},
  {"feature": "menu.language",
   "reason": "the language switch changes no served content, so the corpus captures nothing"}`, 1))
	// A summary line names BOTH holes — legitimate writing — and each then
	// gets its own paragraph. The summary is read first; it must not lock the
	// second exclusion out of the paragraph written for it.
	writeFile(t, ws, "docs/demo/exclusions.md", "# What this documentation does not cover\n"+
		"\n"+
		"## Exclusions\n"+
		"\n"+
		"Two screens sit outside: "+ref("menu.logout")+" and "+ref("menu.language")+" — the session "+
		"teardown and the language switch, neither of which the corpus captures.\n"+
		"\n"+
		"- "+ref("menu.logout")+" — signing out tears the session down without showing a screen "+
		"of its own, so the net never captures it.\n"+
		"\n"+
		"- "+ref("menu.language")+" — the language switch changes no served content, so the corpus "+
		"captures nothing of it either.\n")
	got := runCoverage(t, ws)
	if !got.OK {
		t.Fatalf("a summary line naming two holes locked the second one out of its own paragraph:\n%s", got.Log)
	}
	if got.Named != 2 {
		t.Fatalf("exclusions named = %d, want 2", got.Named)
	}
}

// TestProductDocsCoverageGateMatchesTheWholeExclusionsTitle: the declared token
// was matched as a SUBSTRING. On an insurance product a chapter called "Les
// exclusions de garantie" — reader-facing content — opened the chapter of
// documented holes, and an id named under it counted as declared.
func TestProductDocsCoverageGateMatchesTheWholeExclusionsTitle(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	mutate(t, ws, "docs/demo/exclusions.md", "## Exclusions", "## Les exclusions de garantie")
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("a chapter merely CONTAINING the declared token opened the exclusions chapter:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "CONCEALED_EXCLUSION -- menu.logout") {
		t.Fatalf("the gate is red without the expected cause:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateRefusesACitedTailWildcard: a cited path is read as
// a PATTERN. `/dashboard/**` carries one literal segment, so it clears the
// placeholders-only rule while claiming every screen below it.
func TestProductDocsCoverageGateRefusesACitedTailWildcard(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]",
		"## One item — [[ref:/dashboard/**]] [[ref:039]]")
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("a cited tail wildcard claimed every screen below it and passed:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "carries a TAIL WILDCARD") {
		t.Fatalf("the refusal does not name the cause:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateConfinesTheRoutesFile: `coverage_routes_file` is
// read from INSIDE the net, like `oracle_dir` — and the repair is a launch
// var, outside the writeable set, so it stops the run.
func TestProductDocsCoverageGateConfinesTheRoutesFile(t *testing.T) {
	requireGitPython(t)
	outside := filepath.Join(t.TempDir(), "routes.txt")
	if err := os.WriteFile(outside, []byte(coverageRoutes), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{outside, "../routes.txt"} {
		ws := newCoverageFixture(t)
		cmd := resolveCommand(t, toolCommand(t, "product-docs/main.bot", "coverage_check"), map[string]string{
			"vars.workspace_dir":               ws,
			"input.product_dir":                "docs/demo",
			"input.oracle_path":                filepath.Join(ws, ".golden-master"),
			"vars.coverage_exclusions_heading": defaultExclusionsToken,
			"vars.coverage_no_anchor_marker":   defaultNoAnchorMarker,
			"vars.coverage_routes_file":        bad,
			"vars.coverage_citation_open":      citeOpen,
			"vars.coverage_citation_close":     citeClose,
			"vars.coverage_placeholders":       defaultPlaceholders,
			"vars.coverage_min_prose":          "60",
			"vars.coverage_max_anchorless":     shippedAnchorlessCeiling,
		})
		runExpectingFailure(t, cmd, "absolute path or a dot-dot escape")
	}
}

// TestProductDocsCoverageGateSaysTheNetIsUnproven: Prody reads the inventory as
// given. Nothing here proves the net's OWN gate ever ran on it, and a
// degradation nobody can see is a degradation nobody pays for.
func TestProductDocsCoverageGateSaysTheNetIsUnproven(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	got := runCoverage(t, ws)
	if !got.NetUnproven || !strings.Contains(got.Log, "carries NO trace of its own gate") {
		t.Fatalf("a net with no rite emission beside it was read as proven: net_unproven=%v\n%s", got.NetUnproven, got.Log)
	}
	// The emissions a rite leaves behind are that trace — never a verdict, and
	// the gate stays green either way: this is telemetry, not a refusal.
	writeFile(t, ws, ".golden-master/verify-oracle.sh", "#!/bin/sh\nexit 0\n")
	got = runCoverage(t, ws)
	if got.NetUnproven {
		t.Fatalf("the rite runner beside the net was not seen:\n%s", got.Log)
	}
	if !got.OK {
		t.Fatalf("the proof telemetry turned into a refusal:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateReadsAnAbsentExclusionsKeyAsEmpty: a product that
// excludes NOTHING writes no `exclusions` key, and the net producer reads an
// absent key as an empty list (`coverage.get(key) or []`). Refusing it made
// every such product NET_UNREADABLE for ever — and the repair `fail_log`
// ordered lands in `<oracle_dir>`, which `scope_check` forbids this run to
// write, so the campaign burned every pass obeying two contradictory gates.
func TestProductDocsCoverageGateReadsAnAbsentExclusionsKeyAsEmpty(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	writeFile(t, ws, ".golden-master/corpus.json",
		`{"entries": [{"id": "checkout-pay", "method": "GET", "path": "/checkout/pay"}]}`)
	// No `exclusions` key at all: this product has no hole to declare.
	writeFile(t, ws, ".golden-master/feature-coverage.json",
		`{"features": [{"feature": "payment/card", "entries": ["checkout-pay"]}]}`)
	writeFile(t, ws, "docs/demo/p.md", "# Pay\n\n## Paying — "+ref("/checkout/pay")+" "+ref("checkout-pay")+"\n\n"+
		ref("payment/card")+" "+ref("checkout-pay")+" is the only means of payment a customer may use, "+
		"and the order is confirmed on the same screen.\n")
	got := runCoverage(t, ws)
	if !got.OK {
		t.Fatalf("a product with nothing to exclude was refused, for a repair it is forbidden to make:\n%s", got.Log)
	}
	if got.Exclusions != 0 {
		t.Fatalf("exclusions_total = %d with no key, want 0", got.Exclusions)
	}
	// A key that is THERE and mistyped is still a refusal — and a pre-flight
	// one, because its repair is in the net.
	writeFile(t, ws, ".golden-master/feature-coverage.json",
		`{"features": [{"feature": "payment/card", "entries": ["checkout-pay"]}], "exclusions": {"a": "b"}}`)
	runExpectingFailure(t, coverageCommand(t, ws, "docs/demo", filepath.Join(ws, ".golden-master")),
		"exclusions is present and is not a list")
}

// TestProductDocsCoverageGateIgnoresFencedSamples: a fenced block is a page
// teaching its reader to paste something, not a claim about the product. A
// path or an entry-shaped token inside one is not a citation, and a shell
// comment inside one is not a chapter — the same reason page_lint exempts
// fenced blocks from its chrome rules. Without this the gate would go
// permanently red on any page that shows a command.
func TestProductDocsCoverageGateIgnoresFencedSamples(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	writeFile(t, ws, "docs/demo/how-to.md",
		"# Getting there <!--no-anchor-->\n"+
			"\n"+
			"Paste this into your terminal:\n"+
			"\n"+
			"```sh\n"+
			"# Fetch the invented page\n"+
			"curl [[ref:/dashboard/invented]] [[ref:999]]\n"+
			"```\n")
	got := runCoverage(t, ws)
	if !got.OK {
		t.Fatalf("a fenced sample was read as a citation — every page showing a command would be permanently red:\n%s", got.Log)
	}
	if got.Chapters != 5 {
		t.Fatalf("chapters = %d, want 5 — a comment inside a fenced shell block is not a chapter", got.Chapters)
	}
	// The exemption is the FENCE, not the words: the same tokens in prose are
	// still refused.
	mutate(t, ws, "docs/demo/how-to.md", "```sh\n# Fetch the invented page\ncurl ", "Run: ")
	got = runCoverage(t, ws)
	if got.OK || !strings.Contains(got.Log, "the path /dashboard/invented") {
		t.Fatalf("the same citation in prose slipped through:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateScopesTheChapterToItsPage: chapter state is PER
// PAGE, like fence state. A page ending inside the exclusions chapter must not
// mark the NEXT page's lines as being under it — an exclusion id cited with
// enough prose in an unrelated page would otherwise satisfy
// CONCEALED_EXCLUSION, which is the exact false green this gate exists to
// refuse. The page names below fix the read order (sorted): README, then the
// exclusions page, then the annex.
func TestProductDocsCoverageGateScopesTheChapterToItsPage(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	if err := os.Remove(filepath.Join(ws, "docs/demo/exclusions.md")); err != nil {
		t.Fatal(err)
	}
	// A one-chapter-per-file exclusions page that names nothing.
	writeFile(t, ws, "docs/demo/a-exclusions.md",
		"# Exclusions "+defaultNoAnchorMarker+"\n\nThis page lists what the documentation leaves out.\n")
	// An unrelated annex. Its first heading is DEEPER than the exclusions
	// chapter, so a leaked chapter state would survive into it.
	writeFile(t, ws, "docs/demo/b-annex.md",
		"## Extra notes "+defaultNoAnchorMarker+"\n\n"+
			"- [[ref:menu.logout]] — signing out tears the session down without showing a "+
			"screen of its own, so the net never captures it and this page mentions it only in passing.\n")
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("an exclusion named OUTSIDE the exclusions chapter satisfied the gate — the chapter state leaked across the page boundary:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "CONCEALED_EXCLUSION -- menu.logout") {
		t.Fatalf("the gate is red without the expected cause:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateFoldsTheDeclaredToken: the var's contract says
// the exclusions token matches case- and accent-insensitively. Folding only
// the heading would make every natural spelling — `Exclusions`, an accented
// French token — a token that can NEVER match, so every exclusion would redden
// and the run could never converge. Only the lowercase-ASCII default worked.
func TestProductDocsCoverageGateFoldsTheDeclaredToken(t *testing.T) {
	requireGitPython(t)
	for _, token := range []string{"Exclusions", "EXCLUSIONS", "exclusions"} {
		t.Run(token, func(t *testing.T) {
			ws := newCoverageFixture(t)
			var got coverageOut
			runJSON(t, coverageCommandWith(t, ws, "docs/demo", filepath.Join(ws, ".golden-master"), token), &got)
			if !got.OK {
				t.Fatalf("the declared token %q never matched the chapter it names:\n%s", token, got.Log)
			}
		})
	}
	// An accented token against an accented heading: both sides folded.
	ws := newCoverageFixture(t)
	mutate(t, ws, "docs/demo/exclusions.md", "## Exclusions", "## Périmètre exclu")
	var got coverageOut
	runJSON(t, coverageCommandWith(t, ws, "docs/demo", filepath.Join(ws, ".golden-master"), "PÉRIMÈTRE EXCLU"), &got)
	if !got.OK {
		t.Fatalf("an accented declared token never matched its accented heading:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateCitesOrDoesNot is the bench that decides, and it
// asserts BOTH directions on the SAME fixture. Two earlier rounds oscillated
// between a false green (an invented screen alone on its line was never
// verified) and a false red (an ordinary code span refused as a corpus entry
// that does not exist), because the gate GUESSED from a token's shape which of
// the two it was. A bench that tests one direction only is what let that
// oscillate: each round fixed the direction its bench measured and broke the
// other.
//
// A reference is now what the page SAYS is one. So:
//   - every citation is verified WHEREVER it sits, with nothing else on the
//     line — no false green;
//   - nothing that is not a citation is ever looked at — no false red.
func TestProductDocsCoverageGateCitesOrDoesNot(t *testing.T) {
	requireGitPython(t)

	// ── direction 1: ordinary prose is NEVER a reference ────────────────
	// Each of these was measured to redden the shape inference: `Mot de
	// passe` and `package.json` share (12, {letters, punctuation}) with
	// `checkout-pay`; `404` and `250` share (3, {digits}) with `001`.
	prose := "# Questions " + defaultNoAnchorMarker + "\n\n" +
		"## What if the item is gone " + defaultNoAnchorMarker + "\n\n" +
		"The server answers `404` and the list shows `250` rows at most.\n" +
		"The `Mot de passe` field is never pre-filled, `package.json` is not shipped,\n" +
		"and a `user-profile` block is out of scope here.\n"
	ws := newCoverageFixture(t)
	writeFile(t, ws, "docs/demo/faq.md", prose)
	got := runCoverage(t, ws)
	if !got.OK {
		t.Fatalf("ordinary prose was read as a citation — coverage_ok is a convergence term, so the campaign would be ordered to delete reader-facing text:\n%s", got.Log)
	}
	for _, span := range []string{"404", "250", "Mot de passe", "package.json", "user-profile"} {
		if strings.Contains(got.Log, span) {
			t.Fatalf("the gate even NAMED the ordinary code span %q:\n%s", span, got.Log)
		}
	}

	// ── direction 2: a citation is verified wherever it sits ────────────
	// Same fixture, same page, same tokens — marked as citations this time.
	// Each sits alone on its line, citing nothing else: the case that used
	// to pass, because verification rode on co-location.
	for _, token := range []string{"404", "Mot de passe", "package.json", "user-profile", "items.reporting"} {
		t.Run("cited: "+token, func(t *testing.T) {
			ws := newCoverageFixture(t)
			writeFile(t, ws, "docs/demo/faq.md", "# Questions "+defaultNoAnchorMarker+"\n\n"+
				"## What if the item is gone "+defaultNoAnchorMarker+"\n\n"+
				"The screen "+ref(token)+" answers the question and nothing else does.\n")
			got := runCoverage(t, ws)
			if got.OK {
				t.Fatalf("an invented reference alone on its line was never verified:\n%s", got.Log)
			}
			if !strings.Contains(got.Log, "the reference "+token+" is CITED and") {
				t.Fatalf("the refusal does not name the invented citation %q:\n%s", token, got.Log)
			}
		})
	}

	// ...and a citation of something the net DOES hold stays green in the
	// same position, so the refusal above is about existence, not about
	// being cited alone.
	ws = newCoverageFixture(t)
	writeFile(t, ws, "docs/demo/faq.md", "# Questions "+defaultNoAnchorMarker+"\n\n"+
		"## What if the item is gone "+defaultNoAnchorMarker+"\n\n"+
		"The screen "+ref("039")+" answers the question and nothing else does.\n")
	if got := runCoverage(t, ws); !got.OK {
		t.Fatalf("a citation of an entry the corpus holds was refused:\n%s", got.Log)
	}
	// An empty citation names nothing and is refused rather than ignored.
	ws = newCoverageFixture(t)
	writeFile(t, ws, "docs/demo/faq.md", "# Questions "+defaultNoAnchorMarker+"\n\n"+
		"## What if the item is gone "+defaultNoAnchorMarker+"\n\n"+
		"The screen "+citeOpen+citeClose+" answers nothing at all here.\n")
	got = runCoverage(t, ws)
	if got.OK || !strings.Contains(got.Log, "an EMPTY citation") {
		t.Fatalf("an empty citation was read as no citation:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateNeedsItsCitationSyntax: with no way to recognise a
// reference the gate would read every page as citing nothing — a GAP per
// feature at best, a silent pass at worst. Undeclared is a named refusal, and
// its repair is a launch var, so it stops the run.
func TestProductDocsCoverageGateNeedsItsCitationSyntax(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	for _, tc := range []struct{ name, open, close string }{
		{"no opening token", "", citeClose},
		{"no closing token", citeOpen, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runExpectingFailure(t, coverageCommandFull(t, ws, "docs/demo", filepath.Join(ws, ".golden-master"),
				defaultExclusionsToken, shippedAnchorlessCeiling, tc.open, tc.close, "routes.txt"),
				"the citation syntax is not declared")
		})
	}
	// A docs repo already using [[...]] picks another spelling, and the same
	// pages written with it are read the same way.
	other := strings.NewReplacer(citeOpen, "<<ref:", citeClose, ">>")
	for _, f := range []string{"docs/demo/README.md", "docs/demo/exclusions.md"} {
		b, err := os.ReadFile(filepath.Join(ws, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, f), []byte(other.Replace(string(b))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got coverageOut
	runJSON(t, coverageCommandFull(t, ws, "docs/demo", filepath.Join(ws, ".golden-master"),
		defaultExclusionsToken, shippedAnchorlessCeiling, "<<ref:", ">>", "routes.txt"), &got)
	if !got.OK {
		t.Fatalf("a declared citation syntax other than the default was not honoured:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateRefusesAStaleInventoryReference: the inventory
// and the corpus are regenerated separately, so a feature can keep citing an
// entry the corpus no longer holds. Unchecked, that drift makes two of this
// gate's own rules mutually unsatisfiable — documenting the feature with that
// entry is a PHANTOM_DOC, omitting it is a GAP — and since fail_log orders the
// campaign to fix exactly the named causes, the run burns every pass obeying
// two contradictory instructions.
func TestProductDocsCoverageGateRefusesAStaleInventoryReference(t *testing.T) {
	requireGitPython(t)
	t.Run("an entry the corpus no longer holds", func(t *testing.T) {
		ws := newCoverageFixture(t)
		mutate(t, ws, ".golden-master/feature-coverage.json",
			`{"feature": "items.detail", "entries": ["039"]}`,
			`{"feature": "items.detail", "entries": ["039", "404"]}`)
		runExpectingFailure(t, coverageCommand(t, ws, "docs/demo", filepath.Join(ws, ".golden-master")),
			"corpus entries that DO NOT EXIST (404)")
	})
	// A truthy list that names nothing is not coverage. It used to join the
	// covered features with an empty entry set: a permanent GAP whose detail
	// listed no entry to document.
	t.Run("entries that are a truthy list of nothing", func(t *testing.T) {
		ws := newCoverageFixture(t)
		mutate(t, ws, ".golden-master/feature-coverage.json",
			`{"feature": "items.detail", "entries": ["039"]}`,
			`{"feature": "items.detail", "entries": ["   "]}`)
		runExpectingFailure(t, coverageCommand(t, ws, "docs/demo", filepath.Join(ws, ".golden-master")),
			"COVERED with no usable corpus entry")
	})
}

// TestProductDocsCoverageGateRefusesACitedCatchAll: the catch-all rule cuts
// BOTH ways. A cited path is read as a pattern, so a lone placeholder path
// matches every corpus entry — it would pass while restituting nothing, and
// anchor its chapter into the bargain. The hole the declared-route side closes
// must be closed on the side the graded agent writes.
func TestProductDocsCoverageGateRefusesACitedCatchAll(t *testing.T) {
	requireGitPython(t)
	for _, catchAll := range []string{"/**", "/{slug}", "/:id"} {
		t.Run(catchAll, func(t *testing.T) {
			ws := newCoverageFixture(t)
			mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]",
				"## One item — "+ref(catchAll)+" [[ref:039]]")
			got := runCoverage(t, ws)
			if got.OK {
				t.Fatalf("a placeholder-only citation passed — it matches every path and restitutes none:\n%s", got.Log)
			}
			if !strings.Contains(got.Log, "is made of PLACEHOLDERS ONLY") {
				t.Fatalf("the refusal does not name the cause:\n%s", got.Log)
			}
		})
	}
	// A path with one literal segment still proves something, and `/` is a
	// literal route: neither may be caught by this rule.
	ws := newCoverageFixture(t)
	if got := runCoverage(t, ws); !got.OK {
		t.Fatalf("the rule fired on the intact fixture, whose pages cite `/` and `/dashboard/items/{id}`:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateNamesTheRepairInItsHeadingLine: the rule reads
// the HEADING's own line, so a reference in the chapter body does not anchor
// it. An agent that repairs by adding body references reads the complaint as
// already satisfied and the bounded loop exhausts max_passes without
// converging — the refusal has to name the repair.
func TestProductDocsCoverageGateNamesTheRepairInItsHeadingLine(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]",
		"## One item\n\nThe chapter body cites [[ref:items.detail]] and [[ref:039]] all the same.")
	got := runCoverage(t, ws)
	if got.OK {
		t.Fatalf("a chapter anchored only in its body satisfied the rule:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "IN THE HEADING LINE ITSELF") {
		t.Fatalf("the refusal does not name the repair it wants:\n%s", got.Log)
	}
}

// TestProductDocsCatalogIngestRefusesASymlinkedLocalPath: normpath does not
// resolve symlinks, so a symlink COMMITTED in the docs repo and named by the
// catalog walked straight past a textual containment check. The catalog is
// repo content — hostile-grade by this node's own rule — so one docs-repo PR
// would pull an out-of-workspace repository into the scratch dir and hand it
// to the campaign agent as source material.
func TestProductDocsCatalogIngestRefusesASymlinkedLocalPath(t *testing.T) {
	requireGitPython(t)
	outside := t.TempDir()
	gitIn(t, outside, "init", "-q", "-b", "main")
	writeFile(t, outside, "secret-notes.md", "# not for the campaign\n")
	gitIn(t, outside, "add", "-A")
	gitIn(t, outside, "commit", "-q", "-m", "seed")

	scratch := t.TempDir()
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	if err := os.Symlink(outside, filepath.Join(ws, "mirror")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \"mirror\"\n",
		`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"mirror"}]}`+"\n")
	writeFile(t, ws, "docs/client/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	if got.OKCount != 0 || got.Degraded != 1 {
		t.Fatalf("a symlink to a repository outside the workspace was cloned: %d ok / %d degraded — %s", got.OKCount, got.Degraded, got.Log)
	}
	if note, _ := got.Inventory[0]["note"].(string); !strings.Contains(note, "escapes the docs workspace") {
		t.Fatalf("the refusal does not name its cause: %q", note)
	}
	if _, err := os.Stat(filepath.Join(scratch, "sources", "demo", "secret-notes.md")); err == nil {
		t.Fatalf("the out-of-workspace repository reached the scratch dir anyway")
	}
}

// TestProductDocsCatalogIngestConfinesAURLThatNamesTheFilesystem: `url` and
// `path` are two spellings of ONE thing once the value is on disk. `url` is
// read FIRST, so a containment rule written on `path` alone is a rule an
// attacker only has to not write: a catalog naming an out-of-workspace
// repository by `url` read it with the forge credential in the environment and
// handed its contents to the campaign as source material.
func TestProductDocsCatalogIngestConfinesAURLThatNamesTheFilesystem(t *testing.T) {
	requireGitPython(t)
	outside := newSourceRepo(t)
	for _, tc := range []struct{ name, url, want string }{
		{"an absolute path", outside, "escapes the docs workspace"},
		{"a file:// url", "file://" + outside, "escapes the docs workspace"},
		{"a file:// url carrying a host", "file://forge.invalid/repo.git", "carrying a host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			scratch := t.TempDir()
			gitIn(t, ws, "init", "-q", "-b", "main")
			catalogFixture(t, ws, "catalog/demo",
				"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    url: \""+tc.url+"\"\n",
				`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","url":"`+tc.url+`"}]}`+"\n")
			writeFile(t, ws, "docs/client/README.md", "# Demo\n")
			gitIn(t, ws, "add", "-A")
			gitIn(t, ws, "commit", "-q", "-m", "seed")

			var got ingestOut
			runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
			if got.OKCount != 0 || got.Degraded != 1 {
				t.Fatalf("a url naming a tree outside the workspace was cloned: %d ok / %d degraded — %s", got.OKCount, got.Degraded, got.Log)
			}
			if note, _ := got.Inventory[0]["note"].(string); !strings.Contains(note, tc.want) {
				t.Fatalf("the refusal does not name its cause %q: %q", tc.want, note)
			}
			if _, err := os.Stat(filepath.Join(scratch, "sources", "demo", "locales", "fr.json")); err == nil {
				t.Fatalf("the out-of-workspace repository reached the scratch dir anyway")
			}
		})
	}

	// ...and the SAME url, naming a repository the workspace holds, takes the
	// local decision: no forge, no network, no credential.
	t.Run("a url inside the workspace is read as a local source", func(t *testing.T) {
		ws := t.TempDir()
		scratch := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		inside := newLocalSource(t, ws)
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    url: "+inside+"\n",
			`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","url":"`+inside+`"}]}`+"\n")
		writeFile(t, ws, "docs/client/README.md", "# Demo\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "seed")

		var got ingestOut
		runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
		if got.OKCount != 1 {
			t.Fatalf("inventory = %d ok / %d degraded, want 1/0: %s", got.OKCount, got.Degraded, got.Log)
		}
		if local, _ := got.Inventory[0]["local"].(bool); !local {
			t.Fatalf("a url naming a repository ON DISK took the FORGE decision — the credential environment and the hardlinked clone come with it: %+v", got.Inventory[0])
		}
	})
}

// TestProductDocsCatalogIngestRefusesADelegatedObjectStore: a repository
// DELEGATES its object store through objects/info/alternates, and a clone
// serves the UNION — so a repository sitting inside the workspace hands over
// the contents of one that is not, and confining the path confines nothing.
// `--no-local` does not close it either: upload-pack serves the alternates all
// the same.
func TestProductDocsCatalogIngestRefusesADelegatedObjectStore(t *testing.T) {
	requireGitPython(t)
	outside := t.TempDir()
	gitIn(t, outside, "init", "-q", "-b", "main")
	writeFile(t, outside, "secret-notes.md", "# not for the campaign\n")
	gitIn(t, outside, "add", "-A")
	gitIn(t, outside, "commit", "-q", "-m", "seed")
	outsideHead := strings.TrimSpace(gitIn(t, outside, "rev-parse", "HEAD"))

	ws := t.TempDir()
	scratch := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	mirror := filepath.Join(ws, "mirror.git")
	gitIn(t, ws, "init", "-q", "--bare", "-b", "main", mirror)
	writeFile(t, mirror, "objects/info/alternates", filepath.Join(outside, ".git", "objects")+"\n")
	gitIn(t, mirror, "update-ref", "refs/heads/main", outsideHead)

	catalogFixture(t, ws, "catalog/demo",
		"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \"mirror.git\"\n",
		`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"mirror.git"}]}`+"\n")
	writeFile(t, ws, "docs/client/README.md", "# Demo\n")
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	if got.OKCount != 0 || got.Degraded != 1 {
		t.Fatalf("a repository delegating its objects outside the workspace was cloned: %d ok / %d degraded — %s", got.OKCount, got.Degraded, got.Log)
	}
	if note, _ := got.Inventory[0]["note"].(string); !strings.Contains(note, "reads its git objects from OUTSIDE the docs workspace") {
		t.Fatalf("the refusal does not name its cause: %q", note)
	}
	if _, err := os.Stat(filepath.Join(scratch, "sources", "demo", "secret-notes.md")); err == nil {
		t.Fatalf("the delegated store put an out-of-workspace tree into the scratch dir anyway")
	}
}

// TestProductDocsCoverageGateInertWithoutANet is the NON-REGRESSION contract.
// A product with no golden-master net must get the bot it had before this gate
// existed: nothing certified, nothing refused, an EMPTY log (a log is what
// reaches the next pass as a complaint).
func TestProductDocsCoverageGateInertWithoutANet(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	// A documentation that would fail every rule above — no anchor anywhere,
	// no exclusions chapter, invented paths — and a net that is NOT there.
	writeFile(t, ws, "docs/demo/p.md", "# Whatever\n\n## A chapter with no reference at all\n\nSee `/nowhere` and `042`.\n")
	var got coverageOut
	runJSON(t, coverageCommand(t, ws, "docs/demo", ""), &got)
	if !got.OK {
		t.Fatalf("the gate refused a product that carries no net: %s", got.Log)
	}
	if got.NetPresent || got.CauseCount != 0 || got.Log != "" {
		t.Fatalf("without a net the gate must be silent, got net_present=%v causes=%d log=%q", got.NetPresent, got.CauseCount, got.Log)
	}
}

// TestProductDocsCoverageGateDegradesVisiblyWithoutRoutes answers the one
// artifact golden-master does NOT commit. Its route table lives behind
// `config.json.routes_probe`, a command it replays at every gate, so the file
// this var names is usually absent. The check then falls back to the corpus
// alone — and says so: degraded is loud, and it still REFUSES.
func TestProductDocsCoverageGateDegradesVisiblyWithoutRoutes(t *testing.T) {
	requireGitPython(t)
	ws := newCoverageFixture(t)
	if err := os.Remove(filepath.Join(ws, ".golden-master/routes.txt")); err != nil {
		t.Fatal(err)
	}
	got := runCoverage(t, ws)
	if !got.OK {
		t.Fatalf("the degraded check refused a documentation whose every path the corpus observes:\n%s", got.Log)
	}
	if !got.Degraded || !strings.Contains(got.Log, "DEGRADED") {
		t.Fatalf("the degradation is not visible: degraded=%v log=%q", got.Degraded, got.Log)
	}
	// Degraded is not disabled: an invented path is still refused, and the
	// message says the check was narrower than it should have been.
	mutate(t, ws, "docs/demo/README.md", "## One item — [[ref:/dashboard/items/{id}]] [[ref:039]]",
		"## One item — [[ref:/dashboard/invented]] [[ref:039]]")
	got = runCoverage(t, ws)
	if got.OK {
		t.Fatalf("the degraded check went GREEN on an invented path — a degraded gate that refuses nothing is a disabled one:\n%s", got.Log)
	}
	if !strings.Contains(got.Log, "no declared route table was available") {
		t.Fatalf("the refusal does not say the check was degraded:\n%s", got.Log)
	}
}

// TestProductDocsCoverageGateReadsItsVocabularyFromTheNet: `001` is ONE
// campaign's convention. A net naming its entries differently is read exactly
// the same way — the gate never learns an id vocabulary, it reads citations.
func TestProductDocsCoverageGateReadsItsVocabularyFromTheNet(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	writeFile(t, ws, ".golden-master/corpus.json",
		`{"entries": [{"id": "checkout-pay", "method": "GET", "path": "/checkout/pay"}]}`)
	writeFile(t, ws, ".golden-master/feature-coverage.json",
		`{"features": [{"feature": "payment/card", "entries": ["checkout-pay"]}], "exclusions": []}`)
	writeFile(t, ws, "docs/demo/p.md", "# Pay\n\n## Paying — "+ref("/checkout/pay")+" "+ref("checkout-pay")+"\n\n"+
		ref("payment/card")+" "+ref("checkout-pay")+" is the only means of payment a customer\n"+
		"may use, and the order is confirmed on that same screen without a further step.\n")
	got := runCoverage(t, ws)
	if !got.OK {
		t.Fatalf("the gate did not read the net's own vocabulary:\n%s", got.Log)
	}
	// A CITATION the corpus does not carry is caught on a line citing nothing
	// else — and the same token as an ordinary code span on the same page is
	// not, because only a citation is a reference.
	writeFile(t, ws, "docs/demo/q.md", "# Refunds\n\nThe refund screen is recorded as `checkout-ref` and nothing else.\n")
	if got := runCoverage(t, ws); !got.OK {
		t.Fatalf("a code span sharing the corpus vocabulary was refused:\n%s", got.Log)
	}
	writeFile(t, ws, "docs/demo/q.md", "# Refunds\n\nThe refund screen is recorded as "+ref("checkout-ref")+" and nothing else.\n")
	got = runCoverage(t, ws)
	if got.OK || !strings.Contains(got.Log, "the reference checkout-ref is CITED and") {
		t.Fatalf("a citation the corpus does not carry slipped through:\n%s", got.Log)
	}
}

// TestProductDocsPageLintTolerATesTheDeclaredMarker: one declared token,
// honoured by every site that reads a heading. If the editorial lint called
// the anchorless marker a working note, the two gates would order the campaign
// to add and to remove the same characters and the run could never converge.
// The exemption is the EXACT token: any other comment is still a violation.
func TestProductDocsPageLintToleratesTheDeclaredMarker(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	writeFile(t, ws, "docs/demo/p.md", "# Method\n\n## How this was built "+defaultNoAnchorMarker+"\n\nNothing observed here.\n")
	var got lintOut
	runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
	if !got.LintOK {
		t.Fatalf("the editorial lint refused the anchorless marker the coverage gate requires: %+v", got.Violations)
	}
	writeFile(t, ws, "docs/demo/p.md", "# Method\n\n## How this was built "+defaultNoAnchorMarker+
		"\n\n<!-- ask the product team about this one -->\nNothing observed here.\n")
	runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
	if got.LintOK {
		t.Fatalf("the exemption widened into a licence: a genuine working note survived the lint")
	}
	// The marker and a genuine note on the SAME line: the exemption is the
	// comment that IS the token, not the line that carries one.
	writeFile(t, ws, "docs/demo/p.md", "# Method\n\n## How this was built "+defaultNoAnchorMarker+
		" <!-- ask the product team -->\n\nNothing observed here.\n")
	runJSON(t, lintCommand(t, ws, "docs/demo", allLintRules, ""), &got)
	if got.LintOK {
		t.Fatalf("a working note rode along on the marker's line: %+v", got.Violations)
	}
}

// TestProductDocsPageLintMarkerIsNotASwitchOnEveryRule: the declared token used
// to be stripped from every line BEFORE any rule read it, which made the token
// a switch on all of them — a marker declared as an opening HTML comment put
// out `html_comments`, one declared as `password` put out the secret scan. The
// exemption belongs to the rule it exists for, and to nothing else.
func TestProductDocsPageLintMarkerIsNotASwitchOnEveryRule(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	writeFile(t, ws, "docs/demo/p.md", "# Access\n\npassword: hunter2-9f3a81bc\n\n"+
		"<!-- ask the product team about this one -->\n")
	for _, marker := range []string{"<!--", "password", "hunter2-9f3a81bc"} {
		t.Run(marker, func(t *testing.T) {
			cmd := resolveCommand(t, toolCommand(t, "product-docs/main.bot", "page_lint"), map[string]string{
				"vars.workspace_dir":             ws,
				"input.product_dir":              "docs/demo",
				"input.oracle_path":              filepath.Join(ws, ".golden-master"),
				"vars.lint_rules":                allLintRules,
				"vars.extra_forbidden_headings":  "",
				"vars.coverage_no_anchor_marker": marker,
			})
			var got lintOut
			runJSON(t, cmd, &got)
			if got.LintOK {
				t.Fatalf("a declared marker %q disabled a rule it has nothing to do with", marker)
			}
			rules := map[string]bool{}
			for _, v := range got.Violations {
				rules[fmt.Sprint(v["rule"])] = true
			}
			if !rules["secret_material"] || !rules["html_comments"] {
				t.Fatalf("declaring %q silenced a rule: violations = %+v", marker, got.Violations)
			}
		})
	}
}

// TestProductDocsPageLintMarkerExemptionIsArmedByTheNet: "without a net the
// graph is unchanged" was true of the fail_log EXPRESSION and false of the
// VALUE — `lint_ok` went from false to true on the same page, because the
// exemption rode on a var rather than on the net that justifies it. The graph
// test stubs `page_lint`, so only the real node can see this.
func TestProductDocsPageLintMarkerExemptionIsArmedByTheNet(t *testing.T) {
	requireGitPython(t)
	ws := t.TempDir()
	writeFile(t, ws, "docs/demo/p.md", "# Method\n\n## How this was built "+defaultNoAnchorMarker+
		"\n\nNothing observed here.\n")
	var got lintOut
	runJSON(t, lintCommandWithNet(t, ws, "docs/demo", allLintRules, "", filepath.Join(ws, ".golden-master")), &got)
	if !got.LintOK {
		t.Fatalf("with a net, the marker coverage_check requires was called a working note: %+v", got.Violations)
	}
	runJSON(t, lintCommandWithNet(t, ws, "docs/demo", allLintRules, "", ""), &got)
	if got.LintOK {
		t.Fatalf("with NO net there is no gate to contradict, so the marker is a working note again — the node must be the one it was: %+v", got.Violations)
	}
}

// ─── the self-documenting catalog entry, and the net it carries ──────────
//
// `repos: [{id: …, path: "."}]` is the shape a CAMPAIGN generates to document
// its own repository: a source already on disk, named RELATIVE to the
// workspace, cloned over the filesystem with no forge, no network and no
// credential — and then redacted and read like any other source.
//
// The same front door resolves the golden-master net exactly once, in the
// workspace first and then in each clone, so the exhaustiveness gate and the
// campaign both learn about it from ONE place: a second derivation somewhere
// else is how the two end up disagreeing.
func TestProductDocsCatalogIngestLocalPathSource(t *testing.T) {
	requireGitPython(t)
	scratch := t.TempDir()
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	catalogFixture(t, ws, "catalog/demo",
		"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \".\"\n",
		`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"."}]}`+"\n")
	writeFile(t, ws, "docs/client/README.md", "# Demo\n")
	writeFile(t, ws, "src/app.ts", "export const submit = 'Send';\n")
	writeFile(t, ws, ".env", "SECRET_KEY=never-read-me\n")
	writeFile(t, ws, ".golden-master/corpus.json", coverageCorpus)
	writeFile(t, ws, ".golden-master/feature-coverage.json", coverageInventory)
	gitIn(t, ws, "add", "-A")
	gitIn(t, ws, "commit", "-q", "-m", "seed")

	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)

	if got.OKCount != 1 || got.Degraded != 0 {
		t.Fatalf("inventory = %d ok / %d degraded, want 1/0 — a local source needs no forge: %s", got.OKCount, got.Degraded, got.Log)
	}
	clone, _ := got.Inventory[0]["path"].(string)
	if !strings.HasPrefix(clone, scratch) {
		t.Fatalf("the local source landed at %q, not under the scratch dir — it must be cloned OUT of the docs worktree", clone)
	}
	if _, err := os.Stat(filepath.Join(clone, "src/app.ts")); err != nil {
		t.Fatalf("the local clone is missing the source the campaign must read: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".env")); err == nil {
		t.Fatalf(".env survived into the local clone — a local source is redacted like any other")
	}
	if _, err := os.Stat(filepath.Join(clone, ".git")); err == nil {
		t.Fatalf("the local clone kept its history — the redacted blobs stay one `git show` away")
	}
	// The source tree itself is never touched: the redaction runs on the copy.
	if _, err := os.Stat(filepath.Join(ws, ".env")); err != nil {
		t.Fatalf("redacting the clone deleted the file from the SOURCE tree: %v", err)
	}
	if got.OraclePath != filepath.Join(ws, ".golden-master") {
		t.Fatalf("oracle_path = %q, want the net in the workspace", got.OraclePath)
	}
	if !strings.Contains(got.Log, "exhaustiveness gate is ARMED") {
		t.Fatalf("the front door does not say it armed the gate: %s", got.Log)
	}
}

// A path is confined to the workspace, and must be a repository: the catalog
// is repo content, so every one of these is a NAMED degraded entry rather
// than a tree nobody chose to expose.
func TestProductDocsCatalogIngestRefusesAnUnsafeLocalPath(t *testing.T) {
	requireGitPython(t)
	outside := t.TempDir()
	for _, tc := range []struct{ name, path, want string }{
		{"absolute", outside, "never absolute"},
		{"escapes the workspace", "../..", "escapes the docs workspace"},
		{"not a repository", "src", "is not a git repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := t.TempDir()
			ws := t.TempDir()
			gitIn(t, ws, "init", "-q", "-b", "main")
			catalogFixture(t, ws, "catalog/demo",
				"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo\n    path: \""+tc.path+"\"\n",
				`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo","path":"`+tc.path+`"}]}`+"\n")
			writeFile(t, ws, "docs/client/README.md", "# Demo\n")
			writeFile(t, ws, "src/app.ts", "export const x = 1;\n")
			gitIn(t, ws, "add", "-A")
			gitIn(t, ws, "commit", "-q", "-m", "seed")

			var got ingestOut
			runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
			if got.OKCount != 0 || got.Degraded != 1 {
				t.Fatalf("inventory = %d ok / %d degraded, want 0/1: %s", got.OKCount, got.Degraded, got.Log)
			}
			note, _ := got.Inventory[0]["note"].(string)
			if !strings.Contains(note, tc.want) {
				t.Fatalf("the refusal does not name its cause %q: %q", tc.want, note)
			}
		})
	}
}

// The net may live in the SOURCE repository rather than in the docs repo —
// that is where golden-master commits it. One resolver covers both, and
// reports nothing found rather than guessing.
func TestProductDocsCatalogIngestFindsTheNetInASourceClone(t *testing.T) {
	requireGitPython(t)
	// The docs repo carries no net of its own; the SOURCE does. `wholeNet`
	// false commits the corpus alone — half a net is no net.
	newWS := func(t *testing.T, wholeNet bool) (string, string) {
		t.Helper()
		ws := t.TempDir()
		gitIn(t, ws, "init", "-q", "-b", "main")
		source := newLocalSource(t, ws)
		writeFile(t, source, ".golden-master/corpus.json", coverageCorpus)
		if wholeNet {
			writeFile(t, source, ".golden-master/feature-coverage.json", coverageInventory)
		}
		gitIn(t, source, "add", "-A")
		gitIn(t, source, "commit", "-q", "-m", "net")
		catalogFixture(t, ws, "catalog/demo",
			"id: demo\ndocs:\n  product_dir: docs/client\nrepos:\n  - id: demo-src\n    url: "+source+"\n",
			`{"id":"demo","docs":{"product_dir":"docs/client"},"repos":[{"id":"demo-src","url":"`+source+`"}]}`+"\n")
		writeFile(t, ws, "docs/client/README.md", "# Demo\n")
		gitIn(t, ws, "add", "-A")
		gitIn(t, ws, "commit", "-q", "-m", "seed")
		return ws, t.TempDir()
	}

	ws, scratch := newWS(t, true)
	var got ingestOut
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	if got.OraclePath != filepath.Join(scratch, "sources", "demo-src", ".golden-master") {
		t.Fatalf("oracle_path = %q, want the net inside the source clone", got.OraclePath)
	}

	// Half a net is no net: the gate arms on BOTH artifacts or on neither,
	// because a corpus with no inventory certifies nothing.
	ws, scratch = newWS(t, false)
	runJSON(t, ingestCommand(t, ws, "catalog", "demo", scratch), &got)
	if got.OraclePath != "" {
		t.Fatalf("oracle_path = %q with only half a net, want empty", got.OraclePath)
	}
	if !strings.Contains(got.Log, "exhaustiveness gate stays inert") {
		t.Fatalf("the front door does not say the gate stays inert: %s", got.Log)
	}
}
