package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Each queue has a fixed number of slots for line-capped envelopes.
const (
	multiplexerPendingCalls   = 4
	multiplexerPendingReplies = 4
)

const defaultMultiplexerReplyDrain = time.Second

const multiplexerBusy = "launcher: tool/ask_user relay is at capacity"

// MultiplexerHandler dispatches the launcher-side responses to envelopes
// the runner emits mid-stream. Hooks are optional: nil means
// "treat as not-implemented and surface a structured error to the
// runner" (for tool_call / ask_user — they need a reply) or "drop
// silently" (for session_capture / event — fire-and-forget by design).
//
// Tool, Ask and state hooks share a serial worker and must stop when their context
// is cancelled. Event and capture hooks run in wire order, independently of
// the worker: any state shared across the two lanes needs synchronization.
type MultiplexerHandler struct {
	// OnToolCall executes a tool the runner asked for and returns
	// the (output, err) pair the launcher will forward back as a
	// [EnvelopeToolResult]. The id is opaque to the handler — the
	// multiplexer routes the response back by ID automatically.
	OnToolCall func(ctx context.Context, name string, input json.RawMessage) (string, error)

	// OnCodemodeState serves only the run bank bound by the launcher. It
	// is not a tool call and cannot reach the tool registry.
	OnCodemodeState func(ctx context.Context, req StateRequest) (StateReply, error)

	// OnAskUser routes a runner-side ask_user request through the
	// engine's pause path and returns the answers map once the run
	// resumes. The multiplexer encodes the answers into an
	// [EnvelopeAskUserAnswer] correlated by id.
	OnAskUser func(ctx context.Context, data AskUserData) (map[string]string, error)

	// OnPermissionAsk serves the approval channel's ask (#3513): the
	// runner's permission gate produced an Ask verdict on a captured exact
	// call. The handler binds the candidate (registered tool, placement,
	// current policy verdict, held approval state) and returns the
	// decision; an allow carries the REAL tool result — the launcher
	// consumed the approval and executed the call itself. A defer means
	// the launcher is suspending the run for this approval; the runner
	// raises the pause instead.
	OnPermissionAsk func(ctx context.Context, ask PermissionAskData) (PermissionDecisionData, error)

	// OnSessionCapture stores a runner-emitted session snapshot for
	// compaction-retry. Fire-and-forget — the multiplexer doesn't
	// echo a reply and proceeds with the next envelope.
	OnSessionCapture func(snapshot json.RawMessage)

	// OnEvent forwards a runner-emitted observability event to the
	// engine's events.jsonl pipeline. Fire-and-forget.
	OnEvent func(eventType string, payload map[string]any)
}

// Multiplexer drives the launcher-side NDJSON loop: it reads
// envelopes from the runner's stdout, dispatches them to the
// [MultiplexerHandler] hooks, sends correlated replies via the
// runner's stdin, and returns the terminal [IOResult] when a
// [EnvelopeResult] arrives.
//
// Single-use: callers construct one per runner invocation. Run closes the
// launcher ends if they implement [io.Closer], to interrupt pending I/O on
// shutdown. A stream that can block must support concurrent Close that wakes
// its pending operation.
type Multiplexer struct {
	reader     *EnvelopeReader
	writer     *EnvelopeWriter
	handler    MultiplexerHandler
	stdout     io.Closer
	stdin      io.Closer
	replyDrain time.Duration
}

// NewMultiplexer wraps the runner's stdout reader + stdin writer.
// The handler may be the zero value (all hooks nil) — see
// [MultiplexerHandler].
func NewMultiplexer(stdout io.Reader, stdin io.Writer, h MultiplexerHandler) *Multiplexer {
	stdoutCloser, _ := stdout.(io.Closer)
	stdinCloser, _ := stdin.(io.Closer)
	reader := NewEnvelopeReader(stdout)
	if h.OnCodemodeState != nil {
		reader.OmitDecodePayload()
	}
	return &Multiplexer{
		reader:     reader,
		writer:     NewEnvelopeWriter(stdin),
		handler:    h,
		stdout:     stdoutCloser,
		stdin:      stdinCloser,
		replyDrain: defaultMultiplexerReplyDrain,
	}
}

