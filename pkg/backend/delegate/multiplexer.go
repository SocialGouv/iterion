package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MultiplexerHandler dispatches the launcher-side responses to envelopes
// the runner emits mid-stream. All four hooks are optional: nil means
// "treat as not-implemented and surface a structured error to the
// runner" (for tool_call / ask_user — they need a reply) or "drop
// silently" (for session_capture / event — fire-and-forget by design).
//
// V2-1 ships the multiplexer with all hooks nil-safe so the only path
// exercised in the existing wire (task → result) keeps working
// unchanged. V2-2 wires OnToolCall to the in-process tool registry +
// MCP manager; V2-3 wires OnAskUser to the engine pause path; V2-4
// wires OnSessionCapture to the session store for compaction-retry.
type MultiplexerHandler struct {
	// OnToolCall executes a tool the runner asked for and returns
	// the (output, err) pair the launcher will forward back as a
	// [EnvelopeToolResult]. The id is opaque to the handler — the
	// multiplexer routes the response back by ID automatically.
	OnToolCall func(ctx context.Context, name string, input json.RawMessage) (string, error)

	// OnAskUser routes a runner-side ask_user request through the
	// engine's pause path and returns the answers map once the run
	// resumes. The multiplexer encodes the answers into an
	// [EnvelopeAskUserAnswer] correlated by id.
	OnAskUser func(ctx context.Context, data AskUserData) (map[string]string, error)

	// OnSessionCapture stores a runner-emitted session snapshot for
	// compaction-retry. Fire-and-forget — the multiplexer doesn't
	// echo a reply and proceeds with the next envelope.
	OnSessionCapture func(snapshot json.RawMessage)

	// OnEvent forwards a runner-emitted observability event to the
	// engine's events.jsonl pipeline. Fire-and-forget.
	OnEvent func(eventType string, payload map[string]interface{})
}

// Multiplexer drives the launcher-side NDJSON loop: it reads
// envelopes from the runner's stdout, dispatches them to the
// [MultiplexerHandler] hooks, sends correlated replies via the
// runner's stdin, and returns the terminal [IOResult] when a
// [EnvelopeResult] arrives.
//
// Single-use: callers construct one per runner invocation.
type Multiplexer struct {
	reader  *EnvelopeReader
	writer  *EnvelopeWriter
	handler MultiplexerHandler
}

// NewMultiplexer wraps the runner's stdout reader + stdin writer.
// The handler may be the zero value (all hooks nil) — see
// [MultiplexerHandler].
func NewMultiplexer(stdout io.Reader, stdin io.Writer, h MultiplexerHandler) *Multiplexer {
	return &Multiplexer{
		reader:  NewEnvelopeReader(stdout),
		writer:  NewEnvelopeWriter(stdin),
		handler: h,
	}
}

// Send writes env to the runner's stdin. Used by callers to seed the
// initial [EnvelopeTask] (and a [EnvelopeSessionReplay] when applicable
// in V2-4).
func (m *Multiplexer) Send(env Envelope) error {
	return m.writer.Write(env)
}

