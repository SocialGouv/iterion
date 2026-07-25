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
	"commit hashes, or raw diff excerpts. Refer to attached files by their captions, never their paths."

// ExecuteReviewCompanion drives a review gate's companion LLM
// (interaction: review). Given a pre-resolved system prompt (the companion's
// authored contract) and a user message (the diff context + dialogue
// transcript + the human's latest reply, assembled by the runtime), it
// returns a structured result carrying:
//
//   - "message"           — the next test-walkthrough message to show the human
//   - "needs_human_input" — false when the companion is satisfied it can conclude
//   - "media_refs"        — exact available attachment paths to show beside the turn
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
	// {needs_human_input, message, media_refs}.
	// Reuse wrapSchemaWithHumanFlag for the needs_human_input clone (same
	// per-call, no-shared-map discipline as ExecuteHumanLLMForInteraction),
	// then add the companion-only fields. media_refs gets a precise nested
	// JSON schema rather than FieldTypeJSON's intentionally unconstrained {}.
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
// a guided review turn. The model only chooses an exact artifact path and a
// plain-text caption; runtime code resolves kind/MIME/size from the real run
// manifest and drops invented paths before persistence.
func reviewCompanionJSONSchema(base *ir.Schema) (json.RawMessage, error) {
	if base == nil {
		return nil, fmt.Errorf("model: review companion: nil output schema")
	}
	reserved := map[string]struct{}{
		"needs_human_input": {},
		"message":           {},
		"media_refs":        {},
	}
	for _, field := range base.Fields {
		if _, collision := reserved[field.Name]; collision {
			return nil, fmt.Errorf("model: review companion: output schema %q uses reserved field %q", base.Name, field.Name)
		}
	}

	companionSchema := wrapSchemaWithHumanFlag(base)
	companionSchema.Name = base.Name + "_review_companion"
	companionSchema.Fields = append(companionSchema.Fields,
		&ir.SchemaField{Name: "message", Type: ir.FieldTypeString},
		&ir.SchemaField{Name: "media_refs", Type: ir.FieldTypeJSON},
	)
	raw, err := SchemaToJSON(companionSchema)
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("decode generated companion schema: %w", err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("generated companion schema has no properties object")
	}
	// The message is rendered verbatim beside a consequential human action.
	// Bound it structurally and spell out the product-language contract in the
	// schema so every structured-output provider receives the same constraint.
	properties["message"] = map[string]any{
		"type":        "string",
		"maxLength":   maxReviewCompanionMessageChars,
		"description": reviewCompanionMessageDescription,
	}
	properties["media_refs"] = map[string]any{
		"type":     "array",
		"maxItems": 12,
		"description": "Passive media, document, or data attachments to show beside this review turn. " +
			"Select only exact paths from the available review attachment manifest; use an empty array " +
			"when none are relevant.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "Exact area-relative path from the available review media manifest.",
				},
				"caption": map[string]any{
					"type":        "string",
					"description": "Short plain-text explanation of what the human should validate.",
				},
			},
			"required":             []string{"path", "caption"},
			"additionalProperties": false,
		},
	}
	return json.Marshal(schema)
}
