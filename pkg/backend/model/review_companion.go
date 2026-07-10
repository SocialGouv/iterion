package model

import (
	"context"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ExecuteReviewCompanion drives a review gate's companion LLM
// (interaction: review). Given a pre-resolved system prompt (the companion's
// authored contract) and a user message (the diff context + dialogue
// transcript + the human's latest reply, assembled by the runtime), it
// returns a structured result carrying:
//
//   - "message"           — the next test-walkthrough message to show the human
//   - "needs_human_input" — false when the companion is satisfied it can conclude
//   - plus every field of the review node's output schema (the verdict:
//     decision / confidence / blockers / …)
//
// The companion never has tools — it reasons over the change and the
// conversation. systemText is resolved by the caller (the runtime, which
// holds rs.vars/outputs), so this layer only performs the LLM call.
func (e *ClawExecutor) ExecuteReviewCompanion(ctx context.Context, node *ir.HumanNode, systemText, userMessage string) (map[string]any, error) {
	if node == nil {
		return nil, fmt.Errorf("model: review companion: nil node")
	}
	base, ok := e.schemas[node.OutputSchema]
	if !ok {
		return nil, fmt.Errorf("model: review node %q references unknown output schema %q", node.ID, node.OutputSchema)
	}

	// Companion schema = the node's verdict schema + {needs_human_input, message}.
	// Reuse wrapSchemaWithHumanFlag for the needs_human_input clone (same
	// per-call, no-shared-map discipline as ExecuteHumanLLMForInteraction),
	// then append the companion's `message` field.
	companionSchema := wrapSchemaWithHumanFlag(base)
	companionSchema.Name = base.Name + "_review_companion"
	companionSchema.Fields = append(companionSchema.Fields,
		&ir.SchemaField{Name: "message", Type: ir.FieldTypeString})
	jsonSchema, err := SchemaToJSON(companionSchema)
	if err != nil {
		return nil, fmt.Errorf("model: review node %q: schema conversion: %w", node.ID, err)
	}

	modelSpec := ir.ExpandEnvWithDefault(node.Model)
	client, err := e.registry.Resolve(modelSpec)
	if err != nil {
		return nil, fmt.Errorf("model: review node %q: %w", node.ID, err)
	}

	genOpts := GenerationOptions{
		Model:          modelSpec,
		System:         systemText,
		ExplicitSchema: jsonSchema,
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: userMessage}}},
		},
	}
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(node.ID, systemText, userMessage)
	}
	applyHooks(node.ID, LoopIterationFromContext(ctx), e.hooks, &genOpts)

	result, err := GenerateObjectDirect[map[string]any](ctx, client, genOpts)
	if err != nil {
		return nil, fmt.Errorf("model: review node %q: companion generation: %w", node.ID, err)
	}
	out := result.Object
	if out == nil {
		out = make(map[string]any)
	}
	return out, nil
}
