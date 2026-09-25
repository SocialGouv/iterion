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

// hostRe captures the host of a URL or of an ssh/git remote written with a
// user. `git://` and `git+ssh://` are here because a forge that serves them
// carries an identity exactly as much as one that serves https.
var hostRe = regexp.MustCompile(
	`(?:https?://|git://|git\+ssh://|ssh://[A-Za-z0-9._-]+@|ssh://|[A-Za-z0-9._-]+@)([A-Za-z0-9][A-Za-z0-9._-]*\.[A-Za-z]{2,24})`)

// scpRe captures the scp-form git remote written WITHOUT a user —
// `forge.internal.acme.fr:team/app.git`, which is what a clone line copied out
// of a forge's UI looks like. The `.git` suffix is required: a bare
// `host:path` shape is indistinguishable from a filename and a line number,
// and a guard that flagged those would be worked around within a day.
var scpRe = regexp.MustCompile(
	"(?:^|[\\s\"'`(<])([A-Za-z0-9][A-Za-z0-9._-]*\\.[A-Za-z]{2,24}):[A-Za-z0-9._~/-]+\\.git\\b")

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

	// ── everything below entered with the scope, and each one is a review
	// decision taken once: "is this host public infrastructure, or is it
	// somebody's?" They are grouped by what they are, not by where they sit.

	// THIS PROJECT'S OWN platform, already public in the documentation it
	// documents. They are named here rather than matched by a suffix rule: a
	// suffix would admit a subdomain nobody reviewed.
	"socialgouv.github.io":                                  "this project's own public documentation site",
	"social.gouv.fr":                                        "the organisation this project belongs to",
	"iterion.cloud":                                         "this project's own public service",
	"iterion.fabrique.social.gouv.fr":                       "this project's own deployment, named in its runbooks",
	"buildd.bko.fabrique.social.gouv.fr":                    "this project's own build host, named in its runbooks",
	"sentry2.fabrique.social.gouv.fr":                       "this project's own error-reporting host, named in its runbooks",
	"iterion-app-boite-a-idees.ovh.fabrique.social.gouv.fr": "this project's own demo deployment, named in a runbook",
	"evil.fabrique.social.gouv.fr":                          "a deliberately hostile URL in a security doc's worked example",

	// Model providers and agent runtimes the bots talk to or document.
	"api.anthropic.com":     "the model provider's public API",
	"console.anthropic.com": "the model provider's public console",
	"platform.claude.com":   "the model provider's public platform",
	"code.claude.com":       "the public documentation of an agent runtime",
	"claude.ai":             "the model provider's public product page",
	"api.openai.com":        "a model provider's public API",
	"auth.openai.com":       "a model provider's public auth endpoint",
	"developers.openai.com": "a model provider's public developer documentation",
	"api.x.ai":              "a model provider's public API",
	"api.z.ai":              "a model provider's public API",
	"models.dev":            "a public model index a backend reads",
	"huggingface.co":        "a public model hub",
	"opencode.ai":           "a public agent runtime named in a comparison",
	"pi.dev":                "the public site of an execution backend named in an ADR",

	// Public documentation of the tools and platforms this project uses.
	"go.dev":                     "the Go project's public site",
	"kubernetes.io":              "the public Kubernetes documentation",
	"keda.sh":                    "the public documentation of an autoscaler a chart depends on",
	"velero.io":                  "the public documentation of a backup tool named in a runbook",
	"nats-io.github.io":          "the public chart repository of a message broker a chart depends on",
	"direnv.net":                 "the public site of a shell tool the developer setup names",
	"www.jetify.com":             "the public site of the devbox toolchain",
	"registry.npmjs.org":         "the public npm registry",
	"deb.nodesource.com":         "a public Debian package repository named in a setup script",
	"s3.amazonaws.com":           "a public object-store endpoint form in a configuration example",
	"docs.github.com":            "GitHub's public documentation",
	"docs.gitlab.com":            "GitLab's public documentation",
	"docs.gravatar.com":          "a public avatar service's documentation",
	"users.noreply.github.com":   "GitHub's own no-reply mail domain",
	"wails.io":                   "the public site of a desktop toolkit the desktop build uses",
	"cdn.prod.website-files.com": "a public CDN serving a logo named in a comparison's asset list",

	// Standards and references cited in documentation.
	"www.w3.org":       "a public standards body",
	"json-schema.org":  "the public JSON Schema specification",
	"publicsuffix.org": "the public suffix list",
	"man7.org":         "public manual pages cited in a runbook",
	"arxiv.org":        "a public preprint server cited in a document",

	// Competing products named in a published comparison. Naming a competitor's
	// PUBLIC documentation is the point of a comparison; none of these is
	// anybody's private infrastructure.
	"n8n.io":                     "a competing product's public site, named in a comparison",
	"docs.n8n.io":                "a competing product's public documentation",
	"zapier.com":                 "a competing product's public site, named in a comparison",
	"help.zapier.com":            "a competing product's public help site",
	"community.zapier.com":       "a competing product's public community site",
	"www.make.com":               "a competing product's public site, named in a comparison",
	"help.make.com":              "a competing product's public help site",
	"apps.make.com":              "a competing product's public app directory",
	"crewai.com":                 "a competing product's public site, named in a comparison",
	"www.crewai.com":             "a competing product's public site, named in a comparison",
	"docs.crewai.com":            "a competing product's public documentation",
	"enterprise-docs.crewai.com": "a competing product's public documentation",
	"dify.ai":                    "a competing product's public site, named in a comparison",
	"docs.dify.ai":               "a competing product's public documentation",
	"flowiseai.com":              "a competing product's public site, named in a comparison",
	"docs.flowiseai.com":         "a competing product's public documentation",
	"www.langchain.com":          "a competing product's public site, named in a comparison",
	"docs.langchain.com":         "a competing product's public documentation",
	"www.activepieces.com":       "a competing product's public site, named in a comparison",
	"www.windmill.dev":           "a competing product's public site, named in a comparison",

	// Placeholders and local names that resolve to nobody.
	"host.docker.internal":    "the container runtime's own name for the host, in a configuration example",
	"tempo.observability":     "an in-cluster service name in a configuration example",
	"our.host":                "a placeholder hostname in a worked example",
	"jira-mcas.atlassian.net": "a placeholder tenant in a ticket-tracker example",

	// File NAMES that parse as hostnames and are not. They reach the allowlist
	// by the same door as everything else: a review decision, taken once, with
	// the reason written down.
	"iterion-author.v1.schema": "a versioned schema FILE NAME under docs/references, not a host",
	"iterion-author.v2.schema": "a versioned schema FILE NAME under docs/references, not a host",
}

