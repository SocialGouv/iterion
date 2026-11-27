package bots

import (
	"os"
	"strings"
	"testing"
)

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
