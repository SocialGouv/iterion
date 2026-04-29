package model

import "strings"

// capabilitiesForModel returns static capabilities for a given provider and model ID.
// This replaces sdk.ModelCapabilitiesOf() which required a runtime interface assertion.
func capabilitiesForModel(provider, modelID string) ModelCapabilities {
	switch provider {
	case "anthropic":
		return anthropicCapabilities(modelID)
	case "openai":
		return openaiCapabilities(modelID)
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
