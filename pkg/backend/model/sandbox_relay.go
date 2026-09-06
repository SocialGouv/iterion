package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The sandboxed claw path runs the LLM loop in `iterion __claw-runner`
// inside the container, a process with no event hooks of its own. Left
// there, everything that loop observes — which model a call went to, what
// it consumed, which tools it ran and how they answered, when it retried
// or compacted, where each turn ended — never reaches the host: the run
// store carries no llm_request / llm_step_finished / tool_* for the node,
// no usage_progress can be derived for a supervisor's cost_gt monitor, an
// in-container permission denial leaves no audit, `iterion fork --turn`
// has no anchor, and the runner's metering sees the delegation total
// alone.
//
// This file is the relay across that boundary, on the IPC channel the
// runner already has: the runner installs SandboxRelayHooks, which encode
// each hook as an `event` envelope; the launcher's multiplexer handler
// decodes them with ApplyRelayedEvent and re-fires its own EventHooks, so a
// sandboxed claw node produces the same events, turn checkpoints, log lines
// and derived usage samples as an in-process one, through the same code.
//
// One generation callback deliberately does NOT cross: OnLLMResponse.
// The host's own store hooks leave it nil — per-call latency and usage
// surface on llm_step_finished with richer detail — so relaying it would
// fire nothing on the shipped wiring. Its one optional consumer is the
// `--metrics` Prometheus latency histogram, chained through ExtraHooks.
//
// Why the turn SNAPSHOT crosses the wire rather than being written to a
// run directory the host reads back: there is no run directory the
// container can rely on. The host-state bind is the only one that could
// carry `~/.iterion`, and it is absent exactly where the sandbox is
// mandatory — the kubernetes driver REFUSES host_state=auto (no host
// filesystem in a pod), `host_state: none` is the recommended posture for
// a multi-tenant runner, collectHostStateMounts skips a store nested in
// the workspace, and a cloud run's store is Mongo + S3, which no
// bind-mount reaches. The IPC is the one channel every driver and every
// store backend share.

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

// relayedToolStarted is the wire form of LLMToolStartedInfo. InputSize is
// the size the container measured, kept whole even when Input itself is
// clamped: the studio reads it as "how big was this call".
type relayedToolStarted struct {
	ToolName  string          `json:"tool"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	InputSize int             `json:"input_size"`
	Input     json.RawMessage `json:"input,omitempty"`
	Iteration int             `json:"iteration"`
}

// relayedToolFinished is the wire form of LLMToolCallInfo. Error crosses
// as its message — the host rebuilds an error from it, which is what
// routes the event to tool_error and makes an in-container permission
// denial auditable. Duration crosses in milliseconds, the only precision
// the persisted event keeps.
type relayedToolFinished struct {
	ToolName   string `json:"tool"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	InputSize  int    `json:"input_size"`
	Output     string `json:"output,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// relayedRetry is the wire form of RetryInfo.
type relayedRetry struct {
	Attempt    int    `json:"attempt"`
	Error      string `json:"error,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	DelayMs    int64  `json:"delay_ms"`
}

// relayedCompact is the wire form of LLMCompactInfo.
type relayedCompact struct {
	BeforeMessages      int `json:"before_messages"`
	AfterMessages       int `json:"after_messages"`
	RemovedMessageCount int `json:"removed_message_count"`
	Iteration           int `json:"iteration"`
}

