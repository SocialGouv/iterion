package delegate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// EnvelopeType discriminates the payload carried over the multiplexed
// NDJSON IPC channel between the iterion launcher (host) and the
// claw runner sub-process (inside the sandbox container). Each type
// has a documented direction (launcher→runner or runner→launcher);
// receivers should ignore unknown types and log them for forward
// compatibility.
type EnvelopeType string

const (
	// EnvelopeTask: launcher → runner. Sent once at startup. Data is
	// the full [IOTask].
	EnvelopeTask EnvelopeType = "task"

	// EnvelopeToolCall: runner → launcher. The runner asks the
	// launcher to execute a tool that lives on the host side
	// (in-process registry, MCP manager). Data is [ToolCallData].
	// The launcher MUST reply with [EnvelopeToolResult] carrying the
	// same ID.
	EnvelopeToolCall EnvelopeType = "tool_call"

	// EnvelopeToolResult: launcher → runner. The launcher's response
	// to a [EnvelopeToolCall]. Data is [ToolResultData]. ID matches
	// the corresponding tool_call so the runner can route the
	// payload back to the waiting in-runner ToolDef closure.
	EnvelopeToolResult EnvelopeType = "tool_result"

	// EnvelopeAskUser: runner → launcher. The runner pauses pending
	// a human answer (mid-tool-loop). Data is [AskUserData]. The
	// launcher routes the request through the engine's pause/resume
	// path and replies with [EnvelopeAskUserAnswer].
	EnvelopeAskUser EnvelopeType = "ask_user"

	// EnvelopeAskUserAnswer: launcher → runner. Carries the human's
	// response. Data is [AskUserAnswerData]. ID matches the
	// corresponding ask_user envelope.
	EnvelopeAskUserAnswer EnvelopeType = "ask_user_answer"

	// EnvelopeSessionCapture: runner → launcher. The runner emits
	// the rolling message store before exit (or periodically) so the
	// launcher can stash it for compaction-retry. Data is opaque
	// JSON understood by the in-runner session store.
	EnvelopeSessionCapture EnvelopeType = "session_capture"

	// EnvelopeSessionReplay: launcher → runner. The launcher
	// pre-loads a previously captured store at startup so the
	// runner resumes from it. Data is the same opaque JSON.
	EnvelopeSessionReplay EnvelopeType = "session_replay"

	// EnvelopeEvent: runner → launcher. Observability passthrough —
	// the runner forwards events that should be appended to the run's
	// events.jsonl. Data is [EventData].
	EnvelopeEvent EnvelopeType = "event"

	// EnvelopeResult: runner → launcher. The terminal envelope. The
	// runner exits after sending. Data is the [IOResult].
	EnvelopeResult EnvelopeType = "result"
)

