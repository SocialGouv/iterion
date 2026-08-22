package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// executeHumanLLM handles human nodes in llm or llm_or_human interaction mode.
// It calls GenerateObjectDirect against api.APIClient with mode-specific
// schema handling for llm_or_human (wrapper schema with needs_human_input).
//
// schemaOverride, when non-nil, is used as the structured-output schema
// instead of looking up node.OutputSchema in e.schemas. This lets callers
// (notably ExecuteHumanLLMForInteraction) thread per-call synthetic
// schemas through without having to register them on the shared
// e.schemas map — eliminating a concurrent-map-write race when multiple
// fan-out branches dispatch interaction LLMs in parallel.
func (e *ClawExecutor) executeHumanLLM(ctx context.Context, node *ir.HumanNode, input map[string]any, schemaOverride *ir.Schema) (map[string]any, error) {
	if node.Interaction == ir.InteractionHuman || node.Interaction == ir.InteractionNone {
		return nil, fmt.Errorf("model: human node %q in %s interaction mode should not be executed by the model layer", node.ID, node.Interaction)
	}

	// Resolve API client (expand env var references, including
	// ${VAR:-default} forms — recipes use those for model fallbacks
	// like "openai/${ITERION_RENOVACY_MODEL_GPT:-gpt-5.5}").
	modelSpec := ir.ExpandEnvWithDefault(node.Model)
	client, err := e.registry.Resolve(modelSpec)
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: %w", node.ID, err)
	}

	// Build GenerationOptions.
	genOpts := GenerationOptions{
		Model: modelSpec,
	}

	// Reasoning effort (dynamic override from input, then static node property).
	// Coerce against the model's supported matrix so a recipe asking for "max"
	// on an OpenAI model is silently clamped rather than rejected at the API.
	if _, modelID, perr := ParseModelSpec(modelSpec); perr == nil {
		if effort := coerceEffortForModel(resolveReasoningEffort("", input), modelID); effort != "" {
			genOpts.ProviderOptions = providerOptsForNode(effort)
		}
	}

	td := TemplateDataFromContext(ctx)

	// System prompt.
	var systemText string
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText = e.resolveTemplate(p.Body, input, td)
			genOpts.System = systemText
		}
	}

	// User message from input.
	userText := e.buildUserMessage("", input, td)

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(node.ID, systemText, userText)
	}

	if userText != "" {
		genOpts.Messages = []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: userText}}},
		}
	}

	// Observability hooks.
	applyHooks(node.ID, LoopIterationFromContext(ctx), e.hooks, &genOpts)

	// Determine the schema to use. A non-nil override takes precedence
	// over the registered-schemas lookup; this is how the interaction
	// path passes its per-call synthetic schema without mutating the
	// shared e.schemas map.
	var schema *ir.Schema
	if schemaOverride != nil {
		schema = schemaOverride
	} else {
		var ok bool
		schema, ok = e.schemas[node.OutputSchema]
		if !ok {
			return nil, fmt.Errorf("model: human node %q references unknown schema %q", node.ID, node.OutputSchema)
		}
	}

	// For llm_or_human, wrap the schema with needs_human_input field.
	if node.Interaction == ir.InteractionLLMOrHuman {
		schema = wrapSchemaWithHumanFlag(schema)
	}

	jsonSchema, err := SchemaToJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: schema conversion: %w", node.ID, err)
	}
	genOpts.ExplicitSchema = jsonSchema

	result, err := GenerateObjectDirect[map[string]any](ctx, client, genOpts)
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: structured generation: %w", node.ID, err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]any)
	}

	// Attach usage metadata. Going through cost.Annotate rather than
	// stamping the keys by hand is what puts `_cost_usd` on this path: an
	// `interaction: llm` human node is a real LLM call, and hand-stamping
	// left it invisible to max_cost_usd on every model, priced or not.
	cost.Annotate(output, modelSpec, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)

	return output, nil
}

// ExecuteHumanLLMForInteraction handles delegate interaction requests by
// creating a synthetic HumanNode from the original node's InteractionFields
// and calling executeHumanLLM. The questions from the ErrNeedsInteraction
// become the input, and the interaction schema is synthesized from the
// question keys.
//
// Returns:
//   - answers: LLM-generated answers for each question
//   - needsHuman: true if the LLM decided to escalate (llm_or_human mode only)
//   - err: any error from model execution
func (e *ClawExecutor) ExecuteHumanLLMForInteraction(
	ctx context.Context,
	nodeID string,
	ni *ErrNeedsInteraction,
	fields ir.InteractionFields,
) (answers map[string]any, needsHuman bool, err error) {
	// Build synthetic schema from question keys.
	schemaFields := make([]*ir.SchemaField, 0, len(ni.Questions))
	for key := range ni.Questions {
		sanitized := sanitizeSchemaKey(key)
		schemaFields = append(schemaFields, &ir.SchemaField{
			Name: sanitized,
			Type: ir.FieldTypeString,
		})
	}
	syntheticSchema := &ir.Schema{
		Name:   nodeID + "_interaction",
		Fields: schemaFields,
	}

	// The synthetic schema is per-call state, not workflow state — pass
	// it through to executeHumanLLM as an override instead of registering
	// it on the shared e.schemas map. The previous registration approach
	// races with sibling fan-out branches reading e.schemas for their own
	// agent/judge/router execution and crashes the process with Go's
	// 'fatal error: concurrent map writes' when ≥2 branches concurrently
	// reach this path. Threading the schema as a parameter also avoids
	// the secondary leak (synthetic entries accumulating across runs
	// without an evict counterpart).
	schemaName := syntheticSchema.Name

	// Build synthetic HumanNode.
	node := &ir.HumanNode{
		BaseNode: ir.BaseNode{ID: nodeID + "_interaction"},
		SchemaFields: ir.SchemaFields{
			OutputSchema: schemaName,
		},
		InteractionFields: fields,
		Model:             fields.InteractionModel,
		SystemPrompt:      fields.InteractionPrompt,
	}

	// Build input from questions (question_key → question text).
	input := make(map[string]any, len(ni.Questions))
	for k, v := range ni.Questions {
		input[sanitizeSchemaKey(k)] = v
	}

	output, err := e.executeHumanLLM(ctx, node, input, syntheticSchema)
	if err != nil {
		return nil, false, fmt.Errorf("model: interaction LLM for node %q: %w", nodeID, err)
	}

	// Check if the LLM decided to escalate (llm_or_human mode).
	if v, ok := output["needs_human_input"]; ok {
		if b, ok := v.(bool); ok {
			needsHuman = b
		}
		delete(output, "needs_human_input")
	}

	// Strip metadata keys.
	delete(output, "_tokens")
	delete(output, "_model")

	return output, needsHuman, nil
}

// sanitizeSchemaKey replaces characters that are invalid in JSON Schema
// property names with underscores. This ensures question keys containing
// special characters (spaces, dots, etc.) produce valid schema fields.
func sanitizeSchemaKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	result := b.String()
	if result == "" {
		return "input"
	}
	return result
}

// wrapSchemaWithHumanFlag creates a copy of the schema with an additional
// needs_human_input boolean field for auto_or_pause mode.
func wrapSchemaWithHumanFlag(schema *ir.Schema) *ir.Schema {
	fields := make([]*ir.SchemaField, len(schema.Fields), len(schema.Fields)+1)
	copy(fields, schema.Fields)
	fields = append(fields, &ir.SchemaField{
		Name: "needs_human_input",
		Type: ir.FieldTypeBool,
	})
	return &ir.Schema{
		Name:   schema.Name + "_auto_or_pause",
		Fields: fields,
	}
}
