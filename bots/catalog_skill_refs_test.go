package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// Copi's three postures (info / design / debug) each promise the operator
// a body of authored knowledge. Pin the mapping so a rename cannot quietly
// leave a posture answering from priors.
//
// And pin the SECOND half, which is the load-bearing one: the model has to
// know a skill EXISTS before it can call for it, and for a bundle skill
// nothing tells it automatically.
//
//   - the `skill` tool's schema enumerates nothing — it takes a name and
//     says "invoke one when it matches", with no catalog attached;
//   - iterion's generated "## Skills" roster is fed by SetSkillHints, which
//     is called only from applyLibrarySkills — the skill LIBRARY
//     (~/.iterion/skills) and plugin contributions. Bundle skills are
//     mirrored to disk but produce no hints, so for a bundle-only bot like
//     Copi that section is not rendered at all.
//
// So the roster in Copi's own system prompt IS the discovery mechanism. A
// skill added to `skills:` but not named there is mirrored, loadable, and
// invisible — the agent never learns it can ask for it. Verified against
// the real runs: Copi calls `skill` unprompted and picks by posture
// (run 01a02e43 loaded iterion-dsl-authoring three times while drafting).
func TestCopilotHasASkillPerPosture(t *testing.T) {
	raw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)
	declared := map[string]bool{}
	for _, s := range declaredSkills(src) {
		declared[s] = true
	}
	// The roster lives in a prompt body; strip the `skills:` declaration
	// itself so a name present ONLY there cannot satisfy this check.
	prompts := stripSkillsDeclarations(src)

	for _, want := range []struct{ posture, skill string }{
		{"info", "iterion-concepts"},
		{"design", "iterion-dsl-authoring"},
		// Spelling and method fail differently: a graph can compile cleanly
		// and still route from prose or retry into a correction twin.
		{"design", "iterion-bot-architecture"},
		{"debug", "iterion-run-debug"},
		{"every turn", "copi-conversation"},
	} {
		if !declared[want.skill] {
			t.Errorf("posture %q lost its skill: %q is not in copilot's skills: list",
				want.posture, want.skill)
		}
		if !strings.Contains(prompts, want.skill) {
			t.Errorf("posture %q: %q is mirrored but never NAMED in a prompt.\n"+
				"Nothing else tells the model it exists — the skill tool carries no "+
				"catalog, and the generated \"## Skills\" roster covers library and "+
				"plugin skills, not bundle ones. An unnamed skill is unreachable.",
				want.posture, want.skill)
		}
	}
}

// The versioned authoring method travels with Copi. A declaration in an
// operator bot identifies these embedded rules; it must never make the agent
// crawl an arbitrary workspace (or the operator's home) looking for a second
// copy. Besides being nondeterministic, that lookup once spent sixteen minutes
// in a single glob rooted at /home/victor.
func TestCopilotAuthoringStandardIsEmbedded(t *testing.T) {
	mainRaw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	skillRaw, err := os.ReadFile("copilot/skills/iterion-bot-architecture.md")
	if err != nil {
		t.Fatalf("read architecture skill: %v", err)
	}

	main := string(mainRaw)
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	skill := string(skillRaw)
	for _, forbidden := range []string{
		"ITERION_AUTHORING_STANDARD_PATH",
		"that standard outranks the skill: read it",
	} {
		if strings.Contains(main, forbidden) {
			t.Errorf("copilot prompt still delegates its standard to an external file: found %q", forbidden)
		}
	}
	requireCopilotContract(t, "Copi embedded authoring standard", system,
		"An authoring_standard declaration names those embedded rules; it is not a path.",
		"Never search the workspace, the operator's home, or an environment variable for another copy.",
	)

	for _, required := range []string{
		"**Identifier:** `victor/iterion-bot-authoring/v1`",
		"## 5. Deterministic verdicts, advisory review, and human decisions",
		"### 9.1 Context-window budget and compaction avoidance",
		"## 12. Validation and checklist",
	} {
		if !strings.Contains(skill, required) {
			t.Errorf("embedded authoring standard is incomplete: missing %q", required)
		}
	}
	if strings.Contains(skill, "ITERION_AUTHORING_STANDARD_PATH") {
		t.Error("architecture skill still instructs Copi to resolve an external authoring-standard file")
	}
}