// Envelope is a single NDJSON line on the IPC channel. Type drives the
// dispatch, ID correlates request/response pairs (tool_call/result,
// ask_user/answer), Data is the type-specific payload. Each line on
// the wire MUST be exactly one Envelope JSON object terminated by
// newline.
type Envelope struct {
	Type EnvelopeType    `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ToolCallData is the payload of an [EnvelopeToolCall]. The runner
// fills Name from the tool registry and Input from the LLM tool_use
// block. Schema validation happens on the launcher side, where the
// authoritative tool registry lives.
type ToolCallData struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolResultData is the payload of an [EnvelopeToolResult]. Exactly
// one of Output / Error / AskUser is set:
//
//   - Output: whatever the tool's Execute returned (typically a
//     stringified text block).
//   - Error: a human-readable failure summary the runner translates
//     into a generic Go error inside the proxy ToolDef closure.
//   - AskUser (V2-3): the launcher-side ToolDef returned a
//     [*ErrAskUser]; the runner-side proxy reconstructs the typed
//     error so the LLM loop's existing pause/resume path triggers
//     identically to the unsandboxed case.
type ToolResultData struct {
	Output  string           `json:"output,omitempty"`
	Error   string           `json:"error,omitempty"`
	AskUser *AskUserToolFail `json:"ask_user,omitempty"`
}

// AskUserToolFail mirrors [ErrAskUser] for the wire. It carries the
// pre-pause LLM state (the captured conversation + the pending
// tool_use ID) so the runner-side proxy can rebuild a typed
// [*ErrAskUser] and the engine's existing mid-tool-loop resume path
// rehydrates the exact pre-pause state on the next turn. V2-3.
type AskUserToolFail struct {
	Question         string          `json:"question"`
	PendingToolUseID string          `json:"pending_tool_use_id,omitempty"`
	Conversation     json.RawMessage `json:"conversation,omitempty"`
}

// AskUserData is the payload of an [EnvelopeAskUser]. Mirrors the
// engine's interaction request shape so the launcher can route it
// directly to the human-in-the-loop pause path.
type AskUserData struct {
	Reason    string            `json:"reason,omitempty"`
	Questions []AskUserQuestion `json:"questions"`
}

// AskUserQuestion is one question inside an ask_user envelope.
type AskUserQuestion struct {
	Header      string                  `json:"header,omitempty"`
	Question    string                  `json:"question"`
	Options     []AskUserQuestionOption `json:"options,omitempty"`
	MultiSelect bool                    `json:"multi_select,omitempty"`
}

// AskUserQuestionOption is one selectable option for a question.
type AskUserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskUserAnswerData is the payload of an [EnvelopeAskUserAnswer].
// Answers maps question text → selected option label (or free-form
// text when the user picked "Other").
type AskUserAnswerData struct {
	Answers map[string]string `json:"answers"`
}

// EventData is the payload of an [EnvelopeEvent]. The runner forwards
// observability events that belong in the run's events.jsonl
// (tool_called, llm_request, …) — the launcher persists them via the
// engine's normal emit path.
type EventData struct {
	Type    string                 `json:"type"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// MaxEnvelopeLineBytes caps each NDJSON line. 4 MiB covers typical
// `git log` outputs of large repos and big LLM tool_use payloads
// without unbounded memory exposure. Lines exceeding this trigger an
// error rather than truncation: a side-channel (shared volume) for
// gigantic tool results is V3.
const MaxEnvelopeLineBytes = 4 * 1024 * 1024

// ErrEnvelopeLineTooLong is returned when an incoming NDJSON line
// exceeds [MaxEnvelopeLineBytes]. Callers should surface a clear error
// to the operator pointing at the offending tool name in the most
// recent envelope (typically the failing tool_call).
var ErrEnvelopeLineTooLong = errors.New("delegate: envelope line exceeds MaxEnvelopeLineBytes")

// EnvelopeReader reads NDJSON envelopes from an [io.Reader]. Use
// [NewEnvelopeReader] to construct. Not safe for concurrent use; the
// IPC protocol is single-reader on each side.
type EnvelopeReader struct {
	scanner *bufio.Scanner
}

// NewEnvelopeReader wraps r with a [bufio.Scanner] sized for the
// envelope max-line cap.
func NewEnvelopeReader(r io.Reader) *EnvelopeReader {
	s := bufio.NewScanner(r)
	// bufio.Scanner needs both an initial buffer and a max — without
	// the explicit Buffer call it caps lines at MaxScanTokenSize (64 KiB).
	s.Buffer(make([]byte, 0, 64*1024), MaxEnvelopeLineBytes)
	return &EnvelopeReader{scanner: s}
}

// Read returns the next envelope on the channel. Returns [io.EOF]
// when the writer closed the channel cleanly, or
// [ErrEnvelopeLineTooLong] when a single line exceeds the cap.
func (er *EnvelopeReader) Read() (Envelope, error) {
	if !er.scanner.Scan() {
		err := er.scanner.Err()
		if err == nil {
			return Envelope{}, io.EOF
		}
		if errors.Is(err, bufio.ErrTooLong) {
			return Envelope{}, ErrEnvelopeLineTooLong
		}
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(er.scanner.Bytes(), &env); err != nil {
		return Envelope{}, fmt.Errorf("delegate: decode envelope: %w (line=%q)", err, truncate(string(er.scanner.Bytes()), 256))
	}
	return env, nil
}

// EnvelopeWriter writes NDJSON envelopes to an [io.Writer]. Safe for
// concurrent use — multiple goroutines on the runner side (tool
// proxies, session-capture timer, event forwarder) may emit envelopes
// concurrently, and the mutex serialises the line writes so they
// never interleave.
type EnvelopeWriter struct {
	w  io.Writer
	mu sync.Mutex
}

// NewEnvelopeWriter wraps w as an [EnvelopeWriter].
func NewEnvelopeWriter(w io.Writer) *EnvelopeWriter {
	return &EnvelopeWriter{w: w}
}

// Write marshals env and emits it as a single NDJSON line. Holds the
// internal mutex for the duration of the write to keep lines
// uninterleaved across concurrent goroutines.
func (ew *EnvelopeWriter) Write(env Envelope) error {
	buf, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("delegate: encode envelope: %w", err)
	}
	buf = append(buf, '\n')
	ew.mu.Lock()
	defer ew.mu.Unlock()
	if _, err := ew.w.Write(buf); err != nil {
		return fmt.Errorf("delegate: write envelope: %w", err)
	}
	return nil
}

// NewTaskEnvelope builds a task envelope wrapping an [IOTask]. The
// launcher emits exactly one of these at startup; the runner's
// [EnvelopeReader] expects it as the first envelope on the channel.
func NewTaskEnvelope(task IOTask) (Envelope, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal IOTask: %w", err)
	}
	return Envelope{Type: EnvelopeTask, Data: data}, nil
}

