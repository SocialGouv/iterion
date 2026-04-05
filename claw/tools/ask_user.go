package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zendev-sh/goai"
)

var askUserSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"question": {
			"type": "string",
			"description": "The question to ask the user"
		}
	},
	"required": ["question"]
}`)

// AskUser returns a goai.Tool that pauses to ask the user a question.
// onAskUser is called synchronously with the question and must return the answer.
// If nil, a fallback message is returned.
func AskUser(onAskUser func(question string) string) goai.Tool {
	return goai.Tool{
		Name:        "ask_user",
		Description: "Pause the current task and ask the user a clarifying question. Returns the user's typed response.",
		InputSchema: askUserSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Question string `json:"question"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("ask_user: parse input: %w", err)
			}
			if params.Question == "" {
				return "", fmt.Errorf("ask_user: 'question' is required")
			}

			if onAskUser == nil {
				return "[ask_user is not available in non-interactive mode. Question was: " + params.Question + "]", nil
			}

			return onAskUser(params.Question), nil
		},
	}
}
