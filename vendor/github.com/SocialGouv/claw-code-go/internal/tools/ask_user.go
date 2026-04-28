package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
)

// AskUserQuestionTool returns the tool definition for asking the user a question.
// Execution is handled specially by the runtime — the agent loop pauses and
// surfaces the question to the user via the TUI or stdin.
func AskUserQuestionTool() api.Tool {
	return api.Tool{
		Name:        "ask_user",
		Description: "Pause the current task and ask the user a clarifying question. Returns the user's typed response.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"question": {
					Type:        "string",
					Description: "The question to ask the user",
				},
			},
			Required: []string{"question"},
		},
	}
}

// AskUserInput extracts the question from the tool input map.
// Actual execution is handled by the runtime layer, not here, because it
// requires interaction with the event channel or stdin.
func AskUserInput(input map[string]any) (string, bool) {
	q, ok := input["question"].(string)
	return q, ok && q != ""
}

// AskUserFallback returns a placeholder result when interactive input is unavailable.
func AskUserFallback(question string) api.ContentBlock {
	return api.ContentBlock{
		Type:    "tool_result",
		Content: []api.ContentBlock{{Type: "text", Text: "[ask_user is not available in non-interactive mode. Question was: " + question + "]"}},
		IsError: false,
	}
}
