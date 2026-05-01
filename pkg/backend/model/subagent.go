package model

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawhooks "github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// NewSubagentRunner returns a closure suitable as the executor for
// the claw `agent` tool. When the LLM dispatches the agent tool, the
// closure spins up a child conversation against the same model
// registry, filters the tool registry by AllowedToolsForSubagent, and
// returns the child's final text alongside its agent_id.
//
// Hooks are propagated to the parent's event stream with a
// "subagent:<agent_id>" nodeID so a live test can observe the full
// sub-tree.
//
// The returned executor never recurses into itself: the `agent` tool
// is filtered out of the child's tool set, regardless of the
// subagent type's allowlist. defaultModel is used when the input
// omits the model field; pass an empty string to force an explicit
// model on every call.
func NewSubagentRunner(
	modelReg *Registry,
	toolReg *tool.Registry,
	eventHooks EventHooks,
	lifecycle *clawhooks.Runner,
	defaultModel string,
) func(ctx context.Context, input map[string]any) (string, error) {
	return func(ctx context.Context, input map[string]any) (string, error) {
		spec, err := clawtools.ValidateAgentInput(input)
		if err != nil {
			return "", err
		}

		// claw's ValidateAgentInput substitutes a hard-coded default
		// model when the caller omits it. That default is bare
		// ("claude-opus-4-6"), but iterion's model registry resolves
		// "provider/model-id". Treat an unset input["model"] as
		// "use our defaultModel" so workflow authors don't have to
		// specify the model on every agent call.
		raw, _ := input["model"].(string)
		modelSpec := raw
		if modelSpec == "" {
			modelSpec = defaultModel
		}
		if modelSpec == "" {
			return "", fmt.Errorf("subagent: no model specified and no default")
		}

		client, err := modelReg.Resolve(modelSpec)
		if err != nil {
			return "", fmt.Errorf("subagent: resolve model %q: %w", modelSpec, err)
		}
		_, modelID, err := ParseModelSpec(modelSpec)
		if err != nil {
			return "", fmt.Errorf("subagent: parse model spec: %w", err)
		}

		allowed := clawtools.AllowedToolsForSubagent(spec.SubagentType)
		genTools := buildSubagentTools(toolReg, allowed)

		opts := GenerationOptions{
			Model:  modelID,
			System: "You are a sub-agent. Complete the task with the tools available and return your final answer concisely.",
			Messages: []api.Message{{
				Role:    "user",
				Content: []api.ContentBlock{{Type: "text", Text: spec.Prompt}},
			}},
			Tools:    genTools,
			MaxSteps: 10,
			Hooks:    lifecycle,
		}
		applyHooks("subagent:"+spec.AgentID, eventHooks, &opts)

		result, err := GenerateTextDirect(ctx, client, opts)
		if err != nil {
			return "", fmt.Errorf("subagent %s: %w", spec.AgentID, err)
		}

		out := map[string]any{
			"agent_id":      spec.AgentID,
			"name":          spec.Name,
			"description":   spec.Description,
			"subagent_type": spec.SubagentType,
			"model":         modelSpec,
			"status":        "completed",
			"text":          result.Text,
			"input_tokens":  result.TotalUsage.InputTokens,
			"output_tokens": result.TotalUsage.OutputTokens,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return string(b), nil
	}
}

// buildSubagentTools selects tools from the iterion registry to
// expose to a subagent, applying claw's per-type allowlist (nil =
// general-purpose, all tools allowed). The `agent` tool itself is
// always filtered to prevent recursion.
func buildSubagentTools(reg *tool.Registry, allowed map[string]bool) []GenerationTool {
	defs := reg.List()
	out := make([]GenerationTool, 0, len(defs))
	for _, td := range defs {
		if td.QualifiedName == "agent" {
			continue
		}
		if allowed != nil && !allowed[td.QualifiedName] {
			continue
		}
		out = append(out, GenerationTool{
			Name:        td.QualifiedName,
			Description: td.Description,
			InputSchema: td.InputSchema,
			Execute:     td.Execute,
		})
	}
	return out
}
