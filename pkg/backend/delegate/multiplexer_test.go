package delegate

import (
	"context"
	"encoding/json"
	"errors"
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
		OnEvent: func(eventType string, _ map[string]interface{}) {
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

		evEnv, _ := NewEventEnvelope("tool_called", map[string]interface{}{"name": "Bash"})
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