type foreignHost struct {
	file, host string
	line       int
}

// pathSegmentHostRe matches a path segment that IS a hostname: at least three
// labels, the last of them alphabetic. Two dots is the threshold that keeps
// `docker-compose.override.yml` and `values.ovh-prod.yaml` out — one dot is
// how a file names a variant, two is how a host names a subdomain. What gets
// through anyway is a review decision like any other: the allowlist carries it.
var pathSegmentHostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*(?:\.[A-Za-z0-9-]+)+\.[A-Za-z]{2,24}$`)

// hostSegments returns the segments of a path that are themselves hostnames,
// one trailing extension stripped.
func hostSegments(path string) []string {
	var found []string
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == "" || segment == ".." {
			continue
		}
		candidate := segment
		if dot := strings.LastIndex(segment, "."); dot > 0 {
			candidate = segment[:dot]
		}
		if pathSegmentHostRe.MatchString(candidate) {
			found = append(found, strings.ToLower(candidate))
		}
	}
	return found
}

// permittedHost is the ONE decision procedure, used by every caller. A second
// copy would drift and exempt something nobody meant to exempt.
func permittedHost(host string) bool {
	if _, ok := catalogHostAllowlist[host]; ok {
		return true
	}
	for _, suffix := range reservedSuffixes {
		// DNS BOUNDARY, not a string suffix: example.com reserves the
		// DOMAIN, and customerexample.com is a different domain somebody
		// else owns. A reserved entry matches itself, or a subdomain of
		// itself — a label boundary, never a loose string suffix.
		name := strings.TrimPrefix(suffix, ".")
		if host == name || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}

// catalogScope is every tracked path this guard judges: the whole surface a
// working session touches and then publishes. `bots/` and `examples/` were the
// original set, and they are the smallest half of it — a hostname copied out
// of a session lands just as easily in an ADR, a runbook, a chart value, a CI
// step or a shell script, and each of those ships to the same public
// repository.
//
// `:(glob)*.md` is the ROOT markdown only: `*.md` as a plain pathspec would
// sweep in `vendor/`, where several hundred third-party READMEs name several
// hundred hosts nobody here chose.
var catalogScope = []string{"bots", "examples", "docs", "skills", "scripts", "charts", "ci", ":(glob)*.md"}

// catalogFiles is the set this guard judges: the tracked files of
// catalogScope, read from git rather than from the checkout.
//
// The distinction is not pedantry, it is the finding that made this function
// exist. A working directory carries `.devbox/` profiles, `__pycache__/`
// artefacts and build output that no commit holds; scanning it made the guard
// certify the checkout instead of the tree, and the allowlist grew two entries
// justified by files that only existed on one machine. The CI run named both.
// A guard whose verdict moves with the directory it runs in certifies nothing.
func catalogFiles(t *testing.T) []string {
	t.Helper()
	return trackedFilesIn(t, "..", catalogScope...)
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
		// THE NAME IS CONTENT TOO. A file called `forge.internal.acme.fr.json`
		// carries the identity in the one place a scan of bodies never looks,
		// and a directory named after a forge carries it for every file under
		// it. The URL, scp and email shapes apply to the path as line 0;
		// hostSegments adds the shape a path has and a line of prose does not
		// — a segment that IS a hostname.
		for _, host := range hostSegments(path) {
			if !permittedHost(host) {
				found = append(found, foreignHost{file: path, host: host, line: 0})
			}
		}
		lines := append([]string{filepath.ToSlash(path)}, strings.Split(string(body), "\n")...)
		for index, line := range lines {
			hosts := hostRe.FindAllStringSubmatch(line, -1)
			hosts = append(hosts, emailDomainRe.FindAllStringSubmatch(line, -1)...)
			hosts = append(hosts, scpRe.FindAllStringSubmatch(line, -1)...)
			for _, match := range hosts {
				host := strings.ToLower(strings.TrimSuffix(match[1], "."))
				if permittedHost(host) {
					continue
				}
				found = append(found, foreignHost{file: path, host: host, line: index})
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
		"a-skill.md": "Clone it with `git clone git@forge.internal.acme-industries.fr:team/app.git`.\n",
		"a-bot.bot":  "  url: string = \"https://gitlab.private-customer.example.fr/group/repo\"\n",
		"a-test.go":  "\tconst author = \"operator@private-customer.fr\"\n",
		// The shapes the first version of this guard did not see.
		"a-runbook.md": "Mirror it: `git clone git://mirror.private-customer-two.fr/team/app.git`.\n",
		"a-script.sh":  "git remote add upstream scm.private-customer-three.fr:team/app.git\n",
		"a-chart.yaml": "  repository: ssh://deploy@registry.private-customer-four.fr/charts\n",
		// And the one place a scan of file BODIES never looks.
		"forge.private-customer-five.fr.json": "nothing in the body at all\n",
		"innocent.md":                         "See https://github.com/SocialGouv/iterion and mail t@example.invalid.\n",
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
		"mirror.private-customer-two.fr",
		"scm.private-customer-three.fr",
		"registry.private-customer-four.fr",
		"forge.private-customer-five.fr",
	} {
		if !caught[host] {
			t.Errorf("the guard did NOT catch %q — it would let a third party's infrastructure "+
				"ship in the public catalog", host)
		}
	}
}