func TestCopilotUsesRawBytesForExactActiveFileChanges(t *testing.T) {
	raw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)
	system := copilotContractSection(t, src, "prompt copi_system:", "prompt copi_user:")
	requireCopilotContract(t, "Copi exact active-file changes", system,
		"Prefer exact active-file changes when the active local authoring snapshot is clean and readable and bounded unique replacements are safe",
		"Read raw bytes with read_file.",
		"Never copy a before anchor from the active-editor-document AST serialization.",
		"Each before is nonempty, byte-exact, and occurs once.",
		"A file_changes-only turn has empty draft_bot, has_draft false, and all editor apply/save intents none.",
		"Never emit a full draft and exact replacements for the same file.",
		"With dirty:true, ask the operator to save or discard before any whole-document replacement.",
	)
	if strings.Contains(src, "DECLARED COMPANION FILES") || strings.Contains(src, "declared companion-file exact") {
		t.Error("copilot still calls the declared authoring perimeter companion-only")
	}
	if strings.Contains(src, "This is for text only") {
		t.Error("copilot still forces localized structural edits through full-document drafts")
	}
}

func TestCopilotRunWatchContractBoundsOutcomeKinds(t *testing.T) {
	raw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)
	system := copilotContractSection(t, src, "prompt copi_system:", "prompt copi_user:")
	requireCopilotContract(t, "Copi run.watch outcomes", system,
		"run.watch kinds contains one to four distinct outcomes, never all five.",
		"Choose the narrowest set.",
		`exactly ["run.paused","run.failed","run.stalled","run.finished"]`,
	)
}

func TestCopilotRunWatchContractUnderstandsRootedTreeCoverage(t *testing.T) {
	raw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)
	system := copilotContractSection(t, src, "prompt copi_system:", "prompt copi_user:")
	requireCopilotContract(t, "Copi rooted-tree watch", system,
		"target_run is the watched root; outcome_run is the concrete root or descendant that emitted the outcome.",
		"A run.watch covered_run_id means an ancestor watch covers the child.",
		"Another assistant's watch or a 409 is not this conversation's confirmed coverage.",
		"A full run.watch request updates the same assistant's watch in place; do not unwatch first.",
	)
}

// stripSkillsDeclarations removes everything the MODEL never reads, so that
// what remains is prompt text and only prompt text:
//
//   - the `skills: [...]` declaration itself — mirroring a file is not
//     telling the agent the file exists;
//   - every `##` comment. `##` is the DSL's comment marker: the lexer eats
//     the line, so a skill "documented" beside its declaration is invisible
//     to the model. Caught by mutation-testing this very check, which passed
//     against a prompt roster I had deleted because a comment still named
//     the skill.
func stripSkillsDeclarations(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if skillsListRe.MatchString(line) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "##") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
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

// shippedSkillExport is one `exports.skills` entry as the TREE holds it: the
// id a consumer resolves it by, where the producer keeps the bytes, and the
// `name:` the document itself answers to.
type shippedSkillExport struct {
	id              string
	path            string
	frontmatterName string
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

// catalogSkillExports reads every shipped bundle's `exports.skills`, keyed by
// the manifest NAME a dependency's `from` spells — the same key the install
// matches on, exactly (it refuses a producer directory whose manifest name
// disagrees).
func catalogSkillExports(t *testing.T) map[string]map[string]shippedSkillExport {
	t.Helper()
	manifests, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("no manifests found under bots/*/ — the discovery glob broke")
	}
	exports := map[string]map[string]shippedSkillExport{}
	for _, path := range manifests {
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Fatalf("%s: load: %v", path, err)
		}
		if m == nil || m.Name == "" {
			continue
		}
		for _, exp := range m.Exports.Skills {
			doc := filepath.Join(filepath.Dir(path), filepath.FromSlash(exp.Path))
			entry := shippedSkillExport{id: exp.ID, path: exp.Path}
			if body, readErr := os.ReadFile(doc); readErr == nil {
				if match := skillFrontmatterNameRe.FindSubmatch(body); match != nil {
					entry.frontmatterName = string(match[1])
				}
			} else {
				entry.path = "" // absent: the resolution's own refusal
			}
			if exports[m.Name] == nil {
				exports[m.Name] = map[string]shippedSkillExport{}
			}
			exports[m.Name][exp.ID] = entry
		}
	}
	return exports
}

