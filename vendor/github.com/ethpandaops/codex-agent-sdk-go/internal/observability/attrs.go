package observability

import "go.opentelemetry.io/otel/attribute"

// SDK-local attribute keys. Not covered by any OTel spec; Codex-specific
// operational labels. Value sets must stay bounded (cardinality discipline).
var (
	OutcomeKey        = attribute.Key("outcome")
	HookEventKey      = attribute.Key("hook.event")
	ThinkingTokensKey = attribute.Key("thinking.tokens")
)

func Outcome(v string) attribute.KeyValue       { return OutcomeKey.String(v) }
func HookEvent(v string) attribute.KeyValue     { return HookEventKey.String(v) }
func ThinkingTokens(v int64) attribute.KeyValue { return ThinkingTokensKey.Int64(v) }

// FinishReasons returns the gen_ai.response.finish_reasons span attribute.
// Per GenAI semconv this is a string array; Codex has a single stop reason.
func FinishReasons(reasons ...string) attribute.KeyValue {
	return attribute.StringSlice("gen_ai.response.finish_reasons", reasons)
}
