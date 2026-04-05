package agent

import (
	"encoding/json"

	"github.com/zendev-sh/goai/provider"
)

// EventType identifies the kind of streaming event from the agent loop.
type EventType int

const (
	// EventTextDelta carries a streaming text chunk.
	EventTextDelta EventType = iota
	// EventToolStart signals that a tool execution is beginning.
	EventToolStart
	// EventToolDone signals that a tool execution has completed.
	EventToolDone
	// EventUsage carries token usage information for a step.
	EventUsage
	// EventStepFinish signals that a complete step has finished.
	EventStepFinish
	// EventPermissionAsk requests interactive permission confirmation.
	// The consumer must send a PermissionDecision on PermReply.
	EventPermissionAsk
	// EventError carries an error from the agent loop.
	EventError
	// EventDone signals that the agent loop has completed.
	EventDone
)

// Event is a streaming event emitted by the agent loop.
// The Type field determines which other fields are populated.
type Event struct {
	// Type identifies this event's kind.
	Type EventType

	// Text content (EventTextDelta).
	Text string

	// ToolName (EventToolStart, EventToolDone, EventPermissionAsk).
	ToolName string

	// ToolInput (EventToolStart, EventPermissionAsk).
	ToolInput json.RawMessage

	// ToolResult (EventToolDone).
	ToolResult string

	// ToolIsError (EventToolDone).
	ToolIsError bool

	// Usage (EventUsage, EventStepFinish).
	Usage provider.Usage

	// Step (EventStepFinish).
	Step StepInfo

	// Err (EventError).
	Err error

	// PermReply (EventPermissionAsk): consumer sends decision here.
	PermReply chan PermissionDecision
}
