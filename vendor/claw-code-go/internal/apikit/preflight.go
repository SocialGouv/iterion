package apikit

import (
	"encoding/json"
	"math"
)

// ModelTokenLimit holds the token limits for a known model.
type ModelTokenLimit struct {
	MaxOutputTokens     uint32
	ContextWindowTokens uint32
}

// ModelTokenLimitForModel returns the token limits for a known model, or nil
// for unknown models. It first checks the ModelRegistry, then falls back to
// the hardcoded switch for backward compatibility.
func ModelTokenLimitForModel(model string) *ModelTokenLimit {
	canonical := ResolveModelAlias(model)

	// 1. Registry lookup — handles built-in + runtime-registered models.
	//    Guard against zero MaxOutput (e.g., grok-2) to avoid returning
	//    a misleading zero-value limit.
	if entry := DefaultModelRegistry().LookupModel(canonical); entry != nil && entry.MaxOutput > 0 {
		return &ModelTokenLimit{
			MaxOutputTokens:     entry.MaxOutput,
			ContextWindowTokens: entry.ContextWindow,
		}
	}

	// 2. Hardcoded fallback for backward compat.
	switch canonical {
	case "claude-opus-4-6":
		return &ModelTokenLimit{MaxOutputTokens: 32_000, ContextWindowTokens: 200_000}
	case "claude-sonnet-4-6", "claude-haiku-4-5-20251213":
		return &ModelTokenLimit{MaxOutputTokens: 64_000, ContextWindowTokens: 200_000}
	case "grok-3", "grok-3-mini":
		return &ModelTokenLimit{MaxOutputTokens: 64_000, ContextWindowTokens: 131_072}
	default:
		return nil
	}
}

// ResolveModelAlias normalizes model names to their canonical form.
// Delegates to the ModelRegistry which holds all alias mappings (built-in +
// runtime-registered). The registry's ResolveAlias returns the input unchanged
// when no alias match is found, preserving pass-through behavior.
func ResolveModelAlias(model string) string {
	return DefaultModelRegistry().ResolveAlias(model)
}

// PreflightCheck validates that the estimated token usage fits within the
// model's context window. Returns a ContextWindowExceeded error if it
// doesn't, or nil for unknown models (pass-through).
func PreflightCheck(model string, estimatedInputTokens, requestedOutputTokens uint32) error {
	limit := ModelTokenLimitForModel(model)
	if limit == nil {
		return nil
	}

	total := saturatingAddU32(estimatedInputTokens, requestedOutputTokens)
	if total > limit.ContextWindowTokens {
		return &ApiError{
			Kind:                  ErrContextWindowExceeded,
			Model:                 ResolveModelAlias(model),
			EstimatedInputTokens:  estimatedInputTokens,
			RequestedOutputTokens: requestedOutputTokens,
			EstimatedTotalTokens:  total,
			ContextWindowTokens:   limit.ContextWindowTokens,
		}
	}
	return nil
}

// saturatingAddU32 adds two uint32 values, capping at math.MaxUint32 on overflow.
// Matches Rust's u32::saturating_add().
func saturatingAddU32(a, b uint32) uint32 {
	sum := a + b
	if sum < a { // overflow wrapped
		return math.MaxUint32
	}
	return sum
}

// MaxTokensForModelWithOverride returns the max output tokens for a model,
// preferring a plugin-provided override when set. Matches Rust's
// max_tokens_for_model_with_override(model, plugin_override).
func MaxTokensForModelWithOverride(model string, pluginOverride *uint32) uint32 {
	if pluginOverride != nil {
		return *pluginOverride
	}
	return MaxTokensForModel(model)
}

// PreflightMessageRequest validates that a message request will fit within the
// model's context window. It estimates input tokens using the JSON serialization
// heuristic and calls PreflightCheck. Returns nil for unknown models.
func PreflightMessageRequest(model string, messages any, maxOutputTokens uint32) error {
	estimatedInput := EstimateSerializedTokens(messages)
	return PreflightCheck(model, estimatedInput, maxOutputTokens)
}

// EstimateSerializedTokens estimates token count by serializing to JSON and
// dividing by 4 (rough heuristic matching Rust's implementation).
func EstimateSerializedTokens(value any) uint32 {
	data, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return uint32(len(data)/4 + 1)
}
