package pisdk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Event type discriminants. The set is pi's AgentEvent (packages/agent) plus
// the session-level extensions in AgentSessionEvent (packages/coding-agent).
// Variants iterion ignores are still named so a reader can tell "known and
// deliberately unused" from "new since the pinned version".
const (
	// Agent lifecycle.
	EventAgentStart = "agent_start"
	// EventAgentEnd carries the authoritative final transcript. It is the
	// correct source for text, usage and stop reason — far more robust than
	// accumulating message_update deltas.
	EventAgentEnd = "agent_end"
	// EventAgentSettled is the true completion boundary of a prompt: it is
	// emitted after subscribed listeners have run and the pending-message
	// queue has drained. The `prompt` RPC response fires at preflight and
	// means only "accepted".
	EventAgentSettled = "agent_settled"

	// Turn lifecycle.
	EventTurnStart = "turn_start"
	EventTurnEnd   = "turn_end"

	// Message lifecycle.
	EventMessageStart = "message_start"
	// EventMessageUpdate repeats the SAME message as it streams. Never
	// accumulate usage from it.
	EventMessageUpdate = "message_update"
	EventMessageEnd    = "message_end"

	// Tool execution lifecycle.
	EventToolExecutionStart  = "tool_execution_start"
	EventToolExecutionUpdate = "tool_execution_update"
	EventToolExecutionEnd    = "tool_execution_end"

	// Compaction.
	EventCompactionStart = "compaction_start"
	EventCompactionEnd   = "compaction_end"

	// pi's own retry loop. iterion disables it where it can (the RPC
	// `set_auto_retry` command) because nested retries hide rate limits from
	// the executor's classifier and burn quota; seeing one of these means it
	// is still active.
	EventAutoRetryStart = "auto_retry_start"
	EventAutoRetryEnd   = "auto_retry_end"

	// Observational.
	EventEntryAppended       = "entry_appended"
	EventQueueUpdate         = "queue_update"
	EventSessionInfoChanged  = "session_info_changed"
	EventThinkingLevelChange = "thinking_level_changed"
	EventBashExecutionUpdate = "bash_execution_update"

	EventSummarizationRetryScheduled   = "summarization_retry_scheduled"
	EventSummarizationRetryAttemptStar = "summarization_retry_attempt_start"
	EventSummarizationRetryFinished    = "summarization_retry_finished"
)

