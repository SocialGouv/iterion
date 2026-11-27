package catalogcorpus

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A node's `skills: [...]` list names skills BY NAME, and nothing in the
// toolchain checks that a name resolves.
//
// `iterion validate` compiles the graph and lints the bundle's skill
// FILES (C231–C234: frontmatter present, description not terse, names
// unique) — but it never joins the two. A name in `skills:` that matches
// no file is not a compile error and not a lint finding; the runtime
// simply mirrors nothing for it, and the agent's Skill tool comes up
// empty on a skill the prompt told it to load.
//
// That failure is silent in the worst way: the bot still answers. It just
// answers from the model's own priors instead of the bundle's authored
// knowledge, which reads as "the bot got dumber" rather than as a typo.
// Same family as the `_session_id` trap the DSL-authoring skill warns
// about — a load-bearing string the compiler does not check.
//
// So: every name declared in a catalog bot's `skills:` list must exist as
// `<bundle>/skills/<name>.md`, with matching `name:` frontmatter — or be
// DECLARED as a shared dependency the install materializes at exactly that
// path (`dependencies.skills`, ADR-144 clause 2), which this gate holds to
// the same promise, against the producer in this very tree.
//
// EXCEPT that `skills:` resolves from three places, not one — bundle,
// plugin, then the operator-curated skill LIBRARY (ADR-059), in that
// precedence. A bot may therefore reference a skill it deliberately does
// NOT ship, so the operator supplies it. That is a design choice, not a
// typo, and the two are indistinguishable from the name alone — hence the
// allowlist below rather than a cleverer rule. Adding to it is a decision
// to be defended in review; the entry carries its reason.
var operatorAttachedSkills = map[string]string{
	// app-dev (Appy) and review-env deploy to a platform they refuse to
	// know: the playbook — auth, provisioning, how to derive the public
	// URL — is the skill the operator attaches, alongside a mounted
	// credential. Shipping one would put a platform literal in a catalog
	// bot, which is exactly what "catalog bots are repo-agnostic" forbids.
	// Both bots handle its absence explicitly.
	"deploy-target": "operator-attached platform playbook; shipping one would pin the bot to a platform",
	// product-docs (Prody) publishes the same way: the Onyxia/SSPCloud
	// HOW is an org-private skill the operator attaches. Shipping it
	// would pin a catalog bot to one datalab. Absence is handled by
	// publish_gate + optional secrets; swap the skill to swap target.
	"deploy-onyxia-sspcloud": "org-private operator-attached Onyxia-SSPCloud publish playbook; product-docs handles its absence via publish_gate / optional secrets",
}

// skillsListRe matches a node's inline skills list. Lists in this DSL are
// inline arrays — a multi-line YAML sequence does not parse (see the
// iterion-dsl-authoring skill), so one line always holds the whole list.
var skillsListRe = regexp.MustCompile(`^\s+skills:\s*\[([^\]]*)\]`)

// skillFrontmatterNameRe reads `name:` out of a SKILL.md's frontmatter.
var skillFrontmatterNameRe = regexp.MustCompile(`(?m)^name:\s*(\S+)\s*$`)

func TestCatalogSkillReferencesResolve(t *testing.T) {
	// Every shipped workflow, not only each entry: a skills: list resolves
	// against the DECLARING file's directory, so a subbot child and a bundle
	// sibling owe the gate exactly what a main.bot owes — and the guard that
	// globbed mains alone passed over every child's declarations. The
	// corpus table is floored per root, where the globs had an emptiness
	// check.
	all := catalogWorkflowFiles(t)

	checked := 0
	for _, botPath := range all {
		src, err := botUnitSource(botPath)
		if err != nil {
			t.Fatalf("read %s: %v", botPath, err)
		}
		bundleDir := filepath.Dir(botPath)

		deps := declaredSkillDependencies(t, bundleDir)
		for _, declared := range declaredSkills(string(src)) {
			switch answerForSkillReference(declared, operatorAttachedSkills, deps) {
			case skillOperatorAttached:
				continue
			case skillDeclaredDependency:
				// The bytes are the producer's and land at this exact path
				// at install time; what this bundle owes is a declaration
				// that resolves, which TestCatalogSkillDependenciesResolve
				// judges for every bundle at once.
				checked++
				continue
			}
			checked++
			path := filepath.Join(bundleDir, "skills", declared+".md")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s declares skill %q, but %s does not exist.\n"+
					"A skills: entry that resolves to no file is silent: the runtime "+
					"mirrors nothing and the agent's Skill tool finds nothing.",
					botPath, declared, path)
				continue
			}
			// The mirrored skill is addressed by its FRONTMATTER name, so a
			// file whose name disagrees with its filename is reachable under
			// neither reliably.
			m := skillFrontmatterNameRe.FindSubmatch(body)
			if m == nil {
				t.Errorf("%s: skill file %s has no `name:` frontmatter", botPath, path)
				continue
			}
			if got := string(m[1]); got != declared {
				t.Errorf("%s declares skill %q but %s carries `name: %s` — "+
					"filename and frontmatter must agree", botPath, declared, path, got)
			}
		}
	}

	if checked == 0 {
		t.Fatal("matched no skills: declaration at all — the regexp or the DSL changed")
	}
	t.Logf("checked %d skill reference(s) across %d catalog bot(s)", checked, len(all))
}

