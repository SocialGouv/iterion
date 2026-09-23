package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// A catalog bot ships to a PUBLIC repository and is read by anyone. Nothing in
// it may carry the identity of a third party a contributor happens to work
// for: a private forge, a customer's deployment, an operator's address. That
// leak does not announce itself — it arrives as a hostname copied out of a
// working session into a fixture, and nobody re-reads a fixture.
//
// Until this guard there was NOTHING in the tree checking for it: the
// universality guards next door check that a bot is not scoped to iterion's
// own layout or to one language, which is a different question entirely.
//
// WHAT THIS PROVES, and what it does not. A guard that tried to recognise "a
// client name" would be enumerating spellings, and a guard that enumerates
// spellings does not converge — the next payload is always one spelling
// further out. So this one inverts the question and takes a POSITIVE list, over
// the three shapes that actually carry an identity:
//
//   - a URL host (`https://…`, `git@…:`, `ssh://git@…`)
//   - the domain of an email address
//   - nothing else
//
// Every host outside the list below fails the test, so a NEW one is a decision
// somebody takes in review rather than a line nobody reads. What it does NOT
// prove: a person's name, a product codename written in plain words, a client
// named in prose. Those have no shape to match, and pretending otherwise would
// put a green tick next to a question nobody answered.

// hostRe captures the host of a URL, a scp-form git remote, or an ssh remote.
var hostRe = regexp.MustCompile(`(?:https?://|git@|ssh://git@)([A-Za-z0-9][A-Za-z0-9._-]*\.[A-Za-z]{2,24})`)

