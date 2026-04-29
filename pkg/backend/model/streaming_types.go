package model

import (
	"encoding/json"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// FinishReason indicates why generation stopped.
type FinishReason string

// Canonical FinishReason constants.
const (
	FinishStop          FinishReason = "stop"
	FinishToolCalls     FinishReason = "tool-calls"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content-filter"
	FinishError         FinishReason = "error"
	FinishOther         FinishReason = "other"
)

// Usage tracks token consumption for a request.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	ReasoningTokens  int
	CacheReadTokens  int
	CacheWriteTokens int
}

// ToolCall represents the model's request to invoke a tool.
type ToolCall struct {
	// ID is a unique identifier for this tool call.
	ID string

	// Name of the tool to invoke.
	Name string

	// Input is the JSON-encoded arguments.
	Input json.RawMessage
}

// TextResult is the final result of a text generation call.
type TextResult struct {
	// Text is the accumulated generated text.
	Text string

	// ToolCalls requested by the model in the final step.
	ToolCalls []ToolCall

	// Steps contains results from each generation step.
	Steps []StepResult

	// TotalUsage is the aggregated token usage across all steps.
	TotalUsage Usage

	// FinishReason indicates why generation stopped.
	FinishReason FinishReason

	// Messages is the final accumulated conversation list, including
	// the initial input plus every assistant turn (with tool_use
	// blocks) and tool_result message produced during the loop. Hosts
	// that maintain a per-node session — e.g. for compaction-aware
	// retries — capture this slice and feed it back as opts.Messages
	// on the next attempt.
	Messages []api.Message
}

// ObjectResult is a typed wrapper around a text generation result that includes
// a deserialized object of type T (for structured output / GenerateObject).
type ObjectResult[T any] struct {
	// Object is the deserialized result.
	Object T

	// Text is the raw text that was parsed.
	Text string

	// Steps contains results from each generation step.
	Steps []StepResult

	// TotalUsage is the aggregated token usage across all steps.
	TotalUsage Usage

	// FinishReason indicates why generation stopped.
	FinishReason FinishReason
}
