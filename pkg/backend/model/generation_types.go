package model

import (
	"encoding/json"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

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

// TurnCaptureInfo is the payload of the OnTurnCapture hook fired by
// the generation loop after every step completes (and after its tool
// results have been appended, when applicable). It carries everything
// the runtime needs to write a per-turn store.TurnCheckpoint plus,
// optionally, a sibling messages.json blob used as the rehydration
// source for the Fork API.
//
// Conversation is a defensive copy of the messages slice at the
// natural end-of-iteration boundary — the very state the next LLM
// call would be made from if the loop continued. For the final step
// (no follow-up tool calls), the slice is augmented in-snapshot with
// a synthetic assistant text message so the snapshot is forkable too.
type TurnCaptureInfo struct {
	// Step is the 1-based step index, matching StepResult.Number.
	Step int
	// Result mirrors the StepResult passed to OnStepFinish.
	Result StepResult
	// Conversation is the snapshot the runtime persists. Treat as
	// immutable — the generation loop reuses the underlying slice
	// after the callback returns. Callbacks that hand the snapshot
	// off to a goroutine MUST take their own copy first (the
	// generation loop already does so before invoking the hook).
	Conversation []api.Message
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

// ToolCallInfo is passed to the OnToolCall / OnToolStarted hooks before
// (started) and after (completed) a tool executes.
type ToolCallInfo struct {
	// ToolName is the name of the tool that was called.
	ToolName string

	// InputSize is the byte length of the tool input JSON.
	InputSize int

	// ToolUseID correlates start↔completion when multiple parallel tool
	// calls share a node. Populated for OnToolStarted; the post-execution
	// OnToolCall path leaves it empty (existing callers don't need it).
	ToolUseID string

	// Input is the raw JSON arguments passed to the tool. Populated for
	// OnToolStarted so observers can render the tool's target (URL, file
	// path, query, …) and persist structured input for whitelisted tools.
	// Empty on the post-execution OnToolCall path to avoid duplicating
	// payloads in events.jsonl.
	Input json.RawMessage

	// Output is the string the tool returned to the LLM. Populated only on
	// the post-execution OnToolCall path; empty on OnToolStarted. The
	// hooks layer persists it (truncated) on the tool_called event so the
	// studio's per-node Tools tab can render the in/out pair the way
	// Claude Code does.
	Output string

	// Duration is how long the tool execution took.
	Duration time.Duration

	// Error is non-nil if the tool execution failed.
	Error error
}
