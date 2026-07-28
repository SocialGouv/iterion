package model

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

const maxReviewCompanionMessageChars = 800

const reviewCompanionMessageDescription = "Human-facing review instruction. Start with the action the operator " +
	"must take, then give at most three short checks. Use plain, non-technical language and no more than " +
	"120 words. Never include implementation jargon, internal identifiers or statuses, file paths, URLs, " +
	"commit hashes, or raw diff excerpts."

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

	// Companion schema = the node's verdict schema +
	// {needs_human_input, message}.
	// Build it per call from the verdict fields, then add the companion-only
	// boolean and bounded human-facing message without mutating the base schema.
	jsonSchema, err := reviewCompanionJSONSchema(base)
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

// reviewCompanionJSONSchema builds the strict structured-output contract for
// a guided review turn.
func reviewCompanionJSONSchema(base *ir.Schema) (json.RawMessage, error) {
	if base == nil {
		return nil, fmt.Errorf("model: review companion: nil output schema")
	}
	reserved := map[string]struct{}{
		"needs_human_input": {},
		"message":           {},
	}
	for _, field := range base.Fields {
		if _, collision := reserved[field.Name]; collision {
			return nil, fmt.Errorf("model: review companion: output schema %q uses reserved field %q", base.Name, field.Name)
		}
	}

	properties := make(map[string]any, len(base.Fields)+2)
	required := make([]string, 0, len(base.Fields)+2)
	for _, field := range base.Fields {
		properties[field.Name] = fieldToJSONSchema(field)
		required = append(required, field.Name)
	}
	properties["needs_human_input"] = map[string]any{"type": "boolean"}
	properties["message"] = map[string]any{
		"type":        "string",
		"maxLength":   maxReviewCompanionMessageChars,
		"description": reviewCompanionMessageDescription,
	}
	required = append(required, "needs_human_input", "message")

	return json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
}
