package pisdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Ported from packages/coding-agent/src/modes/rpc/rpc-client.ts, whose
// semantics are the reason this is a port and not an improvisation:
//
//   - a response is matched by ID, never by arrival order, because pi
//     dispatches each input line with `void handleInputLine(...)` and does not
//     serialise commands;
//   - the reply to `prompt` fires at PREFLIGHT; the completion boundary is the
//     `agent_settled` event, emitted after extension handlers and the pending
//     message queue have drained;
//   - a line that is neither a response nor valid JSON is IGNORED, not fatal;
//   - process exit or an unwritable stdin must reject every in-flight request,
//     otherwise a caller waits out its whole timeout for a dead process.
//
// One behaviour is deliberately different. The reference client kills with
// SIGTERM; this one closes stdin first, because pi treats end-of-input as a
// graceful shutdown signal (`onInputEnd → shutdown()`) and an RPC pi otherwise
// lives forever by design.

// defaultRequestTimeout matches the reference client's 30s per-request cap.
const defaultRequestTimeout = 30 * time.Second

// Spawner builds the command that runs pi. The default runs the binary
// directly; a host that must route the process elsewhere (into a container,
// say) supplies its own, which is how this package stays free of any
// sandbox dependency.
type Spawner func(ctx context.Context, argv []string) *exec.Cmd

// ClientOptions configures a Client.
type ClientOptions struct {
	// Binary is the pi executable. Ignored when Spawn is set.
	Binary string
	// Args are appended after "--mode rpc".
	Args []string
	// Dir is the working directory. pi has no --cwd flag, so this is the only
	// way to point it at a workspace.
	Dir string
	// Env replaces the child's environment when non-nil.
	Env []string
	// Spawn overrides process construction (sandbox routing, wrappers).
	Spawn Spawner
	// OnEvent receives every event line, in order, on a dedicated goroutine.
	// It must not block for long: pi gates its own stdout on reader
	// backpressure, so a slow consumer slows the agent. The client decouples
	// the reader from this callback, but the queue is not infinite patience.
	OnEvent func(Event)
	// OnUIRequest receives extension UI requests. Returning a UIResponse
	// answers it; returning nil cancels (the safe default). Only called for
	// methods that expect a reply — fire-and-forget ones arrive via OnEvent.
	OnUIRequest func(UIRequest) *UIResponse
	// OnStderr receives pi's stderr line by line. Startup diagnostics (bad
	// model, extension load failure) appear ONLY here, and pi exits non-zero
	// on an error diagnostic — before any handshake completes.
	OnStderr func(line string)
	// RequestTimeout caps a single request. Zero uses defaultRequestTimeout.
	RequestTimeout time.Duration
}

// Client drives one `pi --mode rpc` process.
type Client struct {
	opts ClientOptions

	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu      sync.Mutex
	pending map[string]chan Response
	seq     int
	// exitErr is set once the process dies or stdin breaks; every subsequent
	// request fails with it instead of timing out.
	exitErr error
	closed  bool

	// queue decouples the stdout reader from OnEvent. The reader must never
	// run a callback: pi throttles emission on backpressure, so a slow handler
	// would stall the agent itself.
	queueMu   sync.Mutex
	queueCond *sync.Cond
	queue     []queued
	drained   chan struct{}

	stderrTail *ringBuffer
	// exited is closed by reap once cmd.Wait has returned. Close waits on it
	// rather than calling Wait itself: exec.Cmd.Wait must not be called
	// concurrently, and reap already owns that call.
	exited  chan struct{}
	waitErr error
}

type queued struct {
	event Event
	ui    *UIRequest
}

// NewClient prepares a client. Nothing is spawned until Start.
func NewClient(opts ClientOptions) *Client {
	c := &Client{
		opts:       opts,
		pending:    make(map[string]chan Response),
		drained:    make(chan struct{}),
		exited:     make(chan struct{}),
		stderrTail: newRingBuffer(8 * 1024),
	}
	c.queueCond = sync.NewCond(&c.queueMu)
	return c
}

// Start spawns pi and begins reading its streams.
func (c *Client) Start(ctx context.Context) error {
	if c.cmd != nil {
		return errors.New("pisdk: client already started")
	}

	argv := append([]string{"--mode", "rpc"}, c.opts.Args...)
	if c.opts.Spawn != nil {
		c.cmd = c.opts.Spawn(ctx, argv)
	} else {
		if c.opts.Binary == "" {
			return errors.New("pisdk: no binary configured")
		}
		c.cmd = exec.CommandContext(ctx, c.opts.Binary, argv...) // #nosec G204 — argv is caller-configured, not attacker-controlled
		if c.opts.Dir != "" {
			c.cmd.Dir = c.opts.Dir
		}
		if c.opts.Env != nil {
			c.cmd.Env = c.opts.Env
		}
	}

	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("pisdk: stdin pipe: %w", err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pisdk: stdout pipe: %w", err)
	}
	stderr, err := c.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("pisdk: stderr pipe: %w", err)
	}
	c.stdin = stdin

	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("pisdk: start %s: %w", c.describe(), err)
	}

	go c.readStdout(stdout)
	go c.readStderr(stderr)
	go c.dispatch()
	go c.reap()

	return nil
}