// relayedTurnCapture is the wire form of LLMTurnCaptureInfo — the
// fork-from-here anchor. Conversation is the JSON-encoded []api.Message
// the container's loop had accumulated; when it is over
// relayConversationBudget it is left out and ConversationOmittedBytes
// names its size, because half a conversation is not one.
type relayedTurnCapture struct {
	Step                     int               `json:"step"`
	Text                     string            `json:"text,omitempty"`
	ToolCalls                []relayedToolCall `json:"tool_calls,omitempty"`
	FinishReason             string            `json:"finish_reason,omitempty"`
	InputTokens              int               `json:"input_tokens"`
	OutputTokens             int               `json:"output_tokens"`
	CacheReadTokens          int               `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens         int               `json:"cache_write_tokens,omitempty"`
	Iteration                int               `json:"iteration"`
	Backend                  string            `json:"backend,omitempty"`
	SessionID                string            `json:"session_id,omitempty"`
	Conversation             json.RawMessage   `json:"conversation,omitempty"`
	ConversationOmittedBytes int               `json:"conversation_omitted_bytes,omitempty"`
}

// relayTurnCaptureType names the turn capture on the wire. Unlike its
// siblings it is not a store.EventType: a captured turn is persisted as a
// store.TurnCheckpoint, not appended to events.jsonl — the IPC still
// needs a name for it, and both sides key on this one.
const relayTurnCaptureType = "llm_turn_capture"

// A relayed line is one NDJSON line, and the launcher's reader refuses a
// line over delegate.MaxEnvelopeLineBytes by failing the WHOLE channel
// (delegate.ErrEnvelopeLineTooLong) — which would turn an observability
// feature into a run-killer. The container holds payloads no cap bounds:
// a write_file input, a `go test ./...` output, a full conversation. So
// the relay cuts before it writes, always visibly:
//
//   - a text field over relayFieldBudget is truncated and marked, with
//     the original size named in the marker;
//   - a JSON field over it is replaced by a marker OBJECT, since a
//     document cut mid-way is not JSON the host can decode;
//   - a conversation over relayConversationBudget is omitted whole and
//     its size named (relayedTurnCapture.ConversationOmittedBytes);
//   - an event whose fields are each in budget but whose SUM is not (a
//     step dispatching six large write_file calls at once) keeps its
//     numbers and loses its bulk: relayShrinkValue cuts every long
//     string in the encoded payload, marking each. The token counts are
//     what the run is metered on — dropping the event would bill the
//     node wrong;
//   - anything still over the line cap after that is refused and
//     REPORTED (stderr → the node's error), never written: one lost
//     observation beats a dead channel, and neither is silent.
const (
	// relayFieldBudget is the ceiling the host's own hooks already apply
	// to a tool payload they cannot spill to a sidecar blob
	// (maxFieldSize); several of them still fit under the line cap.
	relayFieldBudget = maxFieldSize
	// relayConversationBudget holds a full 200k-token context with room
	// to spare under the 4 MiB line cap.
	relayConversationBudget = 2 * 1024 * 1024
	// relayEnvelopeOverhead covers the `{"type":"event","data":…}\n`
	// wrapper around the payload when checking the line cap.
	relayEnvelopeOverhead = 64
	// relayShrinkFloor is what a string keeps when the whole payload has
	// to be shrunk to fit one line — enough to recognise the content,
	// small enough that hundreds of them fit.
	relayShrinkFloor = 4 * 1024
)

// relayClampText cuts a text field to relayFieldBudget, marking the cut
// and naming the original size — a reader must be able to tell a short
// answer from one the relay could not carry.
func relayClampText(s string) string {
	if len(s) <= relayFieldBudget {
		return s
	}
	return fmt.Sprintf("%s [iterion sandbox relay: cut, %d bytes in the container]",
		iterlog.Truncate(s, relayFieldBudget), len(s))
}

// relayClampJSON replaces a JSON field over relayFieldBudget with a
// marker object naming what stayed behind. A tool input is a document the
// host and the studio parse, so it crosses whole or as this marker —
// never as a fragment.
func relayClampJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) <= relayFieldBudget {
		return raw
	}
	return json.RawMessage(fmt.Sprintf(`{"_iterion_sandbox_relay_omitted_bytes":%d}`, len(raw)))
}

// relayShrinkValue cuts every string past relayShrinkFloor anywhere in an
// already-encoded payload, marking each cut, and reports whether it
// changed anything. It works on the DECODED tree, so a nested document
// (a tool input) stays valid JSON — cutting its raw text would not.
func relayShrinkValue(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		if len(t) <= relayShrinkFloor {
			return t, false
		}
		return fmt.Sprintf("%s [iterion sandbox relay: dropped to fit one IPC line, %d bytes in the container]",
			iterlog.Truncate(t, relayShrinkFloor), len(t)), true
	case map[string]any:
		shrunk := false
		for k, val := range t {
			nv, changed := relayShrinkValue(val)
			if changed {
				t[k] = nv
				shrunk = true
			}
		}
		return t, shrunk
	case []any:
		shrunk := false
		for i, val := range t {
			nv, changed := relayShrinkValue(val)
			if changed {
				t[i] = nv
				shrunk = true
			}
		}
		return t, shrunk
	}
	return v, false
}

// relayLineSize is the NDJSON line an envelope will occupy.
func relayLineSize(env delegate.Envelope) int {
	return len(env.Data) + relayEnvelopeOverhead
}

// relayClampToolCalls converts a step's tool calls to their wire form,
// clamping each input. Shared by the step and the turn capture, which
// carry the same slice.
func relayClampToolCalls(calls []ToolCallEntry) []relayedToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]relayedToolCall, len(calls))
	for i, tc := range calls {
		out[i] = relayedToolCall{Name: tc.Name, Input: relayClampJSON(tc.Input)}
	}
	return out
}

// SandboxRelayHooks returns the EventHooks the in-container claw runner
// installs: every observation its own loop produces — the LLM steps, the
// tools it executes locally, its retries, its compaction rounds and its
// per-turn fork anchors — is encoded as an `event` envelope and handed to
// write, the runner's IPC writer. A hook cannot return an error, so a
// failed write is passed to report (nil-safe); a channel that fails here
// fails the result envelope right after, loudly.
func SandboxRelayHooks(write func(delegate.Envelope) error, report func(error)) EventHooks {
	send := func(eventType string, payload any) {
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
			send(string(store.EventLLMRequest), relayedLLMRequest(info))
		},
		OnLLMStepFinish: func(_ string, step LLMStepInfo) {
			send(string(store.EventLLMStepFinished), relayedLLMStep{
				Number:           step.Number,
				Text:             relayClampText(step.Text),
				ToolCalls:        relayClampToolCalls(step.ToolCalls),
				FinishReason:     step.FinishReason,
				InputTokens:      step.InputTokens,
				OutputTokens:     step.OutputTokens,
				CacheReadTokens:  step.CacheReadTokens,
				CacheWriteTokens: step.CacheWriteTokens,
				ReasoningTokens:  step.ReasoningTokens,
				ThinkingMs:       step.ThinkingMs,
				Thinking:         relayClampText(step.Thinking),
				Iteration:        step.Iteration,
			})
		},
		OnToolStarted: func(_ string, info LLMToolStartedInfo) {
			send(string(store.EventToolStarted), relayedToolStarted{
				ToolName:  info.ToolName,
				ToolUseID: info.ToolUseID,
				InputSize: info.InputSize,
				Input:     relayClampJSON(info.Input),
				Iteration: info.Iteration,
			})
		},
		OnToolCall: func(_ string, info LLMToolCallInfo) {
			var errMsg string
			if info.Error != nil {
				errMsg = relayClampText(info.Error.Error())
			}
			send(string(store.EventToolCalled), relayedToolFinished{
				ToolName:   info.ToolName,
				ToolUseID:  info.ToolUseID,
				InputSize:  info.InputSize,
				Output:     relayClampText(info.Output),
				DurationMs: info.Duration.Milliseconds(),
				Error:      errMsg,
			})
		},
		OnLLMRetry: func(_ string, info RetryInfo) {
			var errMsg string
			if info.Error != nil {
				errMsg = relayClampText(info.Error.Error())
			}
			send(string(store.EventLLMRetry), relayedRetry{
				Attempt:    info.Attempt,
				Error:      errMsg,
				StatusCode: info.StatusCode,
				DelayMs:    info.Delay.Milliseconds(),
			})
		},
		OnLLMCompacted: func(_ string, info LLMCompactInfo) {
			send(string(store.EventLLMCompacted), relayedCompact(info))
		},
		OnLLMTurnCapture: func(_ string, info LLMTurnCaptureInfo) {
			turn := relayedTurnCapture{
				Step:             info.Step,
				Text:             relayClampText(info.Text),
				ToolCalls:        relayClampToolCalls(info.ToolCalls),
				FinishReason:     info.FinishReason,
				InputTokens:      info.InputTokens,
				OutputTokens:     info.OutputTokens,
				CacheReadTokens:  info.CacheReadTokens,
				CacheWriteTokens: info.CacheWriteTokens,
				Iteration:        info.Iteration,
				Backend:          info.Backend,
				SessionID:        info.SessionID,
			}
			// The snapshot is what a fork replays, so it crosses whole or
			// not at all — and when it cannot, the turn still crosses,
			// naming what stayed in the container.
			if conv := info.MarshalConversation(); len(conv) > 0 {
				if len(conv) <= relayConversationBudget {
					turn.Conversation = conv
				} else {
					turn.ConversationOmittedBytes = len(conv)
				}
			}
			send(relayTurnCaptureType, turn)
		},
	}
}

// relayEnvelope encodes a typed payload as an event envelope. EventData
// carries its payload as a map, so the typed form goes through JSON once
// here and once again on the host — two small encodes per LLM step. A
// payload the field clamps could not bring under the IPC's per-line cap
// is refused HERE: writing it would fail the launcher's reader and with
// it the whole channel, so the caller reports one lost observation
// instead of losing the node.
func relayEnvelope(eventType string, payload any) (delegate.Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return delegate.Envelope{}, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return delegate.Envelope{}, err
	}
	env, err := delegate.NewEventEnvelope(eventType, m)
	if err != nil {
		return delegate.Envelope{}, err
	}
	if relayLineSize(env) <= delegate.MaxEnvelopeLineBytes {
		return env, nil
	}
	// Fields each in budget, sum over the cap: keep the numbers, cut the
	// bulk, mark every cut.
	if _, shrunk := relayShrinkValue(m); shrunk {
		if env, err = delegate.NewEventEnvelope(eventType, m); err != nil {
			return delegate.Envelope{}, err
		}
		if relayLineSize(env) <= delegate.MaxEnvelopeLineBytes {
			return env, nil
		}
	}
	return delegate.Envelope{}, fmt.Errorf(
		"payload is %d bytes with every field already cut, over the %d-byte IPC line cap: refused rather than written, which would fail the channel",
		relayLineSize(env), delegate.MaxEnvelopeLineBytes)
}

// ApplyRelayedEvent re-fires one relayed event through h, for nodeID (the
// host's own node id, never the wire's). handled is false for an event type
// this host does not relay — a newer runner may forward more, and the
// multiplexer's contract for an envelope the launcher does not know is to
// drop it. The error is for a known type whose payload does not decode: a
// defect on one side of the wire, not a version skew, and the node's
// metering is incomplete without it.
func ApplyRelayedEvent(h EventHooks, nodeID, eventType string, payload map[string]any) (handled bool, err error) {
	switch eventType {
	case string(store.EventLLMRequest):
		var r relayedLLMRequest
		if err := decodeRelayed(payload, &r); err != nil {
			return true, err
		}
		if h.OnLLMRequest != nil {
			h.OnLLMRequest(nodeID, LLMRequestInfo(r))
		}
		return true, nil
	case string(store.EventLLMStepFinished):
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
	case string(store.EventToolStarted):
		var ts relayedToolStarted
		if err := decodeRelayed(payload, &ts); err != nil {
			return true, err
		}
		if h.OnToolStarted != nil {
			h.OnToolStarted(nodeID, LLMToolStartedInfo{
				ToolName:  ts.ToolName,
				ToolUseID: ts.ToolUseID,
				InputSize: ts.InputSize,
				Input:     ts.Input,
				Iteration: ts.Iteration,
			})
		}
		return true, nil
	case string(store.EventToolCalled):
		var tc relayedToolFinished
		if err := decodeRelayed(payload, &tc); err != nil {
			return true, err
		}
		if h.OnToolCall != nil {
			info := LLMToolCallInfo{
				ToolName:  tc.ToolName,
				ToolUseID: tc.ToolUseID,
				InputSize: tc.InputSize,
				Output:    tc.Output,
				Duration:  time.Duration(tc.DurationMs) * time.Millisecond,
			}
			// A failed call is a tool_error host-side — the shape a
			// permission denial or a tool crash is audited under.
			if tc.Error != "" {
				info.Error = errors.New(tc.Error)
			}
			h.OnToolCall(nodeID, info)
		}
		return true, nil
	case string(store.EventLLMRetry):
		var r relayedRetry
		if err := decodeRelayed(payload, &r); err != nil {
			return true, err
		}
		if h.OnLLMRetry != nil {
			info := RetryInfo{
				Attempt:    r.Attempt,
				StatusCode: r.StatusCode,
				Delay:      time.Duration(r.DelayMs) * time.Millisecond,
			}
			if r.Error != "" {
				info.Error = errors.New(r.Error)
			}
			h.OnLLMRetry(nodeID, info)
		}
		return true, nil
	case string(store.EventLLMCompacted):
		var c relayedCompact
		if err := decodeRelayed(payload, &c); err != nil {
			return true, err
		}
		if h.OnLLMCompacted != nil {
			h.OnLLMCompacted(nodeID, LLMCompactInfo(c))
		}
		return true, nil
	case relayTurnCaptureType:
		var t relayedTurnCapture
		if err := decodeRelayed(payload, &t); err != nil {
			return true, err
		}
		if h.OnLLMTurnCapture == nil {
			return true, nil
		}
		info := LLMTurnCaptureInfo{
			Step:                     t.Step,
			Text:                     t.Text,
			FinishReason:             t.FinishReason,
			InputTokens:              t.InputTokens,
			OutputTokens:             t.OutputTokens,
			CacheReadTokens:          t.CacheReadTokens,
			CacheWriteTokens:         t.CacheWriteTokens,
			Iteration:                t.Iteration,
			Backend:                  t.Backend,
			SessionID:                t.SessionID,
			ConversationOmittedBytes: t.ConversationOmittedBytes,
		}
		if len(t.ToolCalls) > 0 {
			info.ToolCalls = make([]ToolCallEntry, len(t.ToolCalls))
			for i, tc := range t.ToolCalls {
				info.ToolCalls[i] = ToolCallEntry(tc)
			}
		}
		// The snapshot re-enters the host as the message slice the
		// in-process path holds, so the turn it persists — and the
		// conversation a fork replays from it — is the same artifact.
		if len(t.Conversation) > 0 {
			var msgs []api.Message
			if err := json.Unmarshal(t.Conversation, &msgs); err != nil {
				return true, fmt.Errorf("decode relayed conversation: %w", err)
			}
			info.conversation = msgs
		}
		h.OnLLMTurnCapture(nodeID, info)
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
