package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// proxyDispatcher is the runner-side counterpart to the launcher-side
// [delegate.Multiplexer]. It owns the envelope channel back to the
// launcher: a single reader goroutine consumes envelopes from stdin
// and routes them to per-correlation-ID response channels; a writer
// (mutex-protected) serialises envelopes to stdout.
//
// The proxy ToolDef closures (and V2-3's ask_user closure) call the
// dispatcher to round-trip a request → response pair across the IPC.
// Each call gets a fresh ID so concurrent tool_use blocks (parallel
// tool calls in Anthropic / OpenAI loops) don't collide.
type proxyDispatcher struct {
	reader *delegate.EnvelopeReader
	writer *delegate.EnvelopeWriter

	mu      sync.Mutex
	pending map[string]chan delegate.Envelope

	nextID atomic.Uint64

	done chan struct{} // closed AFTER failAllPending — see start()
}

// newProxyDispatcher wraps the runner's stdin/stdout pair as a
// dispatcher. Caller must invoke [proxyDispatcher.start] exactly once
// before any [proxyDispatcher.callTool] (or future [callAskUser])
// invocation.
func newProxyDispatcher(stdin io.Reader, stdout io.Writer) *proxyDispatcher {
	return &proxyDispatcher{
		reader:  delegate.NewEnvelopeReader(stdin),
		writer:  delegate.NewEnvelopeWriter(stdout),
		pending: make(map[string]chan delegate.Envelope),
		done:    make(chan struct{}),
	}
}

// readNextEnvelope is exposed to the bootstrap path (which reads the
// initial task envelope synchronously before [start] kicks in).
func (d *proxyDispatcher) readNextEnvelope() (delegate.Envelope, error) {
	return d.reader.Read()
}

// write emits env on stdout. Used by the bootstrap path (terminal
// result envelope) and the proxy ToolDef closures (tool_call
// envelopes). Concurrent-safe.
func (d *proxyDispatcher) write(env delegate.Envelope) error {
	return d.writer.Write(env)
}

// start spawns the reader goroutine. It runs until the launcher
// closes stdin (clean shutdown after the runner emits the terminal
// result envelope) or a wire-format error occurs. Either way, all
// outstanding pending channels receive a synthetic error envelope so
// in-flight proxy calls unblock with a useful message rather than
// hang.
func (d *proxyDispatcher) start() {
	go func() {
		var loopErr error
		// Drain pending channels BEFORE closing d.done so callers
		// racing on `<-d.done` see the synthetic error envelope in
		// their reply channel rather than the bare "channel closed"
		// fallback (each pending call is a buffered-1 channel that
		// failAllPending writes into under d.mu).
		defer func() {
			d.failAllPending(loopErr)
			close(d.done)
		}()
		for {
			env, err := d.reader.Read()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					loopErr = err
				}
				return
			}
			d.route(env)
		}
	}()
}

// route delivers env to the channel registered for env.ID, if any.
// Envelopes without an ID, or with an unknown ID, are dropped — the
// only legitimate sources are tool_result, ask_user_answer, and
// (V2-4) session_replay; all carry the ID of their corresponding
// outbound request.
func (d *proxyDispatcher) route(env delegate.Envelope) {
	if env.ID == "" {
		return
	}
	d.mu.Lock()
	ch, ok := d.pending[env.ID]
	if ok {
		delete(d.pending, env.ID)
	}
	d.mu.Unlock()
	if ok {
		ch <- env
	}
}

// failAllPending unblocks every outstanding proxy call with a
// synthetic envelope carrying err. Used when the reader goroutine
// exits unexpectedly.
func (d *proxyDispatcher) failAllPending(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		err = errors.New("runner: launcher closed envelope channel")
	}
	for id, ch := range d.pending {
		// Synthetic tool_result with the error; the proxy closure
		// translates it into a Go error inside the LLM loop.
		data, _ := json.Marshal(delegate.ToolResultData{Error: err.Error()})
		ch <- delegate.Envelope{Type: delegate.EnvelopeToolResult, ID: id, Data: data}
		delete(d.pending, id)
	}
}

