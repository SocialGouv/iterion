package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Generation with retry
// ---------------------------------------------------------------------------

// extractJSON extracts a JSON object from text that may contain markdown
// fences or surrounding commentary. Returns the raw JSON string.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)

	// Strip markdown code fences.
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		return strings.TrimSpace(text)
	}
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		return strings.TrimSpace(text)
	}

	// If text starts with {, it's already JSON.
	if strings.HasPrefix(text, "{") {
		return text
	}

	// Try to find embedded JSON object in the text.
	start := strings.Index(text, "{")
	if start >= 0 {
		// Find the matching closing brace. Track whether we are inside a
		// double-quoted string (respecting backslash escapes) so braces that
		// appear inside string literals — e.g. {"k":"a } b"} — don't terminate
		// the scan early and truncate the object.
		depth := 0
		inString := false
		escaped := false
		for i := start; i < len(text); i++ {
			c := text[i]
			if inString {
				switch {
				case escaped:
					escaped = false
				case c == '\\':
					escaped = true
				case c == '"':
					inString = false
				}
				continue
			}
			switch c {
			case '"':
				inString = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return text[start : i+1]
				}
			}
		}
	}

	return text
}

// ---------------------------------------------------------------------------
// LLM router execution
// ---------------------------------------------------------------------------

// buildRouterSchema creates an auto-generated schema for LLM routers.
// Single mode: {selected_route: string(enum), reasoning: string}
// Multi mode:  {selected_routes: string[](enum), reasoning: string}
func buildRouterSchema(node *ir.RouterNode, candidates []string) *ir.Schema {
	if node.RouterMulti {
		return &ir.Schema{
			Name: node.ID + "_route_selection",
			Fields: []*ir.SchemaField{
				{Name: "selected_routes", Type: ir.FieldTypeStringArray, EnumValues: candidates},
				{Name: "reasoning", Type: ir.FieldTypeString},
			},
		}
	}
	return &ir.Schema{
		Name: node.ID + "_route_selection",
		Fields: []*ir.SchemaField{
			{Name: "selected_route", Type: ir.FieldTypeString, EnumValues: candidates},
			{Name: "reasoning", Type: ir.FieldTypeString},
		},
	}
}

// routerRoutingInstruction returns the standard instruction appended to LLM
// router system prompts, shared by both direct and delegated paths.
func routerRoutingInstruction(candidates []string) string {
	return fmt.Sprintf(
		"\n\nYou are a routing decision maker. Based on the input context, select the most appropriate route(s) from the available options: %v.\nRespond with your selection using the required output format.",
		candidates,
	)
}

// executeLLMRouterUnified is the unified LLM router path that works with any backend.
func (e *ClawExecutor) executeLLMRouterUnified(ctx context.Context, node *ir.RouterNode, input map[string]any) (map[string]any, error) {
	backendName := e.resolveBackendName(node)

	if e.backendRegistry == nil {
		return nil, fmt.Errorf("model: llm router %q uses backend %q but no backend registry configured", node.ID, backendName)
	}

	backend, err := e.backendRegistry.Resolve(backendName)
	if err != nil {
		return nil, fmt.Errorf("model: llm router %q: %w", node.ID, err)
	}

	// Extract route candidates injected by the engine.
	candidatesRaw, ok := input["_route_candidates"]
	if !ok {
		return nil, fmt.Errorf("model: llm router %q: no _route_candidates in input", node.ID)
	}
	var candidates []string
	switch v := candidatesRaw.(type) {
	case []string:
		candidates = v
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("model: llm router %q: _route_candidates contains non-string element", node.ID)
			}
			candidates = append(candidates, s)
		}
	default:
		return nil, fmt.Errorf("model: llm router %q: _route_candidates is %T, expected []string", node.ID, candidatesRaw)
	}

	// Build clean input (without internal keys) for the prompt.
	cleanInput := make(map[string]any)
	for k, v := range input {
		if !strings.HasPrefix(k, "_") {
			cleanInput[k] = v
		}
	}

	td := TemplateDataFromContext(ctx)

	// Build system prompt with routing instruction.
	var systemText string
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText = e.resolveTemplate(p.Body, cleanInput, td)
		}
	}
	systemText += routerRoutingInstruction(candidates)

	// User message.
	userText := e.buildUserMessage(node.UserPrompt, cleanInput, td)

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(node.ID, systemText, userText)
	}

	// Auto-generate schema from candidates.
	schema := buildRouterSchema(node, candidates)
	jsonSchema, err := SchemaToJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("model: llm router %q: schema: %w", node.ID, err)
	}

	// Resolve model for the router (with fallback chain). Use
	// ExpandEnvWithDefault so `${VAR:-default}` syntax in recipes
	// resolves to the default when VAR is unset, instead of the
	// stdlib's silent collapse to "".
	expanded := ir.ExpandEnvWithDefault(node.Model)
	if expanded == "" {
		expanded = os.Getenv("ITERION_DEFAULT_SUPERVISOR_MODEL")
	}
	if expanded == "" {
		expanded = defaultRouterModel
	}

	task := delegate.Task{
		NodeID:           node.ID,
		Iteration:        LoopIterationFromContext(ctx),
		SystemPrompt:     systemText,
		SystemPromptMode: delegate.SystemPromptModeForBackend(backendName),
		UserPrompt:       userText,
		OutputSchema:     jsonSchema,
		Model:            expanded,
		WorkDir:          e.workDir,
		// wireEffort collapses the "ultracode" mode to xhigh so the raw token
		// never reaches the provider; identity for every other level. Routers
		// don't get the orchestration prerogative (they route, not orchestrate).
		ReasoningEffort: wireEffort(e.effortForNode(node, node.ReasoningEffort, input)),
		Sandbox:         e.sandbox,
		// ProviderHint is set per-attempt by dispatchWithProviderFallback
		// as it walks the node's provider chain.
		InboxDrain: e.bindInboxDrain(ctx),
	}

	result, err := e.dispatchWithObservability(ctx, node.ID, backendName, "model: llm router", e.resolveProviderChain(node), backend, &task)
	if err != nil {
		return nil, err
	}

	output := result.Output

	// If structured output parsing fell back to text wrapper, attempt JSON
	// extraction from the text. Routers must produce structured output.
	if result.ParseFallback {
		if textVal, ok := output["text"].(string); ok {
			var parsed map[string]any
			if json.Unmarshal([]byte(textVal), &parsed) == nil {
				output = parsed
			} else {
				return nil, fmt.Errorf("model: llm router %q: backend returned unstructured text, cannot determine route selection", node.ID)
			}
		}
	}

	// Strict validation against the router schema.
	if err := ValidateOutput(output, schema); err != nil {
		return nil, fmt.Errorf("model: llm router %q: output invalid: %w", node.ID, err)
	}

	// Attach metadata.
	stampDelegateOutputMeta(output, result, backendName)

	return output, nil
}