// SessionHeader is the first line of a `--mode json` stream. RPC mode does
// not emit it (the session id comes from get_state instead).
type SessionHeader struct {
	Type          string `json:"type"` // always "session"
	Version       int    `json:"version,omitempty"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

// Event is one decoded line of pi's event stream, flattened from pi's
// discriminated union. Type is the discriminant; fields belonging to other
// variants stay zero.
type Event struct {
	Type string `json:"type"`

	// message_start / message_update / message_end / turn_end
	Message *Message `json:"message,omitempty"`
	// turn_end
	ToolResults []Message `json:"toolResults,omitempty"`
	// agent_end
	Messages  []Message `json:"messages,omitempty"`
	WillRetry bool      `json:"willRetry,omitempty"`

	// tool_execution_*
	ToolCallID    string          `json:"toolCallId,omitempty"`
	ToolName      string          `json:"toolName,omitempty"`
	Args          json.RawMessage `json:"args,omitempty"`
	PartialResult json.RawMessage `json:"partialResult,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	IsError       bool            `json:"isError,omitempty"`

	// compaction_start / compaction_end
	Reason  string `json:"reason,omitempty"` // manual | threshold | overflow
	Aborted bool   `json:"aborted,omitempty"`

	// auto_retry_start / auto_retry_end
	Attempt      int    `json:"attempt,omitempty"`
	MaxAttempts  int    `json:"maxAttempts,omitempty"`
	DelayMs      int    `json:"delayMs,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Success      bool   `json:"success,omitempty"`
	FinalError   string `json:"finalError,omitempty"`

	// queue_update
	Steering []string `json:"steering,omitempty"`
	FollowUp []string `json:"followUp,omitempty"`

	// session_info_changed / thinking_level_changed / bash_execution_update
	Name  string `json:"name,omitempty"`
	Level string `json:"level,omitempty"`
	Delta string `json:"delta,omitempty"`

	// Raw is the undecoded line, kept so a caller can inspect a variant this
	// port does not model yet.
	Raw json.RawMessage `json:"-"`
}

// Stream is a decoded pi event stream: the optional `--mode json` session
// header plus every event line, in order.
type Stream struct {
	Header *SessionHeader
	Events []Event
	// Unparsed holds lines that were not valid JSON objects. A non-empty
	// value on an otherwise-empty stream means the output was not pi's
	// machine-readable format at all (a crash message, a usage banner), and
	// the caller should fall back to treating stdout as plain text.
	Unparsed []string
}

// DecodeStream parses NDJSON from r. It never fails on a malformed or
// unknown line: pi ships roughly weekly, and a new event variant must be
// inert for iterion rather than fatal.
//
// Lines are split on LF only, matching pi's own framing (its jsonl.ts warns
// that Node's readline additionally breaks on U+2028/U+2029, which are legal
// inside JSON string payloads). A trailing CR is trimmed.
//
// The reader has no line-length cap: a single tool_execution_end carrying a
// large file read is routinely megabytes, and bufio.Scanner's default 64 KiB
// limit would silently truncate the stream mid-run.
func DecodeStream(r io.Reader) (Stream, error) {
	var out Stream
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			decodeLine(&out, line)
		}
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
	}
}

// DecodeStreamString is DecodeStream over an in-memory stream.
func DecodeStreamString(s string) Stream {
	out, _ := DecodeStream(strings.NewReader(s))
	return out
}

func decodeLine(out *Stream, line string) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
	if trimmed == "" {
		return
	}
	if trimmed[0] != '{' {
		out.Unparsed = append(out.Unparsed, trimmed)
		return
	}
	raw := json.RawMessage(bytes.Clone([]byte(trimmed)))

	// The session header shares the `type` key with events; it is only ever
	// the first line of a print-mode stream.
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		out.Unparsed = append(out.Unparsed, trimmed)
		return
	}
	if probe.Type == "session" && out.Header == nil {
		var h SessionHeader
		if json.Unmarshal(raw, &h) == nil && h.ID != "" {
			out.Header = &h
			return
		}
	}

	var ev Event
	if json.Unmarshal(raw, &ev) != nil {
		out.Unparsed = append(out.Unparsed, trimmed)
		return
	}
	ev.Raw = raw
	out.Events = append(out.Events, ev)
}

// FinalTranscript returns the authoritative message list for the run: the
// last agent_end that is not followed by another attempt (WillRetry marks a
// transcript pi is about to discard and retry).
//
// Returns nil when the stream carries no usable agent_end — a truncated or
// crashed run — so the caller can fall back to message_end scanning.
func (s Stream) FinalTranscript() []Message {
	for i := len(s.Events) - 1; i >= 0; i-- {
		ev := s.Events[i]
		if ev.Type == EventAgentEnd && !ev.WillRetry && len(ev.Messages) > 0 {
			return ev.Messages
		}
	}
	return nil
}

// AssistantMessages returns the run's assistant messages, de-duplicated by
// identity and in stream order.
//
// It prefers the final transcript; failing that it reconstructs from
// message_end events (one per completed message). It deliberately ignores
// message_update, which re-emits the same message on every streaming delta.
func (s Stream) AssistantMessages() []Message {
	seen := make(map[string]bool)
	var out []Message
	add := func(m Message) {
		if !m.IsAssistant() {
			return
		}
		id := m.Identity()
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, m)
	}

	if transcript := s.FinalTranscript(); transcript != nil {
		for _, m := range transcript {
			add(m)
		}
		return out
	}
	for _, ev := range s.Events {
		if ev.Type == EventMessageEnd && ev.Message != nil {
			add(*ev.Message)
		}
	}
	return out
}
