package model

import (
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// systemReminder wraps harness-origin guidance in the <system-reminder>
// envelope models are trained to read as background context injected by the
// harness rather than user-authored text (claw's authored prompt sections
// state that contract for the claw backend; Claude Code's native prompt
// states it for claude_code). Content that must carry USER authority — e.g.
// operator mid-run steering — says so explicitly inside the envelope.
func systemReminder(text string) string {
	return "<system-reminder>\n" + strings.TrimSpace(text) + "\n</system-reminder>"
}

// todoReseedMessage is the injected user turn appended right after a
// compaction so a tool-equipped agent re-anchors its task list instead of
// drifting on a summarized history (the todo file survives on disk; what is
// lost is the model's view of it).
func todoReseedMessage() api.Message {
	return api.Message{
		Role: "user",
		Content: []api.ContentBlock{{
			Type: "text",
			Text: systemReminder("The conversation history was just compacted. Your todo list may " +
				"no longer be in view: re-read it (todo_write with action \"read\"), reconcile it " +
				"with the summary above, then continue the task directly — do not re-ask questions " +
				"the operator already answered."),
		}},
	}
}

// hasTodoTool reports whether the generation exposes the todo_write tool —
// the reseed nudge is noise for tool-less judges.
func hasTodoTool(tools []GenerationTool) bool {
	for _, t := range tools {
		if t.Name == "todo_write" {
			return true
		}
	}
	return false
}