// ---------------------------------------------------------------------------
// Reasoning effort resolution
// ---------------------------------------------------------------------------

// resolveReasoningEffort determines the effective reasoning effort for a node.
// It checks for a dynamic override in the input map via the reserved key
// "_reasoning_effort", then falls back to the static node property
// (resolving env-substituted forms via ir.ResolveEffortLiteral).
//
// The "_reasoning_effort" key uses an underscore prefix to distinguish it from
// user-defined schema fields. It allows upstream nodes to dynamically control
// the reasoning effort of downstream nodes via edge with-mappings, e.g.:
//
//	router -> agent with {_reasoning_effort: "high"}
//
// Valid values are defined in ir.ValidReasoningEfforts: low, medium, high, xhigh, max.
// Invalid dynamic values are silently ignored (falls back to the static property).
func resolveReasoningEffort(nodeEffort string, input map[string]any) string {
	if v, ok := input["_reasoning_effort"]; ok {
		if s, ok := v.(string); ok && ir.ValidReasoningEfforts[s] {
			return s
		}
	}
	return ir.ResolveEffortLiteral(nodeEffort)
}

// effortForNode is resolveReasoningEffort with the launch-time override on
// top. The override wins over BOTH the node's static `reasoning_effort:` and a
// dynamic `_reasoning_effort` edge mapping, for the same reason the model and
// backend overrides do: the operator is explicitly re-targeting this run
// without editing the .bot, so a workflow-authored value cannot outrank them.
//
// A bot that escalates effort per branch is therefore flattened by a run-wide
// `*` override — which is what asking for one means.
func (e *ClawExecutor) effortForNode(node ir.Node, nodeEffort string, input map[string]any) string {
	if ov := e.modelOverrides.ForNode(node.NodeID(), node.NodeKind()); ov.Effort != "" {
		return ov.Effort
	}
	return resolveReasoningEffort(nodeEffort, input)
}

// providerOptsForNode builds the ProviderOptions map from the resolved
// reasoning effort. Returns nil if no provider options are needed.
func providerOptsForNode(effort string) map[string]any {
	if effort == "" {
		return nil
	}
	return map[string]any{"reasoning_effort": effort}
}

// resolveCompaction returns the effective compaction threshold ratio and
// preserve_recent count for a node, walking the cascade
// node → workflow → env → built-in (0 falls through). The backend treats 0
// as "use default", so callers can pass the result straight through to
// delegate.Task.
func resolveCompaction(node, workflow *ir.Compaction) (ratio float64, preserveRecent int) {
	if node != nil {
		ratio = node.Threshold
		preserveRecent = node.PreserveRecent
	}
	if workflow != nil {
		if ratio == 0 {
			ratio = workflow.Threshold
		}
		if preserveRecent == 0 {
			preserveRecent = workflow.PreserveRecent
		}
	}
	if ratio == 0 {
		if env := os.Getenv("ITERION_CLAW_COMPACT_THRESHOLD_RATIO"); env != "" {
			if v, err := strconv.ParseFloat(env, 64); err == nil && v > 0 && v <= 1 {
				ratio = v
			}
		}
	}
	if preserveRecent == 0 {
		if env := os.Getenv("ITERION_CLAW_COMPACT_PRESERVE_RECENT"); env != "" {
			if v, err := strconv.Atoi(env); err == nil && v > 0 {
				preserveRecent = v
			}
		}
	}
	return ratio, preserveRecent
}