// refuseUnresolvableSkillDependencies is the catalogue's half of ADR-144
// clause 5, as a predicate both the tree walk and its own fixtures call —
// a guard that only ever sees the tree proves nothing while no bundle has
// adopted a dependency yet.
//
// A declared dependency must resolve HERE, in the repository, under the name
// the consumer references it by, and it must not take over a name the
// OPERATOR attaches: that exception ("a bot may reference a skill it does
// NOT ship, so the operator supplies it") carries its reason per entry, and
// a dependency claiming the name would turn a deliberate choice into an
// install-time pin to one platform.
func refuseUnresolvableSkillDependencies(consumer string, deps []bundle.SkillDependency, shipped map[string]bool, exports map[string]map[string]shippedSkillExport, attached map[string]string) []string {
	var findings []string
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		names = append(names, dep.Name)
	}
	sort.Strings(names)
	byName := make(map[string]bundle.SkillDependency, len(deps))
	for _, dep := range deps {
		byName[dep.Name] = dep
	}
	for _, name := range names {
		dep := byName[name]
		if reason, isAttached := attached[dep.Name]; isAttached {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q, which the operator attaches (%s): a catalog bot that depends on it stops being repo-agnostic", consumer, dep.Name, reason))
			continue
		}
		if shipped[dep.Name] {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q AND ships skills/%s.md: the install refuses that shadow, so one of the two has to go", consumer, dep.Name, dep.Name))
			continue
		}
		producer, known := exports[dep.From]
		if !known {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, which exports no skill in this catalogue: the install would find nothing to resolve", consumer, dep.Name, dep.From))
			continue
		}
		exported, ok := producer[dep.ExportedID()]
		if !ok {
			ids := make([]string, 0, len(producer))
			for id := range producer {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, which does not export %q (it exports: %s)", consumer, dep.Name, dep.From, dep.ExportedID(), strings.Join(ids, ", ")))
			continue
		}
		if exported.path == "" {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, whose export %q names a document that is not in the tree", consumer, dep.Name, dep.From, dep.ExportedID()))
			continue
		}
		if exported.frontmatterName != "" && exported.frontmatterName != dep.Name {
			findings = append(findings, fmt.Sprintf("%s declares dependencies.skills %q from %q, but %s carries `name: %s`: the materialized document would answer to another name than the one `skills: [%s]` spells", consumer, dep.Name, dep.From, exported.path, exported.frontmatterName, dep.Name))
		}
	}
	return findings
}

// TestCatalogSkillDependenciesResolve runs the predicate over the shipped
// catalogue: a declaration that cannot resolve is a bot that installs one
// document short and answers from the model's priors instead.
func TestCatalogSkillDependenciesResolve(t *testing.T) {
	exports := catalogSkillExports(t)
	bundles, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range bundles {
		dir := filepath.Dir(path)
		deps := declaredSkillDependencies(t, dir)
		if len(deps) == 0 {
			continue
		}
		shipped := map[string]bool{}
		if docs, globErr := filepath.Glob(filepath.Join(dir, "skills", "*.md")); globErr == nil {
			for _, doc := range docs {
				shipped[strings.TrimSuffix(filepath.Base(doc), ".md")] = true
			}
		}
		list := make([]bundle.SkillDependency, 0, len(deps))
		for _, dep := range deps {
			list = append(list, dep)
		}
		for _, finding := range refuseUnresolvableSkillDependencies(dir, list, shipped, exports, operatorAttachedSkills) {
			t.Error(finding)
		}
	}
}

// TestTheSkillDependencyGateBites is the guard of the guard: every refusal
// above is exercised against a declaration built here, so the gate cannot
// pass by having nothing to judge — which is exactly its state in the tree
// until the first bundle adopts a dependency.
func TestTheSkillDependencyGateBites(t *testing.T) {
	exports := map[string]map[string]shippedSkillExport{
		"app-dev": {
			"verify-build": {id: "verify-build", path: "app-dev/skills/verify-build.md", frontmatterName: "verify-build"},
			"gone":         {id: "gone"},
			"renamed":      {id: "renamed", path: "app-dev/skills/renamed.md", frontmatterName: "something-else"},
		},
	}
	for name, tc := range map[string]struct {
		dep     bundle.SkillDependency
		shipped map[string]bool
		want    string
	}{
		"resolves": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "verify-build"},
			want: "",
		},
		"an operator-attached name": {
			dep:  bundle.SkillDependency{Name: "deploy-target", From: "app-dev", ID: "verify-build"},
			want: "which the operator attaches",
		},
		"shadowing a shipped document": {
			dep:     bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "verify-build"},
			shipped: map[string]bool{"verify-build": true},
			want:    "the install refuses that shadow",
		},
		"a producer outside the catalogue": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "nowhere", ID: "verify-build"},
			want: `from "nowhere", which exports no skill in this catalogue`,
		},
		"an id the producer does not export": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "absent"},
			want: `does not export "absent" (it exports: gone, renamed, verify-build)`,
		},
		"an export whose document is missing": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "gone"},
			want: "names a document that is not in the tree",
		},
		"a document that answers to another name": {
			dep:  bundle.SkillDependency{Name: "verify-build", From: "app-dev", ID: "renamed"},
			want: "carries `name: something-else`",
		},
	} {
		t.Run(name, func(t *testing.T) {
			findings := refuseUnresolvableSkillDependencies("a-bot", []bundle.SkillDependency{tc.dep}, tc.shipped, exports, operatorAttachedSkills)
			if tc.want == "" {
				if len(findings) != 0 {
					t.Fatalf("a resolvable declaration was refused: %v", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %v, want exactly one naming %q", findings, tc.want)
			}
			if !strings.Contains(findings[0], tc.want) {
				t.Fatalf("finding = %q, want it to name %q", findings[0], tc.want)
			}
		})
	}
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