// declaredSkills returns every skill name appearing in any `skills: [...]`
// list in a .bot source, de-duplicated.
func declaredSkills(src string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(src, "\n") {
		// A `##` comment may legitimately show an example list; only the
		// real declaration counts.
		if strings.HasPrefix(strings.TrimSpace(line), "##") {
			continue
		}
		m := skillsListRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, raw := range strings.Split(m[1], ",") {
			name := strings.Trim(strings.TrimSpace(raw), `"'`)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// skillAnswer is what answers a node's `skills:` name. Only one of the three
// is a file in THIS bundle; reading the other two as a missing document is
// how a deliberate choice turns into a false failure.
type skillAnswer int

const (
	// skillShipped: the bundle carries the document itself, which is what
	// the walk below verifies.
	skillShipped skillAnswer = iota
	// skillOperatorAttached: the operator supplies it from their library
	// (ADR-059's third tier), per the allowlist's reasoned entries.
	skillOperatorAttached
	// skillDeclaredDependency: `dependencies.skills` names it and the
	// install materializes the producer's bytes at skills/<name>.md
	// (ADR-144 clause 2).
	skillDeclaredDependency
)

// answerForSkillReference classifies one reference. The operator exception
// comes FIRST: a dependency on an attached name is refused outright by
// refuseUnresolvableSkillDependencies, and classifying it as a dependency
// here would hide that refusal behind an acceptance.
func answerForSkillReference(name string, attached map[string]string, deps map[string]bundle.SkillDependency) skillAnswer {
	if _, isAttached := attached[name]; isAttached {
		return skillOperatorAttached
	}
	if _, shared := deps[name]; shared {
		return skillDeclaredDependency
	}
	return skillShipped
}

// declaredSkillDependencies reads one bundle's `dependencies.skills`, keyed
// by the name its own `skills:` lists reference. A bundle with no manifest
// declares none, which is every bundle that carries a copy of every skill it
// references.
func declaredSkillDependencies(t *testing.T, bundleDir string) map[string]bundle.SkillDependency {
	t.Helper()
	m, err := bundle.LoadManifest(filepath.Join(bundleDir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("%s: load manifest: %v", bundleDir, err)
	}
	if m == nil {
		return nil
	}
	deps := make(map[string]bundle.SkillDependency, len(m.Dependencies.Skills))
	for _, dep := range m.Dependencies.Skills {
		deps[dep.Name] = dep
	}
	return deps
}

// TestASkillReferenceIsAnsweredByOneOfThreeThings pins the classification the
// walk reads. A declared dependency accepted as a missing file would redden
// the gate for a bundle that correctly stopped carrying a copy; an
// operator-attached name read as a dependency would hide the refusal that
// says a catalog bot may not depend on it.
func TestASkillReferenceIsAnsweredByOneOfThreeThings(t *testing.T) {
	deps := map[string]bundle.SkillDependency{
		"verify-build":  {Name: "verify-build", From: "app-dev"},
		"deploy-target": {Name: "deploy-target", From: "app-dev"},
	}
	for name, tc := range map[string]struct {
		ref  string
		want skillAnswer
	}{
		"a shipped document":            {"copi-conversation", skillShipped},
		"a declared dependency":         {"verify-build", skillDeclaredDependency},
		"an operator-attached name":     {"deploy-target", skillOperatorAttached},
		"an operator name with no deps": {"deploy-onyxia-sspcloud", skillOperatorAttached},
	} {
		t.Run(name, func(t *testing.T) {
			if got := answerForSkillReference(tc.ref, operatorAttachedSkills, deps); got != tc.want {
				t.Fatalf("answerForSkillReference(%q) = %d, want %d", tc.ref, got, tc.want)
			}
		})
	}
}
