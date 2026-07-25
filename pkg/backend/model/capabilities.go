package model

import "strings"

// capabilitiesForModel returns capabilities for a given provider and model ID.
// It resolves dynamically: the curated static heuristics (curatedCapabilities)
// are the authoritative fallback, and any spec fetched from the online
// aggregator (see modelspecs.go) is merged over them — a fetched ContextWindow>0
// overrides the static one, and reasoning/tool_call/temperature flags override
// the heuristics when the source provides them. When the aggregator lacks the
// model or is unreachable, the curated value wins. Resolution never performs
// blocking network I/O on this path.
func capabilitiesForModel(provider, modelID string) ModelCapabilities {
	return specs.merge(provider, modelID, curatedCapabilities(provider, modelID))
}

// curatedCapabilities is the static heuristic table — the authoritative
// fallback when the dynamic aggregator lacks a model or is unreachable. It
// keeps the hardcoded values (glm-5.2=1M, glm-5.1/4.6=200K, claude/openai
// reasoning heuristics) so brand-new models not yet in aggregators resolve
// correctly.
func curatedCapabilities(provider, modelID string) ModelCapabilities {
	switch provider {
	case "anthropic":
		return anthropicCapabilities(modelID)
	case "openai":
		return openaiCapabilities(modelID)
	case "xai":
		return xaiCapabilities(modelID)
	default:
		// Conservative default: tool calling + temperature, no reasoning.
		return ModelCapabilities{
			ToolCall:    true,
			Temperature: true,
		}
	}
}

func anthropicCapabilities(modelID string) ModelCapabilities {
	lower := strings.ToLower(modelID)

	// z.ai's GLM models are served through the Anthropic-compatible
	// endpoint, so they arrive here as "anthropic/glm-X". They are a
	// distinct family with their own context windows — handle them
	// before the Claude reasoning heuristics below.
	if strings.Contains(lower, "glm") {
		return glmCapabilities(lower)
	}

	// Claude 3.5+ and Claude 4+ support reasoning via extended thinking.
	hasReasoning := strings.Contains(lower, "claude-3-5") ||
		strings.Contains(lower, "claude-3.5") ||
		strings.Contains(lower, "claude-sonnet-4") ||
		strings.Contains(lower, "claude-opus-4") ||
		strings.Contains(lower, "claude-4")

	return ModelCapabilities{
		Reasoning:   hasReasoning,
		ToolCall:    true,
		Temperature: true,
	}
}

// glmContextWindow returns the context window for a GLM model served via
// z.ai's Anthropic-compatible endpoint. GLM-5.2 ships a 1M-token window
// (released 2026-06-13, ~5x its GLM-5.1 predecessor); GLM-5.1 / GLM-4.6 and
// earlier are 200K-class. modelID arrives lowercased.
func glmContextWindow(modelID string) int {
	if strings.Contains(modelID, "glm-5.2") {
		return 1_000_000
	}
	return 200_000
}

// glmCapabilities returns static capabilities for a GLM model. GLM models
// support tool calling, temperature, and extended thinking via the
// Anthropic-compatible endpoint.
func glmCapabilities(modelID string) ModelCapabilities {
	return ModelCapabilities{
		Reasoning:     true,
		ToolCall:      true,
		Temperature:   true,
		ContextWindow: glmContextWindow(modelID),
	}
}

func openaiCapabilities(modelID string) ModelCapabilities {
	lower := strings.ToLower(modelID)

	// o1, o3, o4 series are reasoning models that don't accept temperature.
	isReasoning := strings.HasPrefix(lower, "o1") ||
		strings.HasPrefix(lower, "o3") ||
		strings.HasPrefix(lower, "o4")

	return ModelCapabilities{
		Reasoning:   isReasoning,
		ToolCall:    true,
		Temperature: !isReasoning,
	}
}

// xaiCapabilities returns static capabilities for xAI Grok models.
// Mirrors claw-code-go's openai provider isReasoningModel (grok-3-mini
// always uses reasoning mode) and claw's built-in context window
// (131_072 for the grok-2/3 family). Dynamic modelspecs overlay newer
// numbers when the online aggregator has them.
func xaiCapabilities(modelID string) ModelCapabilities {
	lower := strings.ToLower(modelID)
	// stripRoutingPrefix-equivalent: "xai/grok-3-mini" won't reach here
	// (ParseModelSpec already strips the provider), but a nested prefix
	// like "grok/grok-3-mini" is still possible if someone types it.
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		lower = lower[idx+1:]
	}
	// claw: grok-3-mini always uses reasoning mode (rejects temperature).
	isReasoning := lower == "grok-3-mini"
	return ModelCapabilities{
		Reasoning:     isReasoning,
		ToolCall:      true,
		Temperature:   !isReasoning,
		ContextWindow: 131_072,
	}
}