// THE SCOPE IS WHAT THE GUARD IS WORTH. It judged `bots/` and `examples/`
// only, which is the smallest half of what a working session touches and
// publishes: an ADR, a runbook, a chart value, a CI step and a shell script
// all ship to the same public repository, and a hostname copied out of a
// session lands in one of those as easily as in a fixture.
//
// A scope that silently covered nothing would make every test here green, so
// each declared directory is required to contribute a file.
func TestCatalogIdentityScopeCoversEveryPublishedSurface(t *testing.T) {
	files := catalogFiles(t)
	for _, want := range catalogScope {
		if strings.HasPrefix(want, ":(glob)") {
			continue
		}
		found := false
		for _, path := range files {
			if strings.Contains(filepath.ToSlash(path), "/"+want+"/") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the guard declares it judges %q and git lists no tracked file under it — "+
				"a scope that covers nothing makes every assertion here green for free", want)
		}
	}
	// The root markdown, which is the one entry the loop above skips: its
	// pathspec exists precisely so `vendor/` does not come with it.
	rootMarkdown, vendored := false, false
	for _, path := range files {
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../")
		if !strings.Contains(rel, "/") && strings.HasSuffix(rel, ".md") {
			rootMarkdown = true
		}
		if strings.HasPrefix(rel, "vendor/") {
			vendored = true
		}
	}
	if !rootMarkdown {
		t.Error("no root markdown is judged — README, SECURITY and the changelog publish hostnames too")
	}
	if vendored {
		t.Error("the scope swept in vendor/, where several hundred third-party READMEs name " +
			"several hundred hosts nobody here chose — the allowlist would become a copy of the internet")
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

// A reserved name reserves ITS DOMAIN, not any string it happens to end
// with: customerexample.com and private-example.org are somebody's real
// domains, and the loose suffix read let them through without an entry.
func TestCatalogReservedDomainsCarryADNSBoundary(t *testing.T) {
	for _, host := range []string{
		"customerexample.com", "private-example.org", "notexample.net",
	} {
		if permittedHost(host) {
			t.Errorf("%s passed on a loose string suffix — it is nobody's reserved domain", host)
		}
	}
	for _, host := range []string{
		"example.com", "api.example.com", "deep.sub.example.org",
		"service.test", "my.service.test", "box.local",
	} {
		if !permittedHost(host) {
			t.Errorf("%s refused — a reserved domain or its own subdomain must pass", host)
		}
	}
}