// Send writes env to the runner's stdin. Used by callers to seed the
// initial [EnvelopeTask] (and a [EnvelopeSessionReplay] when applicable
// in V2-4).
func (m *Multiplexer) Send(env Envelope) error {
	return m.writer.Write(env)
}

// SetRedactor installs the run's pass on the envelope reader, so the raw
// line a decode error quotes crosses it before the error's 256-byte cut
// (iterion#2822). Call before Run; the read loop is single-goroutine.
func (m *Multiplexer) SetRedactor(redact func(string) string) {
	m.reader.SetRedactor(redact)
}

// SetReplyDrainTimeout bounds how long a terminal result may wait for
// outstanding replies. A launcher that already reaps its owned pipes gives
// that reaper time to report the underlying write error. Call before Run.
func (m *Multiplexer) SetReplyDrainTimeout(timeout time.Duration) {
	m.replyDrain = timeout
}

// Run drives the relay until a terminal result, cancellation or a wire
// failure. Shutdown cancels outstanding work, interrupts the pipes and joins
// the workers before returning. Send is only for the initial seed, before Run.
func (m *Multiplexer) Run(ctx context.Context) (IOResult, error) {
	ctx, stop := context.WithCancelCause(ctx)
	workCtx, stopWork := context.WithCancel(ctx)
	calls := make(chan func() (Envelope, error), multiplexerPendingCalls)
	replies := make(chan Envelope, multiplexerPendingReplies)
	queueReply := func(reply Envelope, err error) error {
		if err != nil {
			return err
		}
		// Bound retained bytes even for a launcher-provided Ask answer.
		if len(reply.Data)+len(reply.ID) >= MaxEnvelopeLineBytes {
			return fmt.Errorf("delegate: %s reply exceeds the relay line cap: %w", reply.Type, ErrEnvelopeLineTooLong)
		}
		select {
		case replies <- reply:
			return nil
		default:
			return errors.New("delegate: reply backlog full; cannot queue correlated reply")
		}
	}

	var wg sync.WaitGroup
	var handlerPanic any
	defer func() {
		stopWork()
		stop(nil)
		if m.stdout != nil {
			_ = m.stdout.Close()
		}
		if m.stdin != nil {
			_ = m.stdin.Close()
		}
		wg.Wait()
		if handlerPanic != nil {
			panic(handlerPanic)
		}
	}()

	// The router asks for each read after the previous envelope is routed.
	// A slow ordered callback must not prefetch an answer past the pipe
	// owner's drain boundary.
	type readResult struct {
		env Envelope
		err error
	}
	readNext := make(chan struct{})
	incoming := make(chan readResult)
	wg.Go(func() {
		for {
			select {
			case <-readNext:
			case <-ctx.Done():
				return
			}
			env, err := m.reader.Read()
			select {
			case incoming <- readResult{env, err}:
			case <-ctx.Done():
				return
			}
			if err != nil || env.Type == EnvelopeResult {
				return
			}
		}
	})

	// Tool calls, Ask and state share one FIFO execution lane. The runner cannot
	// opt a call into parallel execution or bypass the launcher handler.
	workerDone := make(chan struct{})
	wg.Go(func() {
		defer close(workerDone)
		defer func() {
			if p := recover(); p != nil {
				// Re-panic on the caller's stack after shutdown, like a
				// synchronous handler. A worker panic must not kill the process.
				handlerPanic = p
				stop(errors.New("delegate: relay handler panicked"))
			}
		}()
		for {
			select {
			case <-workCtx.Done():
				return
			case call := <-calls:
				if workCtx.Err() != nil {
					return
				}
				if err := queueReply(call()); err != nil {
					stop(err)
					return
				}
			}
		}
	})
	writerDone := make(chan struct{})
	written := 0
	wg.Go(func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case reply, ok := <-replies:
				if !ok || ctx.Err() != nil {
					return
				}
				if err := m.writer.Write(reply); err != nil {
					stop(err)
					return
				}
				written++
			}
		}
	})

	requests := 0
	for {
		select {
		case readNext <- struct{}{}:
		case <-ctx.Done():
			return IOResult{}, context.Cause(ctx)
		}
		var r readResult
		select {
		case <-ctx.Done():
			return IOResult{}, context.Cause(ctx)
		case r = <-incoming:
		}
		if err := context.Cause(ctx); err != nil {
			return IOResult{}, err
		}
		if r.err != nil {
			return IOResult{}, r.err
		}
		env := r.env
		switch env.Type {
		case EnvelopeResult:
			var ioRes IOResult
			if err := json.Unmarshal(env.Data, &ioRes); err != nil {
				return IOResult{}, fmt.Errorf("delegate: decode IOResult: %w", err)
			}
			// Do not turn a premature terminal into success by dropping
			// unanswered requests. Stop effects, but give an active
			// handler's reply and queued replies a bounded write drain.
			stopWork()
			timer := time.NewTimer(m.replyDrain)
			defer timer.Stop()
			wait := func(done <-chan struct{}) error {
				select {
				case <-done:
					return context.Cause(ctx)
				case <-ctx.Done():
					return context.Cause(ctx)
				case <-timer.C:
					return errors.New("delegate: terminal result arrived with replies still pending at the relay drain deadline")
				}
			}
			if err := wait(workerDone); err != nil {
				return IOResult{}, err
			}
			close(replies)
			if err := wait(writerDone); err != nil {
				return IOResult{}, err
			}
			if written != requests {
				return IOResult{}, fmt.Errorf("delegate: terminal result arrived with %d unanswered requests", requests-written)
			}
			return ioRes, nil

		case EnvelopeToolCall:
			requests++
			var call ToolCallData
			if err := json.Unmarshal(env.Data, &call); err != nil {
				return IOResult{}, fmt.Errorf("delegate: decode tool_call: %w", err)
			}
			select {
			case calls <- func() (Envelope, error) { return m.handleToolCall(workCtx, env.ID, call) }:
			default:
				if err := queueReply(newToolResultData(env.ID, ToolResultData{Error: multiplexerBusy})); err != nil {
					return IOResult{}, err
				}
			}

		case EnvelopeStateRequest:
			requests++
			req, err := decodeStateRequest(env)
			if err != nil {
				return IOResult{}, err
			}
			deadline := req.LauncherDeadline(time.Now())
			select {
			case calls <- func() (Envelope, error) {
				stateCtx, cancel := context.WithDeadline(workCtx, deadline)
				defer cancel()
				return m.handleState(stateCtx, env.ID, req)
			}:
			default:
				if err := queueReply(stateEnvelope(EnvelopeStateReply, env.ID, StateReply{Op: req.Op, Code: StateUnavailable})); err != nil {
					return IOResult{}, err
				}
			}

		case EnvelopeAskUser:
			requests++
			var data AskUserData
			if err := json.Unmarshal(env.Data, &data); err != nil {
				return IOResult{}, fmt.Errorf("delegate: decode ask_user: %w", err)
			}
			select {
			case calls <- func() (Envelope, error) { return m.handleAskUser(workCtx, env.ID, data) }:
			default:
				if err := queueReply(askUserErrorReply(env.ID, multiplexerBusy)); err != nil {
					return IOResult{}, err
				}
			}

		case EnvelopePermissionAsk:
			requests++
			var ask PermissionAskData
			if err := json.Unmarshal(env.Data, &ask); err != nil {
				return IOResult{}, fmt.Errorf("delegate: decode permission_ask: %w", err)
			}
			select {
			case calls <- func() (Envelope, error) { return m.handlePermissionAsk(workCtx, env.ID, ask) }:
			default:
				// At capacity: the launcher cannot serve this approval now.
				// Fail the ask — never a denial, and never a pause the
				// launcher could not record: the runner aborts with the
				// reason and the node fails for a retry to serve.
				if err := queueReply(permissionAskFailureReply(env.ID, multiplexerBusy), nil); err != nil {
					return IOResult{}, err
				}
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
			// Ignore unknown types for forward compatibility.
		}
	}
}

