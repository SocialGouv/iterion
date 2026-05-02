package apikit

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
