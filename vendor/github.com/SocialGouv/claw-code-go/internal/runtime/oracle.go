package runtime

import (
	"context"
	"fmt"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/apikit"
	"github.com/SocialGouv/claw-code-go/internal/tools"
)

// oracleMaxTokens bounds the advisor's answer.
const oracleMaxTokens = 4096

// oracleEffort is requested when the oracle model supports it (the wire
// layers drop unsupported efforts, but we validate here so an OpenAI
// non-reasoning model never receives the parameter at all).
const oracleEffort = "high"

// executeOracle is the oracle tool's dispatch entry: one read-only,
// tool-less call to the oracle model (Config.OracleModel, falling back to
// the session model) with the question and attached files inlined.
func (loop *ConversationLoop) executeOracle(ctx context.Context, input map[string]any) (string, error) {
	if loop.Client == nil {
		return "", fmt.Errorf("oracle: no API client available")
	}
	system, user, err := tools.BuildOraclePrompt(input, loop.workspaceRoot())
	if err != nil {
		return "", err
	}

	model := ""
	if loop.Config != nil {
		model = loop.Config.OracleModel
		if model == "" {
			model = loop.Config.Model
		}
	}

	req := api.CreateMessageRequest{
		Model:     model,
		MaxTokens: oracleMaxTokens,
		System:    system,
		Messages: []api.Message{{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: user}},
		}},
		Stream: true,
	}
	if apikit.ValidateEffortForModel(oracleEffort, model) == nil {
		req.ReasoningEffort = oracleEffort
	}

	ch, err := loop.Client.StreamResponse(ctx, req)
	if err != nil {
		return "", fmt.Errorf("oracle: %w", err)
	}
	answer, err := collectStreamText(ch)
	if err != nil {
		return "", fmt.Errorf("oracle: %w", err)
	}
	return fmt.Sprintf("[oracle · %s]\n%s", model, answer), nil
}
