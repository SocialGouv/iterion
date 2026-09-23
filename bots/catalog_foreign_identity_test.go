package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	"cache.nixos.org":           "the public Nix binary cache, named in devbox locks",
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
	"golden-master.iterion":  "the synthetic committer identity of a bot's own commits",
	"golden-master.iterionz": "the same identity, in a bundle-name fixture",
	"iterion.local":          "the synthetic committer identity of a bot's own commits",
	"noreply.local":          "the synthetic committer identity of a bot's own commits",
}

type foreignHost struct {
	file, host string
	line       int
}

// scanForForeignHosts walks a directory and returns every host reference that
// is neither reserved nor allowlisted. Factored out of the test so the guard
// can be shown to BITE on an injected example — a bench that cannot go red is
// not a bench.
func scanForForeignHosts(t *testing.T, root string, skip func(path string) bool) []foreignHost {
	t.Helper()
	var found []foreignHost
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if skip != nil && skip(path) {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
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
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

func TestCatalogNamesNoForeignInfrastructure(t *testing.T) {
	skip := func(path string) bool {
		// This file carries the allowlist, so it names every host by
		// construction. Nothing else is exempt — a fixture in a test is as
		// public as a skill.
		return filepath.Base(path) == "catalog_foreign_identity_test.go"
	}
	var found []foreignHost
	for _, root := range []string{".", "../examples"} {
		found = append(found, scanForForeignHosts(t, root, skip)...)
	}
	for _, hit := range found {
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

	found := scanForForeignHosts(t, dir, nil)
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
	for _, root := range []string{".", "../examples"} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || filepath.Base(path) == "catalog_foreign_identity_test.go" {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			lowered := strings.ToLower(string(body))
			for host := range catalogHostAllowlist {
				if strings.Contains(lowered, host) {
					seen[host] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	for host, reason := range catalogHostAllowlist {
		if !seen[host] {
			t.Errorf("the allowlist permits %q (%q) and nothing in the catalog names it any more — "+
				"remove the entry so it cannot silently re-permit that host later", host, reason)
		}
	}
}