// describe names the process for error messages.
func (c *Client) describe() string {
	if c.opts.Binary != "" {
		return c.opts.Binary
	}
	return "pi"
}

// reap waits for exit and fails every in-flight request, so a caller never
// waits out a full timeout against a process that is already gone.
func (c *Client) reap() {
	err := c.cmd.Wait()
	c.waitErr = err
	close(c.exited)

	exitErr := fmt.Errorf("pisdk: pi exited: %w%s", orNoError(err), c.stderrSuffix())
	c.mu.Lock()
	if c.exitErr == nil {
		c.exitErr = exitErr
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func orNoError(err error) error {
	if err == nil {
		return errors.New("clean exit")
	}
	return err
}

// readStdout classifies every record: a correlated response, or an event.
func (c *Client) readStdout(r io.Reader) {
	_ = ScanLines(r, func(line string) {
		if line == "" {
			return
		}
		var probe struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil {
			// Not JSON. pi routes stray writes to stderr, so this should not
			// happen — and if it does, dropping it is what the reference
			// client does too.
			return
		}

		if probe.Type == "response" && probe.ID != "" {
			var resp Response
			if json.Unmarshal([]byte(line), &resp) == nil && c.deliver(resp) {
				return
			}
			return
		}

		if probe.Type == EventExtensionUIRequest {
			var req UIRequest
			if json.Unmarshal([]byte(line), &req) == nil && req.ID != "" {
				c.enqueue(queued{ui: &req})
				return
			}
		}

		var ev Event
		if json.Unmarshal([]byte(line), &ev) == nil {
			ev.Raw = json.RawMessage(line)
			c.enqueue(queued{event: ev})
		}
	})

	// stdout closed: nothing more will arrive, so let the dispatcher finish.
	c.queueMu.Lock()
	c.closed = true
	c.queueCond.Broadcast()
	c.queueMu.Unlock()
}

func (c *Client) readStderr(r io.Reader) {
	_ = ScanLines(r, func(line string) {
		if line == "" {
			return
		}
		c.stderrTail.add(line)
		if c.opts.OnStderr != nil {
			c.opts.OnStderr(line)
		}
	})
}

// deliver routes a response to its waiter. Reports false when no request is
// waiting on that id (a late reply after a timeout, say).
func (c *Client) deliver(resp Response) bool {
	c.mu.Lock()
	ch, ok := c.pending[resp.ID]
	if ok {
		delete(c.pending, resp.ID)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	ch <- resp
	return true
}

// enqueue hands work to the dispatcher without ever blocking the reader.
func (c *Client) enqueue(item queued) {
	c.queueMu.Lock()
	c.queue = append(c.queue, item)
	c.queueCond.Signal()
	c.queueMu.Unlock()
}

// dispatch runs callbacks off the reader goroutine, in arrival order.
func (c *Client) dispatch() {
	defer close(c.drained)
	for {
		c.queueMu.Lock()
		for len(c.queue) == 0 && !c.closed {
			c.queueCond.Wait()
		}
		if len(c.queue) == 0 && c.closed {
			c.queueMu.Unlock()
			return
		}
		item := c.queue[0]
		c.queue = c.queue[1:]
		c.queueMu.Unlock()

		switch {
		case item.ui != nil:
			c.handleUI(*item.ui)
		case c.opts.OnEvent != nil:
			c.opts.OnEvent(item.event)
		}
	}
}

// handleUI answers a UI request, or cancels it when the host declines.
// A fire-and-forget method needs no reply and is surfaced as an event so a
// host can still observe it.
func (c *Client) handleUI(req UIRequest) {
	if !req.ExpectsReply() {
		if c.opts.OnEvent != nil {
			c.opts.OnEvent(Event{Type: EventExtensionUIRequest, Raw: mustRaw(req)})
		}
		return
	}

	var reply *UIResponse
	if c.opts.OnUIRequest != nil {
		reply = c.opts.OnUIRequest(req)
	}
	if reply == nil {
		cancel := NewUICancel(req.ID)
		reply = &cancel
	}
	_ = c.write(*reply)
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// Send issues a command and waits for its correlated response.
func (c *Client) Send(ctx context.Context, cmd Command) (Response, error) {
	c.mu.Lock()
	if c.exitErr != nil {
		err := c.exitErr
		c.mu.Unlock()
		return Response{}, err
	}
	if c.stdin == nil {
		c.mu.Unlock()
		return Response{}, errors.New("pisdk: client not started")
	}
	c.seq++
	cmd.ID = fmt.Sprintf("it-%d", c.seq)
	ch := make(chan Response, 1)
	c.pending[cmd.ID] = ch
	c.mu.Unlock()

	if err := c.write(cmd); err != nil {
		c.mu.Lock()
		delete(c.pending, cmd.ID)
		c.mu.Unlock()
		return Response{}, err
	}

	timeout := c.opts.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp, ok := <-ch:
		if !ok {
			// reap closed it: the process died while we waited.
			c.mu.Lock()
			err := c.exitErr
			c.mu.Unlock()
			if err == nil {
				err = errors.New("pisdk: pi exited while awaiting a response")
			}
			return Response{}, err
		}
		if !resp.Success {
			return resp, fmt.Errorf("pisdk: %s failed: %s", cmd.Type, resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, cmd.ID)
		c.mu.Unlock()
		return Response{}, ctx.Err()
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, cmd.ID)
		c.mu.Unlock()
		return Response{}, fmt.Errorf("pisdk: timeout after %s awaiting %s%s", timeout, cmd.Type, c.stderrSuffix())
	}
}

// write serialises one record onto pi's stdin.
func (c *Client) write(v any) error {
	line, err := MarshalLine(v)
	if err != nil {
		return fmt.Errorf("pisdk: marshal: %w", err)
	}
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("pisdk: stdin closed")
	}
	if _, err := stdin.Write(line); err != nil {
		wrapped := fmt.Errorf("pisdk: write to pi: %w%s", err, c.stderrSuffix())
		c.mu.Lock()
		if c.exitErr == nil {
			c.exitErr = wrapped
		}
		c.mu.Unlock()
		return wrapped
	}
	return nil
}

// Close shuts pi down: closing stdin is its graceful signal, and the kill is
// the backstop for a process that ignores it. Safe to call more than once.
func (c *Client) Close() error {
	if c.cmd == nil {
		return nil
	}

	c.mu.Lock()
	stdin := c.stdin
	c.stdin = nil
	c.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}

	// reap owns cmd.Wait; waiting on its channel keeps that single-caller.
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		select {
		case <-c.exited:
		case <-time.After(2 * time.Second):
		}
	}

	// Let the dispatcher finish delivering what was already read, so a caller
	// that inspects state after Close sees the full stream.
	select {
	case <-c.drained:
	case <-time.After(2 * time.Second):
	}

	return c.waitErr
}

