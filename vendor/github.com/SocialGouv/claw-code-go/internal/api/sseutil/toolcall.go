// Package sseutil holds tiny helpers shared between providers that translate
// OpenAI-style SSE tool_call streams into the Anthropic-shaped api.StreamEvent
// vocabulary. Three providers (openai chat completions, openai responses,
// foundry) all run the same little state machine to accumulate id, name and
// argument chunks for each in-flight tool call. Centralising it here keeps
// fixes (e.g. a buffered argument flush after the start event lands) in one
// place instead of three.
package sseutil

import "github.com/SocialGouv/claw-code-go/internal/api"

// ToolCallAccumulator tracks the streaming state for a single tool_use block
// being assembled across many SSE deltas.
//
// Lifecycle:
//
//  1. Caller constructs the accumulator with the block index it should occupy
//     in the output stream (NewToolCallAccumulator).
//  2. For each incoming delta, caller invokes HandleDelta with whatever
//     fragment of {id, name, arguments} arrived. The accumulator emits the
//     ContentBlockStart event the first time both id and name are known, and
//     then emits ContentBlockDelta for every argument chunk that follows.
//     Argument chunks that arrive before id+name are buffered so they aren't
//     dropped on the floor when the start event has not yet been sent.
//  3. When the stream is exhausted, caller invokes Finish() to flush the
//     ContentBlockStop event (only emitted if a start was emitted).
//
// The accumulator does NOT push events itself — it returns a slice the caller
// passes through its existing send-with-ctx-cancel helper, so cancellation
// semantics stay where the goroutine lives.
type ToolCallAccumulator struct {
	blockIndex int

	id   string
	name string

	startEmitted bool
	stopEmitted  bool

	// argBuffer holds argument fragments that arrived before the start event
	// could be emitted (i.e. before both id and name were known). Once the
	// start event lands, the buffered fragments are flushed in arrival order
	// as a single batch of input_json_delta events.
	argBuffer []string
}

// NewToolCallAccumulator returns a fresh accumulator that will emit its
// content block events at the given index.
func NewToolCallAccumulator(blockIndex int) *ToolCallAccumulator {
	return &ToolCallAccumulator{blockIndex: blockIndex}
}

// BlockIndex returns the block index this accumulator owns.
func (a *ToolCallAccumulator) BlockIndex() int { return a.blockIndex }

// Started reports whether ContentBlockStart has already been emitted for this
// tool call.
func (a *ToolCallAccumulator) Started() bool { return a.startEmitted }

// HandleDelta applies an incoming fragment and returns any StreamEvents the
// caller should forward downstream. Empty strings are treated as "no
// fragment" — passing all-empty values is a no-op.
//
// Behaviour:
//   - The first non-empty id is captured; later id values are ignored (the
//     wire protocol echoes the same id on every delta).
//   - The first non-empty name is captured the same way.
//   - Argument fragments arriving before the start event are buffered.
//   - Argument fragments arriving after the start event are emitted directly
//     as input_json_delta events on this accumulator's block index.
func (a *ToolCallAccumulator) HandleDelta(id, name, arguments string) []api.StreamEvent {
	if a.stopEmitted {
		// Defensive: callers should not push deltas after Finish().
		return nil
	}

	if id != "" && a.id == "" {
		a.id = id
	}
	if name != "" && a.name == "" {
		a.name = name
	}

	var events []api.StreamEvent

	// Emit the start event the moment both id and name are known.
	if !a.startEmitted && a.id != "" && a.name != "" {
		a.startEmitted = true
		events = append(events, api.StreamEvent{
			Type:  api.EventContentBlockStart,
			Index: a.blockIndex,
			ContentBlock: api.ContentBlockInfo{
				Type:  "tool_use",
				Index: a.blockIndex,
				ID:    a.id,
				Name:  a.name,
			},
		})
		// Flush any arguments we buffered while waiting for id/name.
		for _, buffered := range a.argBuffer {
			events = append(events, api.StreamEvent{
				Type:  api.EventContentBlockDelta,
				Index: a.blockIndex,
				Delta: api.Delta{Type: "input_json_delta", PartialJSON: buffered},
			})
		}
		a.argBuffer = nil
	}

	if arguments != "" {
		if a.startEmitted {
			events = append(events, api.StreamEvent{
				Type:  api.EventContentBlockDelta,
				Index: a.blockIndex,
				Delta: api.Delta{Type: "input_json_delta", PartialJSON: arguments},
			})
		} else {
			// Buffer until the start event is sent.
			a.argBuffer = append(a.argBuffer, arguments)
		}
	}

	return events
}

// MarkStarted forces the accumulator into the "started" state without going
// through HandleDelta. It returns the StreamEvent the caller should forward.
//
// This is for the responses-API code path where output_item.added arrives
// with both call_id and name already populated and the caller wants to emit
// the start event up front, before any argument deltas land.
func (a *ToolCallAccumulator) MarkStarted(id, name string) api.StreamEvent {
	a.id = id
	a.name = name
	a.startEmitted = true
	return api.StreamEvent{
		Type:  api.EventContentBlockStart,
		Index: a.blockIndex,
		ContentBlock: api.ContentBlockInfo{
			Type:  "tool_use",
			Index: a.blockIndex,
			ID:    a.id,
			Name:  a.name,
		},
	}
}

// Finish emits a ContentBlockStop event when (and only when) the start event
// was previously emitted. It is safe to call Finish() multiple times — the
// second call is a no-op.
func (a *ToolCallAccumulator) Finish() []api.StreamEvent {
	if !a.startEmitted || a.stopEmitted {
		return nil
	}
	a.stopEmitted = true
	return []api.StreamEvent{{
		Type:  api.EventContentBlockStop,
		Index: a.blockIndex,
	}}
}
