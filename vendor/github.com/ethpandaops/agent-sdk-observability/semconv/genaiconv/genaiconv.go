// Package genaiconv exposes free-function constructors for OpenTelemetry
// GenAI span attributes.
//
// Upstream go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv packages its
// attributes as methods on instrument structs (metric-scoped), which does
// not help span-level instrumentation. This package fills that gap with
// thin wrappers over upstream's typed enums. For metrics, use upstream
// directly via `genaiconv.NewClientOperationDuration(meter)` etc.
//
// The upstream semconv version is exposed via UpstreamSemconvVersion so
// consumers can pin both this module and upstream in lock-step.
package genaiconv

import (
	"go.opentelemetry.io/otel/attribute"
	upstream "go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
)

// UpstreamSemconvVersion is the upstream semconv version this package's
// attribute keys and typed-enum lifts target.
const UpstreamSemconvVersion = "v1.40.0"

// Attribute keys.
var (
	OperationNameKey   = attribute.Key("gen_ai.operation.name")
	ProviderNameKey    = attribute.Key("gen_ai.provider.name")
	ConversationIDKey  = attribute.Key("gen_ai.conversation.id")
	RequestModelKey    = attribute.Key("gen_ai.request.model")
	ResponseModelKey   = attribute.Key("gen_ai.response.model")
	ResponseIDKey      = attribute.Key("gen_ai.response.id")
	OutputTypeKey      = attribute.Key("gen_ai.output.type")
	TokenTypeKey       = attribute.Key("gen_ai.token.type")
	ToolNameKey        = attribute.Key("gen_ai.tool.name")
	ToolCallIDKey      = attribute.Key("gen_ai.tool.call.id")
	ToolDescriptionKey = attribute.Key("gen_ai.tool.description")
	ToolTypeKey        = attribute.Key("gen_ai.tool.type")
	AgentIDKey         = attribute.Key("gen_ai.agent.id")
	AgentNameKey       = attribute.Key("gen_ai.agent.name")
)

// Typed-enum lifts. These accept upstream's strongly-typed attribute values
// (genaiconv.OperationNameChat, ProviderNameAnthropic, etc.) and return a
// span-ready attribute.KeyValue.
func OperationName(v upstream.OperationNameAttr) attribute.KeyValue {
	return OperationNameKey.String(string(v))
}

func ProviderName(v upstream.ProviderNameAttr) attribute.KeyValue {
	return ProviderNameKey.String(string(v))
}

func TokenType(v upstream.TokenTypeAttr) attribute.KeyValue {
	return TokenTypeKey.String(string(v))
}

// SpanName formats a span name per the GenAI semconv rule:
// "{gen_ai.operation.name} {target}" when target is non-empty, otherwise
// just the operation name. target is typically the request model (for
// chat / generate_content / text_completion / embeddings), the tool name
// (for execute_tool), or the agent name (for invoke_agent / create_agent).
func SpanName(op upstream.OperationNameAttr, target string) string {
	if target == "" {
		return string(op)
	}

	return string(op) + " " + target
}

// String-valued attributes (no upstream typed enum).
func ConversationID(v string) attribute.KeyValue  { return ConversationIDKey.String(v) }
func RequestModel(v string) attribute.KeyValue    { return RequestModelKey.String(v) }
func ResponseModel(v string) attribute.KeyValue   { return ResponseModelKey.String(v) }
func ResponseID(v string) attribute.KeyValue      { return ResponseIDKey.String(v) }
func OutputType(v string) attribute.KeyValue      { return OutputTypeKey.String(v) }
func ToolName(v string) attribute.KeyValue        { return ToolNameKey.String(v) }
func ToolCallID(v string) attribute.KeyValue      { return ToolCallIDKey.String(v) }
func ToolDescription(v string) attribute.KeyValue { return ToolDescriptionKey.String(v) }
func ToolType(v string) attribute.KeyValue        { return ToolTypeKey.String(v) }
func AgentID(v string) attribute.KeyValue         { return AgentIDKey.String(v) }
func AgentName(v string) attribute.KeyValue       { return AgentNameKey.String(v) }
