package model

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// capabilitiesForModel returns capabilities for a given provider and model ID.
// It resolves dynamically: the curated static heuristics (curatedCapabilities)
// are the authoritative fallback, and any spec fetched from the online
// aggregator (pkg/backend/modelspecs) is merged over them — a fetched
// ContextWindow>0 overrides the static one, and reasoning/tool_call/temperature
// flags override the heuristics when the source provides them. When the
// aggregator lacks the model or is unreachable, the curated value wins.
// Resolution never performs blocking network I/O on this path.
func capabilitiesForModel(provider, modelID string) ModelCapabilities {
	curated := curatedCapabilities(provider, modelID)
	spec, ok := modelspecs.Default().Lookup(provider, modelID)
	if !ok {
		return curated
	}
	return mergeSpec(spec, curated)
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

	// Every Claude from 3.5 onward supports reasoning via extended thinking.
	// This reads the GENERATION rather than matching a list of known ids:
	// a list silently misclassifies the next model as non-reasoning, which
	// costs full price for a degraded answer and reports nothing. An
	// unparseable id stays conservative (no reasoning).
	major, minor, ok := claudeGeneration(lower)
	hasReasoning := ok && (major >= 4 || (major == 3 && minor >= 5))

	return ModelCapabilities{
		Reasoning:   hasReasoning,
		ToolCall:    true,
		Temperature: true,
	}
}

// claudeGenerationRe pulls the generation out of a Claude model id, in either
// shape Anthropic has used: family-then-number ("claude-opus-5",
// "claude-sonnet-4-6", "claude-haiku-4-5-20251001") and number-then-family
// ("claude-3-5-sonnet-20241022", "claude-3.5-sonnet"). A trailing variant
// suffix such as "claude-opus-5[1m]" does not disturb it.
var claudeGenerationRe = regexp.MustCompile(`claude-(?:[a-z]+-)?(\d+)(?:[-.](\d+))?`)

// claudeGeneration returns the major and minor generation of a Claude model id.
// ok is false when the id carries no recognisable generation, which callers
// must treat as "unknown" rather than as a low generation.
func claudeGeneration(lower string) (major, minor int, ok bool) {
	m := claudeGenerationRe.FindStringSubmatch(lower)
	if m == nil {
		return 0, 0, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, false
	}
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major, minor, true
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