func (m *Multiplexer) handleToolCall(ctx context.Context, id string, call ToolCallData) (Envelope, error) {
	var (
		output  string
		callErr error
	)
	if m.handler.OnToolCall == nil {
		callErr = errors.New("launcher: no tool dispatcher registered (host doesn't know how to execute this tool)")
	} else {
		output, callErr = m.handler.OnToolCall(withForwardedToolUse(ctx, call), call.Name, call.Input)
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
				Options:          askErr.Options,
				AllowFreeText:    askErr.AllowFreeText,
			}
		} else {
			data.Error = callErr.Error()
		}
	}
	// Clamped before it goes on the wire: a host-side tool can return more
	// than the channel's line cap, and an unclamped line kills the whole
	// IPC instead of handing the model a bounded result.
	return newToolResultData(id, data)
}

func (m *Multiplexer) handleAskUser(ctx context.Context, id string, data AskUserData) (Envelope, error) {
	if m.handler.OnAskUser == nil {
		return askUserErrorReply(id, "launcher: no ask_user handler registered")
	}
	answers, err := m.handler.OnAskUser(ctx, data)
	if err != nil {
		// The runner decides whether an Ask handler failure is fatal.
		return askUserErrorReply(id, err.Error())
	}
	return NewAskUserAnswerEnvelope(id, answers)
}

