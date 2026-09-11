package bots

import (
	"os"
	"strings"
	"testing"
)

func TestCopilotUntitledAuthoringAndLaunchUseHostBoundTurns(t *testing.T) {
	src := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, src, "prompt copi_system:", "prompt copi_user:")
	requireCopilotContract(t, "Copi untitled authoring", system,
		"For a new workflow, file:null is the intended unsaved buffer.",
		"file:null is a valid new unsaved editor target. Do not emit bot.create merely because it has no path.",
		"A save-only turn has no draft or apply intent and copies the active session/revision with editor_save_intent explicit.",
		"Studio owns Save As and the destination.",
		"bot.create is only for an explicit bundle/catalog-entry request, not ordinary live-buffer authoring.",
		"bot.create is complete only when receipt.created_bot supplies the exact name and editor_path",
		"offer one navigation reply to bot/<receipt.created_bot.editor_path>",
	)
}

func TestCopilotInstructionSourcesStayJourneyAgnostic(t *testing.T) {
	paths := []string{
		"copilot/main.bot",
		"copilot/manifest.yaml",
		"copilot/skills/copi-conversation.md",
		"copilot/skills/iterion-bot-architecture.md",
		"copilot/skills/iterion-concepts.md",
		"copilot/skills/iterion-dsl-authoring.md",
		"copilot/skills/iterion-run-debug.md",
	}
	var sources strings.Builder
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sources.Write(raw)
	}
	for _, forbidden := range []string{
		"For every simple fixed numeric threshold",
		"The shipped general repair worker is `feature-dev`",
		"bot://shared-planner/hierarchy-feature-author",
		"Lance Revi dessus",
		"Town Planner incident",
	} {
		if strings.Contains(sources.String(), forbidden) {
			t.Errorf("Copi authoritative instructions contain journey-specific fixture %q", forbidden)
		}
	}
}
