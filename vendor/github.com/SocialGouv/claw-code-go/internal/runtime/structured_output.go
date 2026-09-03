package runtime

import (
	"errors"

	"github.com/SocialGouv/claw-code-go/internal/tools"
)

const structuredOutputRequiresWorkMessage = "structured_output rejected: no work has been performed yet in this session. Do the mission with the available tools first; call structured_output only when the mission is complete — it ends the session."

var nonWorkTools = map[string]struct{}{
	"structured_output": {},
	"ask_user":          {},
	"AskUserQuestion":   {},
	"sleep":             {},
	"config":            {},
	"todo_write":        {},
	"enter_plan_mode":   {},
	"exit_plan_mode":    {},
}

func isWorkTool(name string) bool {
	_, excluded := nonWorkTools[name]
	return !excluded
}

func (loop *ConversationLoop) hasWorkCapableTools() bool {
	for _, tool := range loop.allTools() {
		if isWorkTool(tool.Name) {
			return true
		}
	}
	return false
}

func (loop *ConversationLoop) recordWorkToolCompletion(name string, isError bool) {
	if !isError && isWorkTool(name) {
		loop.workToolCompleted.Store(true)
	}
}

// executeStructuredOutput prevents schema-obedient models from treating the
// final-output schema as a first-turn form to fill. Tool-less sessions remain
// valid for pure structured-output probes, and callers can explicitly opt out.
func (loop *ConversationLoop) executeStructuredOutput(input map[string]any) (string, error) {
	allowImmediate := loop.Config != nil && loop.Config.AllowImmediateStructuredOutput
	if !allowImmediate && loop.hasWorkCapableTools() && !loop.workToolCompleted.Load() {
		return "", errors.New(structuredOutputRequiresWorkMessage)
	}
	return tools.ExecuteStructuredOutput(input)
}