// Run drives the loop. Returns the terminal [IOResult] when a
// [EnvelopeResult] is received, or [io.EOF] / a wire-format error
// when the runner closed the channel without sending one. Honours
// ctx — when ctx is cancelled, the next envelope dispatch returns
// ctx.Err() promptly (the underlying [EnvelopeReader] doesn't itself
// listen on ctx, so callers wanting interrupt-driven cancellation
// should also close the runner's stdout from outside).
func (m *Multiplexer) Run(ctx context.Context) (IOResult, error) {
	for {
		if err := ctx.Err(); err != nil {
			return IOResult{}, err
		}
		env, err := m.reader.Read()
		if err != nil {
			return IOResult{}, err
		}
		switch env.Type {
		case EnvelopeResult:
			var ioRes IOResult
			if err := json.Unmarshal(env.Data, &ioRes); err != nil {
				return IOResult{}, fmt.Errorf("delegate: decode IOResult: %w", err)
			}
			return ioRes, nil

		case EnvelopeToolCall:
			if err := m.handleToolCall(ctx, env); err != nil {
				return IOResult{}, err
			}

		case EnvelopeAskUser:
			if err := m.handleAskUser(ctx, env); err != nil {
				return IOResult{}, err
			}

		case EnvelopeSessionCapture:
			if m.handler.OnSessionCapture != nil {
				m.handler.OnSessionCapture(env.Data)
			}

		case EnvelopeEvent:
			if m.handler.OnEvent != nil {
				var ev EventData
				if err := json.Unmarshal(env.Data, &ev); err == nil {
					m.handler.OnEvent(ev.Type, ev.Payload)
				}
			}

		default:
			// Unknown envelope types: ignore for forward compatibility.
			// Future runner versions may emit envelopes a launcher
			// doesn't yet understand; dropping them is preferable to
			// failing the run.
		}
	}
}

func (m *Multiplexer) handleToolCall(ctx context.Context, env Envelope) error {
	var call ToolCallData
	if err := json.Unmarshal(env.Data, &call); err != nil {
		return fmt.Errorf("delegate: decode tool_call: %w", err)
	}
	var (
		output  string
		callErr error
	)
	if m.handler.OnToolCall == nil {
		callErr = errors.New("launcher: no tool dispatcher registered (host doesn't know how to execute this tool)")
	} else {
		output, callErr = m.handler.OnToolCall(ctx, call.Name, call.Input)
	}
	data := ToolResultData{Output: output}
	if callErr != nil {
		// V2-3: when the launcher-side ToolDef returns *ErrAskUser
		// (the canonical pause-then-resume signal from the ask_user
		// tool), preserve the typed payload so the runner-side proxy
		// can rebuild a *ErrAskUser. Without this, the LLM loop's
		// pause/resume path can't trigger across the sandbox boundary.
		var askErr *ErrAskUser
		if errors.As(callErr, &askErr) {
			data.AskUser = &AskUserToolFail{
				Question:         askErr.Question,
				PendingToolUseID: askErr.PendingToolUseID,
				Conversation:     askErr.Conversation,
			}
		} else {
			data.Error = callErr.Error()
		}
	}
	buf, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("delegate: marshal tool_result: %w", err)
	}
	reply := Envelope{Type: EnvelopeToolResult, ID: env.ID, Data: buf}
	return m.writer.Write(reply)
}

func (m *Multiplexer) handleAskUser(ctx context.Context, env Envelope) error {
	var data AskUserData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return fmt.Errorf("delegate: decode ask_user: %w", err)
	}
	var answers map[string]string
	if m.handler.OnAskUser != nil {
		var err error
		answers, err = m.handler.OnAskUser(ctx, data)
		if err != nil {
			// Surface the error back to the runner so the proxy
			// ask_user closure can return it as a Go error inside
			// the LLM loop. The launcher itself does NOT abort the
			// multiplexer — the runner decides whether the failure
			// is fatal.
			return m.replyAskUserError(env.ID, err.Error())
		}
	} else {
		return m.replyAskUserError(env.ID, "launcher: no ask_user handler registered")
	}
	reply, err := NewAskUserAnswerEnvelope(env.ID, answers)
	if err != nil {
		return err
	}
	return m.writer.Write(reply)
}

// replyAskUserError encodes an ask_user failure into the answers map
// using the conventional `__error__` key the runner-side proxy
// recognises and translates into a Go error.
func (m *Multiplexer) replyAskUserError(id, errMsg string) error {
	reply, err := NewAskUserAnswerEnvelope(id, map[string]string{AskUserErrorKey: errMsg})
	if err != nil {
		return err
	}
	return m.writer.Write(reply)
}

// AskUserErrorKey is the conventional answers-map key used to surface
// a launcher-side ask_user dispatch error back to the runner. Both
// sides reference this constant to avoid drift.
const AskUserErrorKey = "__error__"
