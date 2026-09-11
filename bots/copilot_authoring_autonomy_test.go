package bots

import (
	"strings"
	"testing"
)

func TestCopilotOwnsRepairsInsideTheAuthoringPerimeter(t *testing.T) {
	main := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	debug := readCopilotContractFile(t, "copilot/skills/iterion-run-debug.md")

	// The kernel is the single owner of authority, host effects, and proposal
	// shape. The run-debug skill keeps recovery procedure and points back to
	// that owner instead of maintaining a second copy of the same policy.
	requireCopilotContract(t, "Copi system authoring", system,
		"This prompt owns authority, safety, host protocol, and structured-turn semantics.",
		"You have no direct write tool. You may still own a technical repair",
		"Do not delegate work merely because implementation is required",
		"A proposal is not an effect.",
		"sharedBundle marks a verified read-only dependency",
		"Prefer exact active-file changes",
		"Never emit a full draft and exact replacements for the same file.",
		"own the one-file bootstrap",
		"Wait for the save receipt and fresh authoring attachment",
		"request authoring context",
		"On attempt 1, re-read exact bytes and emit at most one corrected proposal",
		"On attempt 2 or higher, emit no draft, changes, or actions",
		"A previously proposed repair stays pending until a matching receipt plus read evidence",
		"A newer unrelated proposal must not silently replace it.",
	)
	requireCopilotContract(t, "Copi run-debug", debug,
		"close the repair loop through the capabilities currently exposed",
		"The system kernel owns authoring authority, proposal shape, validation, receipts, and pending-effect rules",
		"Repair directly through Studio when the required source is inside the active authoring perimeter.",
		"Delegate only across a real capability or repository boundary",
		"Recover the awaited root rather than an orphaned child",
		"Do not stop at diagnosis or a saved patch while a safe recovery step remains.",
	)

	for _, forbidden := range []string{
		"When the defect is in code, delegate; do not describe.",
		"delegate a repair worker, then resume",
		"Delegates real work to other bots.",
		"ask for the smallest one-line manifest declaration",
	} {
		if strings.Contains(system, forbidden) || strings.Contains(debug, forbidden) {
			t.Errorf("Copi still contains an unconditional delegation rule %q", forbidden)
		}
	}

	reflectAgent := copilotContractSection(t, main, "agent reflect:", "judge judge:")
	if !strings.Contains(reflectAgent, "session_slot: assistant_reflection") ||
		!strings.Contains(reflectAgent, "model: \"${ITERION_COPILOT_REFLECTION_MODEL:-openai/gpt-5.6-sol}\"") {
		t.Error("Copi reflection must retain its own persisted Sol session")
	}
	if strings.Contains(reflectAgent, "interaction: human") || strings.Contains(reflectAgent, "assistant_actions") {
		t.Error("Copi reflection must stay private and cannot become a second action-taking chat voice")
	}
	requireCopilotContract(t, "Copi permission", main,
		`permission: deny allow: ["Read(**)", "Glob", "workspace_grep", "ToolSearch", "TodoWrite", "Skill", "diagnostic_shell(iterion --help)"] ask: ["diagnostic_shell"]`,
	)

	dsl := readCopilotContractFile(t, "copilot/skills/iterion-dsl-authoring.md")
	if !strings.Contains(dsl, "C137 | a command reference is inside quotes") {
		t.Error("Copi's DSL skill lost the C137 preflight regression guidance")
	}
}

func TestCopilotTerraKeepsFinalRunActionsHostValid(t *testing.T) {
	main := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	requireCopilotContract(t, "Copi typed host actions", system,
		"Closed catalogue:",
		"run.watch {target_run_id",
		"run.resume accepts run_id and an optional file_path only when current evidence",
		"the authoritative boundary check and never accepts model force.",
		"run.watch kinds contains one to four distinct outcomes, never all five.",
		`exactly ["run.paused","run.failed","run.stalled","run.finished"]`,
		"There is no run.reset.",
	)
}

func TestCopilotAdvisoryReviewKeepsLiveRunRepairBounded(t *testing.T) {
	main := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	reflect := copilotContractSection(t, main, "prompt reflect_system:", "prompt reflect_user:")

	requireCopilotContract(t, "Copi advisory review", system,
		"Review advice retained for Terra is advisory, not a new objective.",
		"Reject speculative expansion when a smaller evidence-backed repair works.",
		"Resolve technical objections that repository, run evidence, validation, or the reviewer can settle",
		"Re-reflect only for one concrete observed blocker.",
		"The reflection_request must restate the original outcome, name that blocker and evidence",
	)
	requireCopilotContract(t, "Copi reflection", reflect,
		"preserve the original user-visible outcome as the scope",
		"smallest safe change that directly explains the observed failure",
		"general migration, policy expansion, or architecture redesign",
		"no local repair can meet that outcome",
	)
}

func TestCopilotManagerScopeBoundaryIsDurableAndBounded(t *testing.T) {
	main := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	requireCopilotContract(t, "Copi manager scope", system,
		"Manager scope boundary",
		"emitted_scope_exclusions",
		"proposal_scope_status",
		"proposal_scope_categories",
		"classifying every proposed effect in draft_bot, file_changes, assistant_actions, and an active reflection_request",
		"retains exclusions monotonically across reflection, receipts, and pauses",
		"Never revive an excluded category through reflection, authoring-context requests, or alternate wording.",
	)
	for _, forbidden := range []string{
		"manager_scope_exclusions: \"[]\"",
		"manager_scope_exclusions: \"'[]'\"",
		"requested_scope_token",
		"scope_narrowing_key",
		"scope_narrowing_count",
		"extract_scope",
		"extracted_scope_exclusions",
		"extracted_scope_reauthorizations",
	} {
		if strings.Contains(main, forbidden) {
			t.Errorf("Copi must not use a string edge literal for manager exclusions: %q", forbidden)
		}
	}
}
