package tool

import (
	"context"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// Async human interaction tools (ADR-081), claw side. The registry
// carries the SPECS with an explicit-error default exec; the claw
// backend overrides Execute per task when the node is interaction:
// async (the closures live on delegate.Task). A node that somehow
// resolves these tools without the closures gets a loud error, never a
// silent no-op.

// AskUserAsyncTool is the non-blocking sibling of ask_user: it posts
// the question to the operator and returns immediately.
func AskUserAsyncTool() api.Tool {
	return api.Tool{
		Name: delegate.AskUserAsyncToolName,
		Description: "Ask the human operator a question WITHOUT pausing your work. " +
			"The question is posted immediately and you keep working; the operator's answer arrives later " +
			"in your conversation as an operator message tagged with the question id. " +
			"Post questions as early as possible so the operator can answer while you work. " +
			"Use the blocking ask_user instead when you cannot take another step without the answer. " +
			"Optional `options` present the user with selectable choices; when `allow_free_text` is true " +
			"(default if no options) the user may also type a free response.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"question": {
					Type:        "string",
					Description: "The question to ask the operator.",
				},
				"options": {
					Type:        "array",
					Description: "Optional list of selectable answers. Each option must have an id and a label.",
					Items: &api.Property{
						Type: "object",
						Properties: map[string]api.Property{
							"id":    {Type: "string", Description: "Stable identifier returned to the model."},
							"label": {Type: "string", Description: "Human-readable text shown to the user."},
						},
						Required: []string{"id", "label"},
					},
				},
				"allow_free_text": {
					Type:        "boolean",
					Description: "When true (default if no options are provided), the user may type a free-text response instead of selecting an option.",
				},
			},
			Required: []string{"question"},
		},
	}
}

// AwaitAnswersTool is the LLM-discretion sync point: it returns the
// collected answers when every posted question is answered, and pauses
// the run otherwise.
func AwaitAnswersTool() api.Tool {
	return api.Tool{
		Name: delegate.AwaitAnswersToolName,
		Description: "Wait for the operator's answers to the questions you posted with ask_user_async. " +
			"Call this ONLY when you truly cannot proceed without the pending answers. " +
			"If everything is already answered it returns the answers immediately; " +
			"otherwise the run pauses until the operator replies.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"note": {
					Type:        "string",
					Description: "Optional short note on why you need the answers now (shown to the operator).",
				},
			},
		},
	}
}

// RegisterAsyncAsk registers the ask_user_async + await_answers tool
// specs. The default execs error explicitly — the claw backend swaps in
// the real per-task behaviour (delegate.Task closures) for interaction:
// async nodes via ApplyAsyncAskExecs.
func RegisterAsyncAsk(reg *Registry) error {
	notBound := func(name string) func(context.Context, map[string]any) (string, error) {
		return func(context.Context, map[string]any) (string, error) {
			return "", fmt.Errorf("%s: async interaction is not bound for this node — declare `interaction: async` on the node (ADR-081)", name)
		}
	}
	if err := RegisterClawTool(reg, AskUserAsyncTool(), notBound(delegate.AskUserAsyncToolName)); err != nil {
		return err
	}
	return RegisterClawTool(reg, AwaitAnswersTool(), notBound(delegate.AwaitAnswersToolName))
}