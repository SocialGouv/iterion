package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMultiplexer_ToolCallRoundTrip is the V2-1 acceptance test. It
// proves the launcher-side multiplexer can:
//  1. Send a task envelope to the runner.
//  2. Receive a tool_call envelope from the runner mid-stream.
//  3. Dispatch via OnToolCall and reply with a correlated tool_result.
//  4. Receive a terminal result envelope and surface it to the caller.
//
// The two halves of the IPC are wired with io.Pipe so the test runs
// in-process without spawning a real runner.
func TestMultiplexer_ToolCallRoundTrip(t *testing.T) {
	// Launcher writes to runnerStdin, runner reads from it. Runner
	// writes to runnerStdout, launcher reads from it. Two pipes form
	// the bidirectional NDJSON channel.
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()

	// Track which tools the launcher's handler was asked to execute,
	// so the assertion can verify both the wire round-trip and the
	// payload integrity.
	type handled struct {
		name  string
		input json.RawMessage
	}
	var (
		mu       sync.Mutex
		dispatch []handled
	)

	handler := MultiplexerHandler{
		OnToolCall: func(_ context.Context, name string, input json.RawMessage) (string, error) {
			mu.Lock()
			dispatch = append(dispatch, handled{name: name, input: input})
			mu.Unlock()
			// Echo back a deterministic answer the runner-side will
			// validate. Real V2-2 implementation will dispatch via the
			// in-process tool registry / MCP manager.
			return "ok:" + name, nil
		},
	}

	mux := NewMultiplexer(runnerStdoutR, runnerStdinW, handler)

	// Spawn the "runner" — reads task, emits tool_call, waits for
	// tool_result on its stdin, validates payload, emits result.
	runnerErr := make(chan error, 1)
	go func() {
		runnerErr <- runRunnerHalf(runnerStdinR, runnerStdoutW)
	}()

	// Send the task envelope (the launcher's seed action).
	taskEnv, err := NewTaskEnvelope(IOTask{NodeID: "node-1", Model: "anthropic/claude-sonnet-4-6"})
	if err != nil {
		t.Fatalf("build task envelope: %v", err)
	}
	if err := mux.Send(taskEnv); err != nil {
		t.Fatalf("send task envelope: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := mux.Run(ctx)
	if err != nil {
		t.Fatalf("multiplexer run: %v", err)
	}
	if got := <-runnerErr; got != nil {
		t.Fatalf("runner half: %v", got)
	}

	// Final result must be the one the runner emitted.
	if result.BackendName != "claw" {
		t.Errorf("BackendName = %q, want claw", result.BackendName)
	}
	if result.Tokens != 42 {
		t.Errorf("Tokens = %d, want 42", result.Tokens)
	}

	// And the launcher must have dispatched exactly one tool_call.
	mu.Lock()
	defer mu.Unlock()
	if len(dispatch) != 1 {
		t.Fatalf("dispatch len = %d, want 1", len(dispatch))
	}
	if dispatch[0].name != "Bash" {
		t.Errorf("dispatched tool = %q, want Bash", dispatch[0].name)
	}
	if !strings.Contains(string(dispatch[0].input), `"command":"echo hi"`) {
		t.Errorf("dispatched input = %s, missing expected command", dispatch[0].input)
	}
}

// runRunnerHalf simulates an in-sandbox runner using the V2-1 envelope
// wire format: it consumes the task envelope, emits one tool_call,
// waits for the correlated tool_result, validates the launcher's
// reply, then emits a terminal result envelope.
func runRunnerHalf(stdin io.Reader, stdout io.WriteCloser) error {
	defer stdout.Close()

	reader := NewEnvelopeReader(stdin)
	writer := NewEnvelopeWriter(stdout)

	// 1. Read the task envelope.
	env, err := reader.Read()
	if err != nil {
		return err
	}
	if env.Type != EnvelopeTask {
		return errors.New("runner: expected task envelope first")
	}

	// 2. Emit a tool_call.
	callEnv, err := NewToolCallEnvelope("call-1", "Bash", json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		return err
	}
	if err := writer.Write(callEnv); err != nil {
		return err
	}

	// 3. Wait for the correlated tool_result.
	reply, err := reader.Read()
	if err != nil {
		return err
	}
	if reply.Type != EnvelopeToolResult {
		return errors.New("runner: expected tool_result")
	}
	if reply.ID != "call-1" {
		return errors.New("runner: tool_result id mismatch")
	}
	var toolRes ToolResultData
	if err := json.Unmarshal(reply.Data, &toolRes); err != nil {
		return err
	}
	if toolRes.Output != "ok:Bash" {
		return errors.New("runner: tool_result output mismatch: " + toolRes.Output)
	}

	// 4. Emit terminal result envelope.
	resEnv, err := NewResultEnvelope(IOResult{BackendName: "claw", Tokens: 42})
	if err != nil {
		return err
	}
	return writer.Write(resEnv)
}

// TestMultiplexer_NilHandlerSurfacesErrorToRunner exercises the
// fallback path where the launcher has no OnToolCall hook (V2-1
// default): the multiplexer must reply with a tool_result envelope
// carrying an Error string so the runner-side proxy ToolDef can
// surface it as a Go error rather than block forever.
func TestMultiplexer_NilHandlerSurfacesErrorToRunner(t *testing.T) {
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()

	mux := NewMultiplexer(runnerStdoutR, runnerStdinW, MultiplexerHandler{})

	gotErrMsg := make(chan string, 1)
	go func() {
		// Runner half: emit a tool_call, capture the error in the
		// reply, then emit the result envelope so the multiplexer
		// terminates cleanly.
		defer runnerStdoutW.Close()
		reader := NewEnvelopeReader(runnerStdinR)
		writer := NewEnvelopeWriter(runnerStdoutW)
		_, _ = reader.Read() // consume task envelope
		callEnv, _ := NewToolCallEnvelope("c1", "Bash", nil)
		_ = writer.Write(callEnv)
		reply, _ := reader.Read()
		var data ToolResultData
		_ = json.Unmarshal(reply.Data, &data)
		gotErrMsg <- data.Error
		resEnv, _ := NewResultEnvelope(IOResult{})
		_ = writer.Write(resEnv)
	}()

	taskEnv, _ := NewTaskEnvelope(IOTask{NodeID: "n"})
	_ = mux.Send(taskEnv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := mux.Run(ctx); err != nil {
		t.Fatalf("multiplexer: %v", err)
	}

	got := <-gotErrMsg
	if !strings.Contains(got, "no tool dispatcher") {
		t.Errorf("expected dispatcher-missing error, got %q", got)
	}
}

// TestMultiplexer_PassesEventsAndSessionCapture verifies the
// fire-and-forget hooks for events + session_capture deliver runner
// payloads to the launcher without blocking the multiplexer loop.
func TestMultiplexer_PassesEventsAndSessionCapture(t *testing.T) {
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()

	var (
		mu         sync.Mutex
		eventLog   []string
		captureLog []string
	)
	handler := MultiplexerHandler{
		OnEvent: func(eventType string, _ map[string]any) {
			mu.Lock()
			eventLog = append(eventLog, eventType)
			mu.Unlock()
		},
		OnSessionCapture: func(snap json.RawMessage) {
			mu.Lock()
			captureLog = append(captureLog, string(snap))
			mu.Unlock()
		},
	}
	mux := NewMultiplexer(runnerStdoutR, runnerStdinW, handler)

	go func() {
		defer runnerStdoutW.Close()
		reader := NewEnvelopeReader(runnerStdinR)
		writer := NewEnvelopeWriter(runnerStdoutW)
		_, _ = reader.Read() // consume task

		evEnv, _ := NewEventEnvelope("tool_called", map[string]any{"name": "Bash"})
		_ = writer.Write(evEnv)

		_ = writer.Write(NewSessionCaptureEnvelope(json.RawMessage(`{"messages":[{"role":"user"}]}`)))

		resEnv, _ := NewResultEnvelope(IOResult{Tokens: 7})
		_ = writer.Write(resEnv)
	}()

	taskEnv, _ := NewTaskEnvelope(IOTask{NodeID: "n"})
	_ = mux.Send(taskEnv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := mux.Run(ctx)
	if err != nil {
		t.Fatalf("multiplexer: %v", err)
	}
	if res.Tokens != 7 {
		t.Errorf("result.Tokens = %d, want 7", res.Tokens)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(eventLog) != 1 || eventLog[0] != "tool_called" {
		t.Errorf("events = %v, want [tool_called]", eventLog)
	}
	if len(captureLog) != 1 || !strings.Contains(captureLog[0], "messages") {
		t.Errorf("session_capture = %v, want a single snapshot", captureLog)
	}
}

type relayObservedWriter struct {
	*io.PipeWriter
	started chan struct{}
}

func (w relayObservedWriter) Write(b []byte) (int, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
	}
	return w.PipeWriter.Write(b)
}

// relayPeer drives the real wire from a runner with no implicit draining of
// replies. Its deadline also releases both pipe directions on a failed check.
type relayPeer struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	send    *EnvelopeWriter
	recv    *EnvelopeReader
	stdout  *io.PipeWriter
	stdin   *io.PipeReader
	done    chan relayOutcome
	writing chan struct{}
}

type relayOutcome struct {
	result IOResult
	err    error
}

func startRelayPeer(t *testing.T, handler MultiplexerHandler, options ...func(*Multiplexer)) *relayPeer {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	deadline, expire := context.WithTimeout(context.Background(), 3*time.Second)
	ctx, cancel := context.WithCancel(deadline)
	// The watchdog, not explicit relay cancellation, can rescue a blocked
	// test. Teardown tests must observe the relay closing its own ends.
	stop := context.AfterFunc(deadline, func() {
		_ = stdinR.CloseWithError(deadline.Err())
		_ = stdoutW.CloseWithError(deadline.Err())
	})
	t.Cleanup(func() {
		stop()
		cancel()
		expire()
		_ = stdinR.Close()
		_ = stdoutW.Close()
	})
	p := &relayPeer{
		t: t, ctx: ctx, cancel: cancel, send: NewEnvelopeWriter(stdoutW), recv: NewEnvelopeReader(stdinR),
		stdout: stdoutW, stdin: stdinR, done: make(chan relayOutcome, 1), writing: make(chan struct{}),
	}
	go func() {
		mux := NewMultiplexer(stdoutR, relayObservedWriter{stdinW, p.writing}, handler)
		for _, option := range options {
			option(mux)
		}
		result, err := mux.Run(ctx)
		p.done <- relayOutcome{result, err}
	}()
	return p
}

func (p *relayPeer) write(env Envelope, err error) {
	p.t.Helper()
	if err != nil {
		p.t.Fatal(err)
	}
	if err := p.send.Write(env); err != nil {
		p.t.Fatal(err)
	}
}

func (p *relayPeer) read(kind EnvelopeType, id string, data any) {
	p.t.Helper()
	env, err := p.recv.Read()
	if err != nil {
		p.t.Fatal(err)
	}
	if env.Type != kind || env.ID != id {
		p.t.Fatalf("got %s:%s, want %s:%s", env.Type, env.ID, kind, id)
	}
	if err := json.Unmarshal(env.Data, data); err != nil {
		p.t.Fatal(err)
	}
}

func relayAwait[T any](t *testing.T, ctx context.Context, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", what, ctx.Err())
		var zero T
		return zero
	}
}

func TestMultiplexer_BlockedToolStillRelaysEventsAndCapturesInOrder(t *testing.T) {
	started := make(chan string, 4)
	observed := make(chan string, 4)
	release := make(chan struct{})
	p := startRelayPeer(t, MultiplexerHandler{
		OnToolCall: func(ctx context.Context, name string, _ json.RawMessage) (string, error) {
			started <- name
			if name == "first" {
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return name, nil
		},
		OnAskUser: func(context.Context, AskUserData) (map[string]string, error) {
			started <- "ask"
			return map[string]string{"choose": "yes"}, nil
		},
		OnEvent:          func(kind string, _ map[string]any) { observed <- kind },
		OnSessionCapture: func(raw json.RawMessage) { observed <- string(raw) },
	})
	p.write(NewToolCallEnvelope("one", "first", nil))
	if got := relayAwait(t, p.ctx, started, "first tool start"); got != "first" {
		t.Fatal(got)
	}
	p.write(NewAskUserEnvelope("human", AskUserData{}))
	// An untrusted runner cannot opt a forwarded tool into parallel execution.
	p.write(Envelope{Type: EnvelopeToolCall, ID: "last", Data: json.RawMessage(`{"name":"last","parallel":true}`)}, nil)
	p.write(NewEventEnvelope("before", nil))
	p.write(NewSessionCaptureEnvelope(json.RawMessage(`{"capture":true}`)), nil)
	p.write(NewEventEnvelope("after", nil))
	for _, want := range []string{"before", `{"capture":true}`, "after"} {
		if got := relayAwait(t, p.ctx, observed, want); got != want {
			t.Fatalf("wire order: got %s, want %s", got, want)
		}
	}
	select {
	case got := <-started:
		t.Fatalf("%s overlapped the first tool", got)
	default:
	}
	close(release)
	var tool ToolResultData
	p.read(EnvelopeToolResult, "one", &tool)
	if tool.Output != "first" {
		t.Fatalf("first result: %+v", tool)
	}
	var answer AskUserAnswerData
	p.read(EnvelopeAskUserAnswer, "human", &answer)
	if answer.Answers["choose"] != "yes" {
		t.Fatalf("answer: %+v", answer)
	}
	p.read(EnvelopeToolResult, "last", &tool)
	if tool.Output != "last" {
		t.Fatalf("last result: %+v", tool)
	}
	for _, want := range []string{"ask", "last"} {
		if got := relayAwait(t, p.ctx, started, want); got != want {
			t.Fatalf("execution order: got %s, want %s", got, want)
		}
	}
	p.write(NewResultEnvelope(IOResult{Tokens: 7}))
	got := relayAwait(t, p.ctx, p.done, "terminal result")
	if got.err != nil || got.result.Tokens != 7 {
		t.Fatalf("result: %+v", got)
	}
}

func TestMultiplexer_SaturationReturnsCorrelatedToolAndAskRefusals(t *testing.T) {
	started := make(chan string, multiplexerPendingCalls+3)
	stopped := make(chan struct{})
	p := startRelayPeer(t, MultiplexerHandler{
		OnToolCall: func(ctx context.Context, name string, _ json.RawMessage) (string, error) {
			started <- name
			if name == "blocked" {
				<-ctx.Done()
				close(stopped)
			}
			return "", ctx.Err()
		},
		OnAskUser: func(context.Context, AskUserData) (map[string]string, error) {
			started <- "ask"
			return nil, nil
		},
	})
	p.write(NewToolCallEnvelope("active", "blocked", nil))
	relayAwait(t, p.ctx, started, "blocked tool start")
	for i := range multiplexerPendingCalls {
		p.write(NewToolCallEnvelope(fmt.Sprint(i), "queued", nil))
	}
	p.write(NewToolCallEnvelope("overflow-tool", "rejected", nil))
	p.write(NewAskUserEnvelope("overflow-ask", AskUserData{}))
	var tool ToolResultData
	p.read(EnvelopeToolResult, "overflow-tool", &tool)
	if tool.Error != multiplexerBusy {
		t.Fatalf("tool refusal: %+v", tool)
	}
	var ask AskUserAnswerData
	p.read(EnvelopeAskUserAnswer, "overflow-ask", &ask)
	if ask.Answers[AskUserErrorKey] != multiplexerBusy {
		t.Fatalf("Ask refusal: %+v", ask)
	}
	p.write(NewResultEnvelope(IOResult{}))
	p.read(EnvelopeToolResult, "active", &tool)
	if got := relayAwait(t, p.ctx, p.done, "relay exit"); got.err == nil || !strings.Contains(got.err.Error(), "unanswered requests") {
		t.Fatalf("premature terminal must not drop queued requests and succeed: %+v", got)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("terminal result returned before the active handler stopped")
	}
	select {
	case got := <-started:
		t.Fatalf("dispatched %s during shutdown", got)
	default:
	}
}

func TestMultiplexer_StalledReplyPipeAbortsOnBacklogLimit(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	p := startRelayPeer(t, MultiplexerHandler{OnToolCall: func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
		close(started)
		defer close(stopped)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	p.write(NewToolCallEnvelope("active", "blocked", nil))
	relayAwait(t, p.ctx, started, "blocked tool start")
	for i := range multiplexerPendingCalls {
		p.write(NewToolCallEnvelope(fmt.Sprint(i), "queued", nil))
	}
	// Park one refusal in the writer, then fill the reply queue behind it.
	p.write(NewToolCallEnvelope("parked", "rejected", nil))
	relayAwait(t, p.ctx, p.writing, "parked reply write")
	for i := range multiplexerPendingReplies + 1 {
		p.write(NewToolCallEnvelope(fmt.Sprintf("overflow-%d", i), "rejected", nil))
	}
	got := relayAwait(t, p.ctx, p.done, "reply-backlog failure")
	if got.err == nil || !strings.Contains(got.err.Error(), "reply backlog full") {
		t.Fatalf("outcome: %+v", got)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("relay returned before the active handler stopped")
	}
}

func TestMultiplexer_TeardownJoinsHandler(t *testing.T) {
	for _, kind := range []string{"EOF", "writer failure", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			started := make(chan struct{})
			stopped := make(chan struct{})
			p := startRelayPeer(t, MultiplexerHandler{OnToolCall: func(ctx context.Context, name string, _ json.RawMessage) (string, error) {
				if name == "blocked" {
					close(started)
					defer close(stopped)
					<-ctx.Done()
					return "", ctx.Err()
				}
				return "early", nil
			}})
			sentinel := errors.New("runner stopped reading its stdin")
			if kind == "writer failure" {
				// Keep the worker occupied on the next call while a saturation
				// reply deterministically hits the broken write direction.
				_ = p.stdin.CloseWithError(sentinel)
			}
			p.write(NewToolCallEnvelope("active", "blocked", nil))
			relayAwait(t, p.ctx, started, "blocked tool start")
			var want error
			switch kind {
			case "EOF":
				_ = p.stdout.Close()
				want = io.EOF
			case "writer failure":
				for i := range multiplexerPendingCalls + 1 {
					p.write(NewToolCallEnvelope(fmt.Sprint(i), "queued", nil))
				}
				want = sentinel
			case "cancel":
				p.cancel()
				want = context.Canceled
			}
			select {
			case got := <-p.done:
				if !errors.Is(got.err, want) {
					t.Fatalf("got %v, want %v", got.err, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("relay did not shut down")
			}
			select {
			case <-stopped:
			default:
				t.Fatal("relay returned before the active handler stopped")
			}
		})
	}
}

func TestMultiplexer_TerminalResultCannotDiscardParkedReply(t *testing.T) {
	p := startRelayPeer(t, MultiplexerHandler{OnToolCall: func(context.Context, string, json.RawMessage) (string, error) {
		return "unread", nil
	}}, func(m *Multiplexer) { m.SetReplyDrainTimeout(50 * time.Millisecond) })
	p.write(NewToolCallEnvelope("parked", "tool", nil))
	relayAwait(t, p.ctx, p.writing, "parked reply write")
	p.write(NewResultEnvelope(IOResult{Tokens: 7}))
	got := relayAwait(t, p.ctx, p.done, "terminal result with parked reply")
	if got.err == nil || !strings.Contains(got.err.Error(), "still pending at the relay drain deadline") {
		t.Fatalf("premature terminal must not discard the parked reply and succeed: %+v", got)
	}
}