// Stderr returns the tail of pi's stderr, for error messages.
func (c *Client) Stderr() string { return c.stderrTail.String() }

func (c *Client) stderrSuffix() string {
	tail := c.stderrTail.String()
	if tail == "" {
		return ""
	}
	return " (stderr: " + tail + ")"
}

// ---------------------------------------------------------------------------
// Typed command helpers
// ---------------------------------------------------------------------------

// GetState fetches session state. Used as the handshake: a successful reply
// proves the JSONL loop is up and reveals the resolved model, session id and
// context window BEFORE any tokens are spent.
func (c *Client) GetState(ctx context.Context) (SessionState, error) {
	resp, err := c.Send(ctx, Command{Type: CmdGetState})
	if err != nil {
		return SessionState{}, err
	}
	var out SessionState
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return SessionState{}, fmt.Errorf("pisdk: decode get_state: %w", err)
	}
	return out, nil
}

// SetAutoRetry turns pi's own retry loop on or off.
//
// Hosts with their own retry policy should disable it: pi's attempts are
// invisible to an outer classifier, and only the final attempt's transcript
// survives in agent_end, so the discarded attempts are billed but absent from
// any accounting derived from it.
//
// Note the side effect — pi persists this to its settings file, so a client
// that flips it is changing the operator's configuration, not just this
// session. Pin PI_CODING_AGENT_DIR to keep it scoped.
func (c *Client) SetAutoRetry(ctx context.Context, enabled bool) error {
	_, err := c.Send(ctx, Command{Type: CmdSetAutoRetry, Enabled: boolPtr(enabled)})
	return err
}

// Prompt submits a turn. It returns as soon as pi ACCEPTS the prompt —
// completion is the agent_settled event, not this reply.
func (c *Client) Prompt(ctx context.Context, message string) error {
	_, err := c.Send(ctx, Command{Type: CmdPrompt, Message: message})
	return err
}

// Steer injects a message the agent picks up at its next turn boundary.
func (c *Client) Steer(ctx context.Context, message string) error {
	_, err := c.Send(ctx, Command{Type: CmdSteer, Message: message})
	return err
}

// Abort cancels the current operation.
func (c *Client) Abort(ctx context.Context) error {
	_, err := c.Send(ctx, Command{Type: CmdAbort})
	return err
}

// SessionStats fetches the authoritative token/cost accounting.
func (c *Client) SessionStats(ctx context.Context) (SessionStats, error) {
	resp, err := c.Send(ctx, Command{Type: CmdGetSessionStats})
	if err != nil {
		return SessionStats{}, err
	}
	var out SessionStats
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return SessionStats{}, fmt.Errorf("pisdk: decode get_session_stats: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------

// ringBuffer keeps the last n bytes of a line stream.
type ringBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
	size  int
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (b *ringBuffer) add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	b.size += len(line) + 1
	for b.size > b.max && len(b.lines) > 1 {
		b.size -= len(b.lines[0]) + 1
		b.lines = b.lines[1:]
	}
}

func (b *ringBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := ""
	for i, l := range b.lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
