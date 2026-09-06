package model

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The sandboxed claw path runs the LLM loop in `iterion __claw-runner`
// inside the container, a process with no event hooks of its own. Left
// there, the per-step observations — which model a call went to, what it
// consumed — never reach the host: the run store carries no llm_request /
// llm_step_finished for the node, no usage_progress can be derived for a
// supervisor's cost_gt monitor, and the runner's metering sees the
// delegation total alone.
//
// This file is the relay across that boundary, on the IPC channel the
// runner already has: the runner installs SandboxRelayHooks, which encode
// the two hooks as `event` envelopes; the launcher's multiplexer handler
// decodes them with ApplyRelayedEvent and re-fires its own EventHooks, so a
// sandboxed claw node produces the same events, log lines and derived usage
// samples as an in-process one, through the same code.

// relayedLLMRequest is the wire form of LLMRequestInfo.
type relayedLLMRequest struct {
	Model           string    `json:"model"`
	MessageCount    int       `json:"message_count"`
	ToolCount       int       `json:"tool_count"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	Iteration       int       `json:"iteration"`
}

// relayedLLMStep is the wire form of LLMStepInfo.
type relayedLLMStep struct {
	Number           int               `json:"step"`
	Text             string            `json:"text,omitempty"`
	ToolCalls        []relayedToolCall `json:"tool_calls,omitempty"`
	FinishReason     string            `json:"finish_reason,omitempty"`
	InputTokens      int               `json:"input_tokens"`
	OutputTokens     int               `json:"output_tokens"`
	CacheReadTokens  int               `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int               `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int               `json:"thinking_tokens,omitempty"`
	ThinkingMs       int               `json:"thinking_ms,omitempty"`
	Thinking         string            `json:"thinking,omitempty"`
	Iteration        int               `json:"iteration"`
}

// relayedToolCall is the wire form of ToolCallEntry.
type relayedToolCall struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// SandboxRelayHooks returns the EventHooks the in-container claw runner
// installs: OnLLMRequest and OnLLMStepFinish are encoded as `event`
// envelopes and handed to write, the runner's IPC writer. A hook cannot
// return an error, so a failed write is passed to report (nil-safe); a
// channel that fails here fails the result envelope right after, loudly.
func SandboxRelayHooks(write func(delegate.Envelope) error, report func(error)) EventHooks {
	send := func(eventType store.EventType, payload any) {
		env, err := relayEnvelope(eventType, payload)
		if err == nil {
			err = write(env)
		}
		if err != nil && report != nil {
			report(fmt.Errorf("relay %s event to the launcher: %w", eventType, err))
		}
	}
	return EventHooks{
		OnLLMRequest: func(_ string, info LLMRequestInfo) {
			send(store.EventLLMRequest, relayedLLMRequest(info))
		},
		OnLLMStepFinish: func(_ string, step LLMStepInfo) {
			var calls []relayedToolCall
			if len(step.ToolCalls) > 0 {
				calls = make([]relayedToolCall, len(step.ToolCalls))
				for i, tc := range step.ToolCalls {
					calls[i] = relayedToolCall(tc)
				}
			}
			send(store.EventLLMStepFinished, relayedLLMStep{
				Number:           step.Number,
				Text:             step.Text,
				ToolCalls:        calls,
				FinishReason:     step.FinishReason,
				InputTokens:      step.InputTokens,
				OutputTokens:     step.OutputTokens,
				CacheReadTokens:  step.CacheReadTokens,
				CacheWriteTokens: step.CacheWriteTokens,
				ReasoningTokens:  step.ReasoningTokens,
				ThinkingMs:       step.ThinkingMs,
				Thinking:         step.Thinking,
				Iteration:        step.Iteration,
			})
		},
	}
}

// relayEnvelope encodes a typed payload as an event envelope. EventData
// carries its payload as a map, so the typed form goes through JSON once
// here and once again on the host — two small encodes per LLM step.
func relayEnvelope(eventType store.EventType, payload any) (delegate.Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return delegate.Envelope{}, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return delegate.Envelope{}, err
	}
	return delegate.NewEventEnvelope(string(eventType), m)
}

// ApplyRelayedEvent re-fires one relayed event through h, for nodeID (the
// host's own node id, never the wire's). handled is false for an event type
// this host does not relay — a newer runner may forward more, and the
// multiplexer's contract for an envelope the launcher does not know is to
// drop it. The error is for a known type whose payload does not decode: a
// defect on one side of the wire, not a version skew, and the node's
// metering is incomplete without it.
func ApplyRelayedEvent(h EventHooks, nodeID, eventType string, payload map[string]any) (handled bool, err error) {
	switch store.EventType(eventType) {
	case store.EventLLMRequest:
		var r relayedLLMRequest
		if err := decodeRelayed(payload, &r); err != nil {
			return true, err
		}
		if h.OnLLMRequest != nil {
			h.OnLLMRequest(nodeID, LLMRequestInfo(r))
		}
		return true, nil
	case store.EventLLMStepFinished:
		var s relayedLLMStep
		if err := decodeRelayed(payload, &s); err != nil {
			return true, err
		}
		if h.OnLLMStepFinish != nil {
			var calls []ToolCallEntry
			if len(s.ToolCalls) > 0 {
				calls = make([]ToolCallEntry, len(s.ToolCalls))
				for i, tc := range s.ToolCalls {
					calls[i] = ToolCallEntry(tc)
				}
			}
			h.OnLLMStepFinish(nodeID, LLMStepInfo{
				Number:           s.Number,
				Text:             s.Text,
				ToolCalls:        calls,
				FinishReason:     s.FinishReason,
				InputTokens:      s.InputTokens,
				OutputTokens:     s.OutputTokens,
				CacheReadTokens:  s.CacheReadTokens,
				CacheWriteTokens: s.CacheWriteTokens,
				ReasoningTokens:  s.ReasoningTokens,
				ThinkingMs:       s.ThinkingMs,
				Thinking:         s.Thinking,
				Iteration:        s.Iteration,
			})
		}
		return true, nil
	}
	return false, nil
}

// decodeRelayed reads a relayed payload map back into its typed wire form.
func decodeRelayed(payload map[string]any, into any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("re-encode payload: %w", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	return nil
}