// call sends env (must have a non-empty ID) and blocks until the
// launcher's correlated response arrives, ctx is cancelled, or the
// reader goroutine exits.
func (d *proxyDispatcher) call(ctx context.Context, env delegate.Envelope) (delegate.Envelope, error) {
	if env.ID == "" {
		return delegate.Envelope{}, errors.New("dispatcher: envelope ID required")
	}
	ch := make(chan delegate.Envelope, 1)
	d.mu.Lock()
	d.pending[env.ID] = ch
	d.mu.Unlock()

	if err := d.writer.Write(env); err != nil {
		d.mu.Lock()
		delete(d.pending, env.ID)
		d.mu.Unlock()
		return delegate.Envelope{}, err
	}

	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.pending, env.ID)
		d.mu.Unlock()
		return delegate.Envelope{}, ctx.Err()
	case <-d.done:
		// failAllPending already populated ch with a synthetic error
		// envelope; pull it so callers see the error rather than the
		// generic "channel closed" message.
		select {
		case reply := <-ch:
			return reply, nil
		default:
			return delegate.Envelope{}, errors.New("runner: envelope channel closed before reply")
		}
	}
}

// callTool issues an EnvelopeToolCall and returns the tool_result's
// (output, error) pair. Used by proxy ToolDefs to forward LLM-driven
// tool execution to the launcher.
//
// V2-3: when the tool_result envelope carries an [AskUserToolFail]
// payload, this rebuilds a typed [*delegate.ErrAskUser] so the LLM
// loop's existing pause/resume flow triggers identically to the
// unsandboxed case (without this the ask_user tool could only ever
// surface as a generic error and the engine wouldn't know to pause).
func (d *proxyDispatcher) callTool(ctx context.Context, name string, input json.RawMessage) (string, error) {
	id := "call-" + strconv.FormatUint(d.nextID.Add(1), 10)
	env, err := delegate.NewToolCallEnvelope(id, name, input)
	if err != nil {
		return "", err
	}
	reply, err := d.call(ctx, env)
	if err != nil {
		return "", fmt.Errorf("dispatcher: tool_call: %w", err)
	}
	var data delegate.ToolResultData
	if err := json.Unmarshal(reply.Data, &data); err != nil {
		return "", fmt.Errorf("dispatcher: decode tool_result: %w", err)
	}
	if data.AskUser != nil {
		return "", &delegate.ErrAskUser{
			Question:         data.AskUser.Question,
			PendingToolUseID: data.AskUser.PendingToolUseID,
			Conversation:     data.AskUser.Conversation,
		}
	}
	if data.Error != "" {
		return "", errors.New(data.Error)
	}
	return data.Output, nil
}

// dispatcherCaptureSink implements [model.SessionCaptureSink] by
// emitting [delegate.EnvelopeSessionCapture] envelopes through the
// proxy dispatcher's writer. V2-4 — the launcher's
// [delegate.MultiplexerHandler.OnSessionCapture] receives them and
// mirrors the snapshot into the host's nodeSessionStore so
// CompactAndRetry compacts the LATEST history rather than the
// last-known-good one.
type dispatcherCaptureSink struct {
	dispatcher *proxyDispatcher
}

// Capture is non-blocking by contract (it runs on the LLM loop's hot
// path). Errors writing the envelope are silently dropped — the sink
// is best-effort; failure means the host store falls one snapshot
// behind, which is recoverable on the next save.
func (s *dispatcherCaptureSink) Capture(_ string, _ string, snapshot []byte) {
	if s == nil || s.dispatcher == nil {
		return
	}
	env := delegate.NewSessionCaptureEnvelope(snapshot)
	_ = s.dispatcher.write(env)
}

// makeProxyToolDefs builds [delegate.ToolDef] entries whose Execute
// closures forward each tool call through the dispatcher to the
// launcher. The launcher's [delegate.MultiplexerHandler.OnToolCall]
// invokes the original Execute closure (which lives on the launcher
// side and may close over the MCP manager, the engine's tool
// registry, etc.) and returns the result back across the wire.
func makeProxyToolDefs(defs []delegate.IOToolDef, d *proxyDispatcher) []delegate.ToolDef {
	if len(defs) == 0 {
		return nil
	}
	out := make([]delegate.ToolDef, len(defs))
	for i, td := range defs {
		name := td.Name
		out[i] = delegate.ToolDef{
			Name:        name,
			Description: td.Description,
			InputSchema: td.InputSchema,
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				return d.callTool(ctx, name, input)
			},
		}
	}
	return out
}
