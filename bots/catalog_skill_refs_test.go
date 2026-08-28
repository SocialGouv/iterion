package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
// `<bundle>/skills/<name>.md`, with matching `name:` frontmatter.
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
	teamBots, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatalf("glob team bots: %v", err)
	}
	demoBots, err := filepath.Glob("../examples/*/main.bot")
	if err != nil {
		t.Fatalf("glob demo bots: %v", err)
	}
	all := append(teamBots, demoBots...)
	if len(all) == 0 {
		t.Fatal("no catalog bots found — the glob or the layout changed")
	}

	checked := 0
	for _, botPath := range all {
		src, err := os.ReadFile(botPath)
		if err != nil {
			t.Fatalf("read %s: %v", botPath, err)
		}
		bundleDir := filepath.Dir(botPath)

		for _, declared := range declaredSkills(string(src)) {
			if _, attached := operatorAttachedSkills[declared]; attached {
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
	raw, err := os.ReadFile("copilot/main.bot")
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
	mainRaw, err := os.ReadFile("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	skillRaw, err := os.ReadFile("copilot/skills/iterion-bot-architecture.md")
	if err != nil {
		t.Fatalf("read architecture skill: %v", err)
	}

	main := string(mainRaw)
	skill := string(skillRaw)
	for _, forbidden := range []string{
		"ITERION_AUTHORING_STANDARD_PATH",
		"that standard outranks the skill: read it",
	} {
		if strings.Contains(main, forbidden) {
			t.Errorf("copilot prompt still delegates its standard to an external file: found %q", forbidden)
		}
	}
	if !strings.Contains(main, "Never search the\n  workspace, the operator's home") {
		t.Error("copilot prompt does not explicitly forbid filesystem discovery of an authoring standard")
	}

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
