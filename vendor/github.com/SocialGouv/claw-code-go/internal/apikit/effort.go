package apikit

import (
	"fmt"
	"strings"
)

// knownEfforts is the union of every effort level any provider accepts. It is
// used for a coarse syntactic check before consulting a model's specific
// matrix, and to keep non-effort tokens (e.g. the on/off/stream reasoning
// *mode*) from leaking onto the wire as an effort value.
var knownEfforts = map[string]bool{
	"none":    true,
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"xhigh":   true,
	"max":     true,
}

// IsKnownEffort reports whether level is a recognised reasoning-effort value.
func IsKnownEffort(level string) bool {
	return knownEfforts[strings.ToLower(strings.TrimSpace(level))]
}

// ValidateEffortForModel checks that level is a recognised effort level and,
// when the model declares an effort matrix, that the model accepts it. An
// empty level is valid (means "use the API default"). Models with no matrix
// accept any recognised level — the provider validates at request time.
func ValidateEffortForModel(level, model string) error {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return nil
	}
	if !knownEfforts[level] {
		return fmt.Errorf("unknown effort level %q (use low, medium, high, xhigh, or max)", level)
	}
	supported, _ := EffortCapabilities(model)
	if len(supported) == 0 {
		return nil
	}
	for _, s := range supported {
		if s == level {
			return nil
		}
	}
	return fmt.Errorf("model %q does not support effort %q (supported: %s)", model, level, strings.Join(supported, ", "))
}

// EffortCapabilities returns the supported reasoning_effort levels and the
// API default for a model name or alias.
//
// supported is ordered low→high (e.g. ["low","medium","high","xhigh","max"]).
// Returns (nil, "") when the model is not in the registry, or when it does
// not declare an effort matrix — callers should treat this as "the model
// does not support reasoning_effort, omit the parameter".
//
// The matrix is curated from provider documentation:
//   - Anthropic: platform.claude.com/docs/en/build-with-claude/effort
//   - OpenAI:    platform.openai.com/docs/guides/reasoning
func EffortCapabilities(nameOrAlias string) (supported []string, defaultLevel string) {
	entry := DefaultModelRegistry().LookupModel(nameOrAlias)
	if entry == nil {
		return nil, ""
	}
	return entry.SupportedReasoningEfforts, entry.DefaultReasoningEffort
}

// AnthropicWireProfile describes how to shape an Anthropic Messages API
// request for a given model. It is derived from the model registry, which is
// curated from Anthropic's effort and adaptive-thinking documentation.
type AnthropicWireProfile struct {
	// SupportedEfforts lists the effort levels this model accepts on the wire
	// (empty when the model has no effort matrix). SupportsEffort is derived
	// from it.
	SupportedEfforts []string
	// ThinkingMode is "adaptive", "manual", or "" — see ModelEntry.ThinkingMode.
	ThinkingMode string
	// RejectsSampling is true when temperature/top_p/top_k must be omitted
	// (they return a 400 on this model).
	RejectsSampling          bool
	RequiresAdaptiveThinking bool
}

// SupportsEffort reports whether the model accepts output_config.effort.
func (p AnthropicWireProfile) SupportsEffort() bool { return len(p.SupportedEfforts) > 0 }

// AcceptsEffort reports whether level is one the model accepts on the wire.
func (p AnthropicWireProfile) AcceptsEffort(level string) bool {
	for _, e := range p.SupportedEfforts {
		if e == level {
			return true
		}
	}
	return false
}

// AnthropicProfile returns the wire-shaping profile for a model name or alias
// in a single registry lookup. Unknown models return a zero profile (no
// effort, no thinking control, sampling allowed) so callers pass requests
// through unchanged.
func AnthropicProfile(nameOrAlias string) AnthropicWireProfile {
	entry := DefaultModelRegistry().LookupModel(nameOrAlias)
	if entry == nil {
		return AnthropicWireProfile{}
	}
	return AnthropicWireProfile{
		SupportedEfforts:         entry.SupportedReasoningEfforts,
		ThinkingMode:             entry.ThinkingMode,
		RejectsSampling:          entry.RejectsSampling,
		RequiresAdaptiveThinking: entry.RequiresAdaptiveThinking,
	}
}
