package model

import "time"

// RequestInfo is passed to the OnRequest hook before a generation call.
type RequestInfo struct {
	// Model is the model ID.
	Model string

	// MessageCount is the number of messages in the request.
	MessageCount int

	// ToolCount is the number of tools available.
	ToolCount int

	// ReasoningEffort is the resolved reasoning_effort spec sent on the
	// request, when set ("low", "medium", "high", "xhigh", "max"). Empty
	// when the node did not request a reasoning level.
	ReasoningEffort string

	// Timestamp is when the request was initiated.
	Timestamp time.Time
}

// ResponseInfo is passed to the OnResponse hook after a generation call completes.
type ResponseInfo struct {
	// Latency is the time from request to response.
	Latency time.Duration

	// Usage is the token consumption for this call.
	Usage Usage

	// FinishReason indicates why generation stopped.
	FinishReason FinishReason

	// Error is non-nil if the call failed.
	Error error

	// StatusCode is the HTTP status code (0 if not applicable).
	StatusCode int
}

// StepResult describes a single generation step in a tool loop.
type StepResult struct {
	// Number is the 1-based step index.
	Number int

	// Text generated in this step.
	Text string

	// ToolCalls requested in this step.
	ToolCalls []ToolCall

	// FinishReason for this step.
	FinishReason FinishReason

	// Usage for this step.
	Usage Usage
}

// CompactInfo is passed to the OnCompact hook when the running tool-loop
// message history is shrunk by claw's pure-function compactor between
// iterations. Emitted only when compaction actually fired (no event when
// the transcript was short enough to skip).
type CompactInfo struct {
	// BeforeMessages is the message count before compaction.
	BeforeMessages int

	// AfterMessages is the message count after compaction.
	AfterMessages int

	// RemovedMessageCount is reported by claw's CompactionResult.
	RemovedMessageCount int
}

// ToolCallInfo is passed to the OnToolCall hook after a tool executes.
type ToolCallInfo struct {
	// ToolName is the name of the tool that was called.
	ToolName string

	// InputSize is the byte length of the tool input JSON.
	InputSize int

	// Duration is how long the tool execution took.
	Duration time.Duration

	// Error is non-nil if the tool execution failed.
	Error error
}