func (m *Multiplexer) handlePermissionAsk(ctx context.Context, id string, ask PermissionAskData) (Envelope, error) {
	if m.handler.OnPermissionAsk == nil {
		// No approval handler: this launcher cannot serve the channel at
		// all (it should never have announced it). Fail the ask — the
		// same direction as the busy path.
		return permissionAskFailureReply(id, "launcher: no approval handler registered"), nil
	}
	decision, err := m.handler.OnPermissionAsk(ctx, ask)
	if err != nil {
		// The handler failed before a decision: fail the ask. A pause
		// must be recorded by the handler to be trusted, and an unrecorded
		// one would only refuse at the launcher's binding.
		return permissionAskFailureReply(id, err.Error()), nil
	}
	return NewPermissionDecisionEnvelope(id, decision)
}

// permissionAskFailureReply builds the terminal-shaped failure reply an
// unservable ask gets (busy, no handler, handler error): the runner's
// round trip reads the reason from it and aborts the ask — the node
// fails for a retry to serve, and no unrecorded pause is ever raised.
func permissionAskFailureReply(id, reason string) Envelope {
	buf, _ := json.Marshal(IOResult{Error: reason})
	return Envelope{Type: EnvelopeResult, ID: id, Data: buf}
}

func askUserErrorReply(id, errMsg string) (Envelope, error) {
	return NewAskUserAnswerEnvelope(id, map[string]string{AskUserErrorKey: errMsg})
}

// AskUserErrorKey is the conventional answers-map key used to surface
// a launcher-side ask_user dispatch error back to the runner. Both
// sides reference this constant to avoid drift.
const AskUserErrorKey = "__error__"

// forwardedToolUseKey carries, on the context OnToolCall receives, the run
// event identity the runner sent with the call (ToolCallData.ToolUseID and
// ParentToolUseID).
type forwardedToolUseKey struct{}

type forwardedToolUse struct{ id, parent string }

func withForwardedToolUse(ctx context.Context, call ToolCallData) context.Context {
	return context.WithValue(ctx, forwardedToolUseKey{}, forwardedToolUse{id: call.ToolUseID, parent: call.ParentToolUseID})
}

// ForwardedToolUseFrom reports the tool_use id and parent id of the forwarded
// call an OnToolCall handler is serving — both empty when the runner sent
// none, an older runner's call. The handler stamps them on any event it emits
// about the call, so that event and the runner's own name one call.
func ForwardedToolUseFrom(ctx context.Context) (toolUseID, parentToolUseID string) {
	f, _ := ctx.Value(forwardedToolUseKey{}).(forwardedToolUse)
	return f.id, f.parent
}
