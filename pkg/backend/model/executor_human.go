package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// meteredHumanFailure renders a human-node generation's spend, on success or
// failure, in the shape the engine books from. Nil when nothing was served.
// Keeping this map separate from answers lets interaction callers strip the
// reserved namespace without losing the bill. Unreported usage is not zero.
func meteredHumanFailure(modelSpec string, result *ObjectResult[map[string]any]) map[string]any {
	if result == nil || (result.TotalUsage.InputTokens == 0 && result.TotalUsage.OutputTokens == 0 && result.TotalUsage.UnreportedCalls == 0) {
		return nil
	}
	output := map[string]any{}
	annotateUsage(output, modelSpec, result.TotalUsage)
	return output
}

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
	output, spend, err := e.executeHumanLLMWithSpend(ctx, node, input, schemaOverride)
	if err != nil {
		return spend, err
	}
	// The standalone node projects the bill into its output. Clear stale
	// model-written cost and usage markers even when the provider reports zero.
	delete(output, "_cost_usd")
	delete(output, "_usage_unreported_calls")
	for key, value := range spend {
		output[key] = value
	}
	return output, nil
}

// executeHumanLLMWithSpend keeps the model's answers and the provider's usage
// independent. The interaction path passes only answers to the next turn.
func (e *ClawExecutor) executeHumanLLMWithSpend(ctx context.Context, node *ir.HumanNode, input map[string]any, schemaOverride *ir.Schema) (map[string]any, map[string]any, error) {
	if node.Interaction == ir.InteractionHuman || node.Interaction == ir.InteractionNone {
		return nil, nil, fmt.Errorf("model: human node %q in %s interaction mode should not be executed by the model layer", node.ID, node.Interaction)
	}

	// Resolve API client: the model is a routing field — `{{vars.…}}` then
	// `${VAR:-default}` (recipes use the latter for model fallbacks like
	// "openai/${ITERION_RENOVACY_MODEL_GPT:-gpt-5.5}").
	modelSpec := e.resolveRoutingField(node.Model)
	client, err := e.registry.Resolve(modelSpec)
	if err != nil {
		return nil, nil, fmt.Errorf("model: human node %q: %w", node.ID, err)
	}

	// Build GenerationOptions.
	genOpts := GenerationOptions{
		Model: modelSpec,
	}

	// Reasoning effort (dynamic override from input, then static node property).
	// Coerce against the model's supported matrix so a recipe asking for "max"
	// on an OpenAI model is silently clamped rather than rejected at the API.
	if _, _, perr := ParseModelSpec(modelSpec); perr == nil {
		if effort := coerceEffortForModel(e.effortForNode(node, "", input), modelroute.Parse(modelSpec).CapabilityID()); effort != "" {
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
			return nil, nil, fmt.Errorf("model: human node %q references unknown schema %q", node.ID, node.OutputSchema)
		}
	}

	// For llm_or_human, wrap the schema with needs_human_input field.
	if node.Interaction == ir.InteractionLLMOrHuman {
		schema = wrapSchemaWithHumanFlag(schema)
	}

	jsonSchema, err := SchemaToJSON(schema)
	if err != nil {
		return nil, nil, fmt.Errorf("model: human node %q: schema conversion: %w", node.ID, err)
	}
	genOpts.ExplicitSchema = jsonSchema

	result, err := GenerateObjectDirect[map[string]any](ctx, client, e.generationForModel(genOpts))
	if err != nil {
		// The llm half of a human node is a real LLM call, and the engine
		// books what it burned from the map returned beside the error (it
		// then degrades to the human pause). A bare nil made that booking
		// inert: the attempt is over — the human answers next, and a human
		// reports no tokens — so this figure is final or lost.
		return nil, meteredHumanFailure(modelSpec, result), fmt.Errorf("model: human node %q: structured generation: %w", node.ID, err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]any)
	}

	spend := meteredHumanFailure(modelSpec, result)
	if spend == nil {
		// A reported zero still replaces any model-written counters on a
		// standalone human node; the booking guard ignores this zero bill.
		spend = map[string]any{}
		annotateUsage(spend, modelSpec, result.TotalUsage)
	}
	return output, spend, nil
}

// ExecuteHumanLLMForInteraction handles delegate interaction requests by
// creating a synthetic HumanNode from the original node's InteractionFields
// and calling executeHumanLLM. The questions from the ErrNeedsInteraction
// become the input, and the interaction schema is synthesized from the
// question keys.
//
// Returns:
//   - answers: LLM-generated answers on success, nil on failure. Reserved
//     metadata is stripped except for keys the delegate actually asks.
//   - spend: the generation's `_tokens`/`_cost_usd` map, independent of the
//     answers, nil when nothing was observed. Callers book it on BOTH success
//     and failure, including a successful decision to escalate to a human.
//   - needsHuman: true if the LLM decided to escalate (llm_or_human mode only)
//   - err: any error from model execution
func (e *ClawExecutor) ExecuteHumanLLMForInteraction(
	ctx context.Context,
	nodeID string,
	ni *ErrNeedsInteraction,
	fields ir.InteractionFields,
) (answers, spend map[string]any, needsHuman bool, err error) {
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

	output, spend, err := e.executeHumanLLMWithSpend(ctx, node, input, syntheticSchema)
	if err != nil {
		return nil, spend, false, fmt.Errorf("model: interaction LLM for node %q: %w", nodeID, err)
	}

	// Check if the LLM decided to escalate (llm_or_human mode).
	if v, ok := output["needs_human_input"]; ok {
		if b, ok := v.(bool); ok {
			needsHuman = b
		}
		delete(output, "needs_human_input")
	}

	stripInteractionMetadata(output, input)

	return output, spend, needsHuman, nil
}

// stripInteractionMetadata drops the reserved `_`-prefixed namespace from an
// interaction answer map, EXCEPT the keys that were actually asked.
//
// The map is returned as `answers` and copied verbatim into the re-invoked
// node's input (runtime.reInvokeBackend), where a `_`-prefixed key both joins
// the reserved namespace the delegates read and — for a node with no
// user-prompt template — gets JSON-serialized into the user message as if it
// were an answer. So the whole namespace goes, rather than a list of known
// keys: the next metadata addition (`_cost_usd` was one) cannot reopen it.
//
// `asked` is the exemption, and it is not optional. sanitizeSchemaKey maps
// every non-alphanumeric rune to `_`, so a delegate question keyed
// "(a) which db?" arrives as the answer key `_a__which_db_`. Deleting that
// discards an answer the LLM did produce: the agent is re-invoked with the
// question still open, asks it again, and burns interaction depth until
// maxInteractionDepth fails the run — with no event anywhere saying an answer
// was dropped. `asked` holds exactly the sanitized question keys, so it is the
// set the strip must leave alone.
func stripInteractionMetadata(answers, asked map[string]any) {
	for k := range answers {
		if _, wasAsked := asked[k]; wasAsked {
			continue
		}
		if strings.HasPrefix(k, "_") {
			delete(answers, k)
		}
	}
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