// NewToolCallEnvelope builds a tool_call envelope. ID must be unique
// across the run so the matching tool_result can be routed back to
// the waiting goroutine on the runner side.
func NewToolCallEnvelope(id, name string, input json.RawMessage) (Envelope, error) {
	data, err := json.Marshal(ToolCallData{Name: name, Input: input})
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal tool_call: %w", err)
	}
	return Envelope{Type: EnvelopeToolCall, ID: id, Data: data}, nil
}

// NewToolResultEnvelope builds a tool_result envelope. ID MUST match
// the corresponding [EnvelopeToolCall].
func NewToolResultEnvelope(id, output, errMsg string) (Envelope, error) {
	data, err := json.Marshal(ToolResultData{Output: output, Error: errMsg})
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal tool_result: %w", err)
	}
	return Envelope{Type: EnvelopeToolResult, ID: id, Data: data}, nil
}

// NewResultEnvelope builds the terminal result envelope. The runner
// exits after sending this; the launcher's multiplexer treats it as
// the loop terminator.
func NewResultEnvelope(result IOResult) (Envelope, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal IOResult: %w", err)
	}
	return Envelope{Type: EnvelopeResult, Data: data}, nil
}

// NewEventEnvelope builds an event envelope passing-through an
// observability event from the runner to the launcher's events.jsonl.
func NewEventEnvelope(eventType string, payload map[string]interface{}) (Envelope, error) {
	data, err := json.Marshal(EventData{Type: eventType, Payload: payload})
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal event: %w", err)
	}
	return Envelope{Type: EnvelopeEvent, Data: data}, nil
}

// NewAskUserEnvelope builds an ask_user envelope. ID must be unique
// so the matching ask_user_answer can be routed back to the waiting
// goroutine on the runner side.
func NewAskUserEnvelope(id string, data AskUserData) (Envelope, error) {
	buf, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal ask_user: %w", err)
	}
	return Envelope{Type: EnvelopeAskUser, ID: id, Data: buf}, nil
}

// NewAskUserAnswerEnvelope builds an ask_user_answer envelope. ID
// MUST match the corresponding [EnvelopeAskUser].
func NewAskUserAnswerEnvelope(id string, answers map[string]string) (Envelope, error) {
	data, err := json.Marshal(AskUserAnswerData{Answers: answers})
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal ask_user_answer: %w", err)
	}
	return Envelope{Type: EnvelopeAskUserAnswer, ID: id, Data: data}, nil
}

// NewSessionCaptureEnvelope wraps an opaque session-store snapshot as
// the runner emits it for compaction-retry.
func NewSessionCaptureEnvelope(snapshot json.RawMessage) Envelope {
	return Envelope{Type: EnvelopeSessionCapture, Data: snapshot}
}

// NewSessionReplayEnvelope carries a previously-captured session so
// the runner can pre-load it before the LLM loop starts.
func NewSessionReplayEnvelope(snapshot json.RawMessage) Envelope {
	return Envelope{Type: EnvelopeSessionReplay, Data: snapshot}
}
