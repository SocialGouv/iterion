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
	loop.recordStructuredOutput(input)
	return tools.ExecuteStructuredOutput(input)
}

// recordStructuredOutput keeps the payload of this loop's structured_output
// call — the typed result a parent reads when this loop is a subagent.
func (loop *ConversationLoop) recordStructuredOutput(payload map[string]any) {
	loop.structuredMu.Lock()
	loop.structuredOutput = payload
	loop.structuredMu.Unlock()
}

// lastStructuredOutput returns the payload recorded by recordStructuredOutput,
// nil when this loop never called structured_output.
func (loop *ConversationLoop) lastStructuredOutput() map[string]any {
	loop.structuredMu.Lock()
	defer loop.structuredMu.Unlock()
	return loop.structuredOutput
}

// storeSubagentStructured keeps a subagent's typed result under its task id.
func (loop *ConversationLoop) storeSubagentStructured(taskID string, payload map[string]any) {
	loop.structuredMu.Lock()
	if loop.subagentStructured == nil {
		loop.subagentStructured = map[string]map[string]any{}
	}
	loop.subagentStructured[taskID] = payload
	loop.structuredMu.Unlock()
}

// subagentStructuredOutput returns the typed result a subagent returned, if it
// called structured_output.
func (loop *ConversationLoop) subagentStructuredOutput(taskID string) (map[string]any, bool) {
	loop.structuredMu.Lock()
	defer loop.structuredMu.Unlock()
	v, ok := loop.subagentStructured[taskID]
	return v, ok
}