// emailDomainRe captures the domain of an email address.
var emailDomainRe = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@((?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,24})\b`)

// reservedSuffixes are the names reserved by RFC 2606 / RFC 6761 for
// documentation and testing. They cannot resolve to anybody's infrastructure,
// which is exactly why a fixture should use them — and why they need no
// per-name entry here.
var reservedSuffixes = []string{
	".example", ".invalid", ".local", ".test", ".localhost",
	"example.com", "example.org", "example.net",
}

// catalogHostAllowlist is the POSITIVE list: every host a shipped catalog file
// may name, with the reason it is safe to publish. Adding an entry is a review
// decision — "is this host public infrastructure, or is it somebody's?"
var catalogHostAllowlist = map[string]string{
	// Forges and package infrastructure the bots genuinely talk to.
	"github.com":                "the public forge this project is hosted on",
	"api.github.com":            "GitHub's public API",
	"raw.githubusercontent.com": "GitHub's public raw-content host",
	"gitlab.com":                "the public GitLab instance",
	"codeberg.org":              "a public forge",
	"nixhub.io":                 "the public devbox package index, named in devbox locks",
	"json.schemastore.org":      "the public JSON schema store",
	"dl.min.io":                 "a public download host for an object-store client",
	// Public vulnerability and standards sources the security bots read.
	"api.osv.dev":         "the public OSV vulnerability API",
	"nvd.nist.gov":        "the public national vulnerability database",
	"www.cisa.gov":        "a public agency advisory source",
	"api.first.org":       "the public CVSS/EPSS API",
	"krebsonsecurity.com": "a public security news source in a feed list",
	// Public accessibility and design references.
	"accessibilite.numerique.gouv.fr": "the public accessibility reference the a11y bots cite",
	"www.cert.ssi.gouv.fr":            "a public advisory source",
	"www.systeme-de-design.gouv.fr":   "a public design-system reference",
	// Public documentation of tools a skill names.
	"docs.fallow.tools": "the public documentation of a static analyser named in a skill",
	// Placeholders that are not reserved names but name nobody: they appear in
	// prose as "replace this with yours".
	"myorg.atlassian.net": "a placeholder tenant in a ticket-tracker skill's example",
	"domaine.fr":          "a placeholder domain inside a French UI error-message example",
	"x.fr":                "a placeholder URL in a truncation test fixture",
	// Identities the engine writes into throwaway git commits. They are not
	// hosts: nothing resolves them, and they exist so a commit has an author.
	"golden-master.iterion": "the synthetic committer identity of a bot's own commits",
	"iterion.local":         "the synthetic committer identity of a bot's own commits",
	"noreply.local":         "the synthetic committer identity of a bot's own commits",
}

type foreignHost struct {
	file, host string
	line       int
}

// catalogFiles is the set this guard judges: the files of bots/ and examples/
// that are IN THE COMMIT, read from git rather than from the checkout.
//
// The distinction is not pedantry, it is the finding that made this function
// exist. A working directory carries `.devbox/` profiles, `__pycache__/`
// artefacts and build output that no commit holds; scanning it made the guard
// certify the checkout instead of the tree, and the allowlist grew two entries
// justified by files that only existed on one machine. The CI run named both.
// A guard whose verdict moves with the directory it runs in certifies nothing.
func catalogFiles(t *testing.T) []string {
	t.Helper()
	return trackedFilesIn(t, "..", "bots", "examples")
}

// trackedFilesIn lists the files git TRACKS under the named subdirectories of
// root, as paths relative to the caller. Taking root as a parameter is what
// lets the property below be proven on a throwaway repository: a test that
// wrote an untracked file into THIS checkout to prove it would race every
// other package's test that reads the tree — pkg/repomap's freshness gate
// among them, which is how CI found this the first time.
func trackedFilesIn(t *testing.T, root string, subdirs ...string) []string {
	t.Helper()
	args := append([]string{"ls-files", "-z", "--"}, subdirs...)
	out, err := gittest.Cmd(root, args...).Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}
	var files []string
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel != "" {
			files = append(files, filepath.Join(root, filepath.FromSlash(rel)))
		}
	}
	if len(files) == 0 {
		t.Fatalf("git lists no file under %v in %s — the guard would pass by scanning nothing", subdirs, root)
	}
	return files
}

// scanForeignHosts returns every host reference in the named files that is
// neither reserved nor allowlisted. Factored out of the test so the guard can
// be shown to BITE on injected examples — a bench that cannot go red is not a
// bench.
func scanForeignHosts(t *testing.T, files []string, skip func(path string) bool) []foreignHost {
	t.Helper()
	var found []foreignHost
	for _, path := range files {
		if skip != nil && skip(path) {
			continue
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		for index, line := range strings.Split(string(body), "\n") {
			hosts := hostRe.FindAllStringSubmatch(line, -1)
			hosts = append(hosts, emailDomainRe.FindAllStringSubmatch(line, -1)...)
			for _, match := range hosts {
				host := strings.ToLower(strings.TrimSuffix(match[1], "."))
				if _, ok := catalogHostAllowlist[host]; ok {
					continue
				}
				reserved := false
				for _, suffix := range reservedSuffixes {
					if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
						reserved = true
						break
					}
				}
				if reserved {
					continue
				}
				found = append(found, foreignHost{file: path, host: host, line: index + 1})
			}
		}
	}
	return found
}

// skipTheGuardItself: this file carries the allowlist and the injected
// examples of the bite test, so it names every host by construction. ONE
// definition, used by every test here — a second copy would drift and exempt
// something nobody meant to exempt. Nothing else is exempt: a fixture in a
// test is as public as a skill.
func skipTheGuardItself(path string) bool {
	return filepath.Base(path) == "catalog_foreign_identity_test.go"
}

func TestCatalogNamesNoForeignInfrastructure(t *testing.T) {
	for _, hit := range scanForeignHosts(t, catalogFiles(t), skipTheGuardItself) {
		t.Errorf("%s:%d names the host %q, which is neither a reserved documentation name nor "+
			"on the catalog allowlist. A shipped bot must not carry a third party's "+
			"infrastructure: if this host is public and the reference is deliberate, add it to "+
			"catalogHostAllowlist with the reason; if it came out of a working session, remove it.",
			hit.file, hit.line, hit.host)
	}
}

// The guard is shown to BITE. A bench that cannot go red is not a bench: this
// one injects the shapes a leak actually takes and requires each to be caught.
func TestCatalogForeignIdentityGuardBites(t *testing.T) {
	dir := t.TempDir()
	fixtures := map[string]string{
		"a-skill.md":  "Clone it with `git clone git@forge.internal.acme-industries.fr:team/app.git`.\n",
		"a-bot.bot":   "  url: string = \"https://gitlab.private-customer.example.fr/group/repo\"\n",
		"a-test.go":   "\tconst author = \"operator@private-customer.fr\"\n",
		"innocent.md": "See https://github.com/SocialGouv/iterion and mail t@example.invalid.\n",
	}
	for name, body := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var injected []string
	for name := range fixtures {
		injected = append(injected, filepath.Join(dir, name))
	}
	found := scanForeignHosts(t, injected, nil)
	caught := map[string]bool{}
	for _, hit := range found {
		caught[hit.host] = true
		if filepath.Base(hit.file) == "innocent.md" {
			t.Errorf("the guard flagged %q in a file naming only public infrastructure and a "+
				"reserved name — a guard that refuses the legitimate case gets worked around",
				hit.host)
		}
	}
	for _, host := range []string{
		"forge.internal.acme-industries.fr",
		"gitlab.private-customer.example.fr",
		"private-customer.fr",
	} {
		if !caught[host] {
			t.Errorf("the guard did NOT catch %q — it would let a third party's infrastructure "+
				"ship in the public catalog", host)
		}
	}
}

// An allowlist entry that no longer matches anything is debt with a reason
// attached to nothing: it silently re-permits a host somebody removed on
// purpose. Drained the same way the universality exemptions are.
func TestCatalogHostAllowlistCarriesNoDeadEntry(t *testing.T) {
	seen := map[string]bool{}
	for _, path := range catalogFiles(t) {
		if skipTheGuardItself(path) {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lowered := strings.ToLower(string(body))
		for host := range catalogHostAllowlist {
			if strings.Contains(lowered, host) {
				seen[host] = true
			}
		}
	}
	for host, reason := range catalogHostAllowlist {
		if !seen[host] {
			t.Errorf("the allowlist permits %q (%q) and nothing in the catalog names it any more — "+
				"remove the entry so it cannot silently re-permit that host later", host, reason)
		}
	}
}

// The regression CI caught, pinned — on a throwaway repository, because the
// first attempt to pin it wrote an untracked file into THIS checkout and
// pkg/repomap's freshness gate, running in parallel, counted it as a ninth
// skill. A guard proven by dirtying the tree it guards is a flake with a
// motive.
//
// The defect itself: the guard walked the working DIRECTORY, so its verdict
// moved with whatever was lying around. A `.devbox/` profile and a
// `__pycache__/` artefact — in no commit — justified two allowlist entries,
// and a clean checkout found both dead.
func TestCatalogIdentityGuardJudgesTheCommitNotTheCheckout(t *testing.T) {
	requireAssessmentTools(t)
	root := t.TempDir()
	gittest.Run(t, root, "init", "-q", "-b", "main")
	gittest.Run(t, root, "config", "user.email", "t@example.com")
	gittest.Run(t, root, "config", "user.name", "t")
	gittest.Run(t, root, "config", "commit.gpgsign", "false")
	if err := os.MkdirAll(filepath.Join(root, "bots", "sample", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	committed := filepath.Join("bots", "sample", "skills", "tracked.md")
	if err := os.WriteFile(filepath.Join(root, committed), []byte("public: https://github.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", committed)
	gittest.Run(t, root, "commit", "-qm", "one tracked file")

	// The artefacts a working directory accumulates, none of them in the commit.
	for rel, body := range map[string]string{
		filepath.Join("bots", "sample", ".devbox", "gen", "flake.nix"): "https://cache.nixos.example/x\n",
		filepath.Join("bots", "sample", "__pycache__", "x.pyc"):        "git@forge.internal.acme-industries.fr:team/app.git\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files := trackedFilesIn(t, root, "bots")
	if len(files) != 1 || !strings.HasSuffix(files[0], "tracked.md") {
		t.Fatalf("the judged set is %v — it is reading the checkout, not the commit", files)
	}

	// And the consequence, both ways round: an untracked file can neither
	// excuse an allowlist entry nor hide a leak.
	if found := scanForeignHosts(t, files, nil); len(found) != 0 {
		t.Fatalf("the committed file names a foreign host it should not: %v", found)
	}
	walked := []string{
		filepath.Join(root, "bots", "sample", "__pycache__", "x.pyc"),
	}
	if found := scanForeignHosts(t, walked, nil); len(found) == 0 {
		t.Fatal("the scanner does not see the private forge in the untracked artefact — " +
			"then the guard is blind, not merely scoped")
	}
}
