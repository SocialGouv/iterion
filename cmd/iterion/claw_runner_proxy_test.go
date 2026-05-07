package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// TestProxyDispatcher_RoundTripsToolCall is the V2-2 acceptance-level
// test. It proves the runner-side proxy infrastructure round-trips a
// tool execution across the IPC: makeProxyToolDefs → callTool →
// EnvelopeToolCall → matching EnvelopeToolResult → Output returned to
// the LLM loop's caller.
//
// The "launcher" half is simulated in-process: it reads tool_call
// envelopes from the runner's stdout, dispatches via a canned handler,
// and replies with tool_result envelopes correlated by ID.
func TestProxyDispatcher_RoundTripsToolCall(t *testing.T) {
	// stdin into the runner / stdout from the runner.
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()

	dispatcher := newProxyDispatcher(runnerStdinR, runnerStdoutW)
	dispatcher.start()

	// Build proxy ToolDefs from a single IOToolDef.
	proxies := makeProxyToolDefs([]delegate.IOToolDef{
		{Name: "Bash", Description: "run shell", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}, dispatcher)
	if len(proxies) != 1 {
		t.Fatalf("len(proxies) = %d, want 1", len(proxies))
	}

	// Spawn the launcher half: read tool_call envelopes, reply with
	// tool_result envelopes carrying canned output for each ID.
	launcherDone := make(chan error, 1)
	var calledMu sync.Mutex
	called := []string{}
	go func() {
		reader := delegate.NewEnvelopeReader(runnerStdoutR)
		writer := delegate.NewEnvelopeWriter(runnerStdinW)
		for {
			env, err := reader.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					launcherDone <- nil
				} else {
					launcherDone <- err
				}
				return
			}
			if env.Type != delegate.EnvelopeToolCall {
				continue
			}
			var call delegate.ToolCallData
			if err := json.Unmarshal(env.Data, &call); err != nil {
				launcherDone <- err
				return
			}
			calledMu.Lock()
			called = append(called, call.Name+":"+string(call.Input))
			calledMu.Unlock()
			reply, err := delegate.NewToolResultEnvelope(env.ID, "result-of-"+call.Name, "")
			if err != nil {
				launcherDone <- err
				return
			}
			if err := writer.Write(reply); err != nil {
				launcherDone <- err
				return
			}
		}
	}()

	// Drive the proxy ToolDef Execute (simulating the LLM loop calling
	// the tool). Three concurrent invocations to exercise the
	// per-correlation-ID routing under load.
	const N = 3
	results := make([]string, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			input := json.RawMessage(`{"command":"echo ` + string(rune('a'+idx)) + `"}`)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := proxies[0].Execute(ctx, input)
			results[idx] = out
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d returned error: %v", i, err)
		}
		if results[i] != "result-of-Bash" {
			t.Errorf("call %d output = %q, want result-of-Bash", i, results[i])
		}
	}

	calledMu.Lock()
	defer calledMu.Unlock()
	if len(called) != N {
		t.Errorf("launcher saw %d tool calls, want %d", len(called), N)
	}

	// Clean shutdown: closing stdin to runner triggers EOF on the
	// reader goroutine, which closes the dispatcher's done channel.
	_ = runnerStdoutW.Close()
	_ = runnerStdinW.Close()
	if err := <-launcherDone; err != nil {
		t.Errorf("launcher half: %v", err)
	}
}

// TestProxyDispatcher_SurfacesLauncherErrorAsToolError verifies that
// a tool_result envelope carrying an Error string surfaces back as a
// Go error inside the proxy ToolDef closure (which is what the LLM
// loop expects when a tool fails — the message is fed into the LLM
// as a tool_result content block).
func TestProxyDispatcher_SurfacesLauncherErrorAsToolError(t *testing.T) {
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()

	dispatcher := newProxyDispatcher(runnerStdinR, runnerStdoutW)
	dispatcher.start()

	proxies := makeProxyToolDefs([]delegate.IOToolDef{{Name: "Bash"}}, dispatcher)

	go func() {
		reader := delegate.NewEnvelopeReader(runnerStdoutR)
		writer := delegate.NewEnvelopeWriter(runnerStdinW)
		env, _ := reader.Read()
		reply, _ := delegate.NewToolResultEnvelope(env.ID, "", "permission denied")
		_ = writer.Write(reply)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := proxies[0].Execute(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error from launcher-side tool failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should surface launcher message, got: %v", err)
	}
}

// TestProxyDispatcher_PreservesErrAskUserAcrossWire is the V2-3
// acceptance test. It proves a launcher-side ToolDef returning
// *delegate.ErrAskUser round-trips back to the runner as a typed
// *delegate.ErrAskUser (preserving Question, PendingToolUseID, and
// Conversation), so the LLM loop's existing pause/resume path
// triggers identically inside and outside the sandbox.
func TestProxyDispatcher_PreservesErrAskUserAcrossWire(t *testing.T) {
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()

	dispatcher := newProxyDispatcher(runnerStdinR, runnerStdoutW)
	dispatcher.start()

	proxies := makeProxyToolDefs([]delegate.IOToolDef{{Name: "ask_user"}}, dispatcher)

	// "Launcher" half: drives the multiplexer with an OnToolCall that
	// returns a typed *ErrAskUser, exactly as the engine's real
	// ask_user ToolDef does.
	mux := delegate.NewMultiplexer(runnerStdoutR, runnerStdinW, delegate.MultiplexerHandler{
		OnToolCall: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return "", &delegate.ErrAskUser{
				Question:         "approve commit?",
				PendingToolUseID: "tu_42",
				Conversation:     json.RawMessage(`{"messages":[{"role":"user"}]}`),
			}
		},
	})
	muxDone := make(chan error, 1)
	go func() {
		// Send a no-op task envelope so the multiplexer's Run() can
		// proceed; the test never actually reaches a result envelope
		// because the proxy returns ErrAskUser and we exit early.
		muxDone <- nil
		_, _ = mux.Run(context.Background())
	}()
	<-muxDone

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, execErr := proxies[0].Execute(ctx, json.RawMessage(`{"question":"approve commit?"}`))
	if execErr == nil {
		t.Fatal("expected ErrAskUser from launcher")
	}
	var askErr *delegate.ErrAskUser
	if !errors.As(execErr, &askErr) {
		t.Fatalf("expected *delegate.ErrAskUser, got %T (%v)", execErr, execErr)
	}
	if askErr.Question != "approve commit?" {
		t.Errorf("Question = %q, want 'approve commit?'", askErr.Question)
	}
	if askErr.PendingToolUseID != "tu_42" {
		t.Errorf("PendingToolUseID = %q, want tu_42", askErr.PendingToolUseID)
	}
	if !strings.Contains(string(askErr.Conversation), "messages") {
		t.Errorf("Conversation should round-trip; got %s", askErr.Conversation)
	}
}

// TestProxyDispatcher_FailsAllPendingOnReaderEOF unblocks waiting
// proxy calls when the launcher closes stdin unexpectedly, so the LLM
// loop returns a useful error instead of hanging forever.
func TestProxyDispatcher_FailsAllPendingOnReaderEOF(t *testing.T) {
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()
	defer runnerStdoutR.Close()
	defer runnerStdoutW.Close()

	dispatcher := newProxyDispatcher(runnerStdinR, runnerStdoutW)
	dispatcher.start()

	proxies := makeProxyToolDefs([]delegate.IOToolDef{{Name: "Bash"}}, dispatcher)

	// Close the runner's stdin from the launcher side immediately.
	// The reader goroutine sees EOF and propagates the failure.
	go func() {
		// Drain the tool_call envelope so the writer goroutine in
		// dispatcher.call doesn't block on a full pipe.
		reader := delegate.NewEnvelopeReader(runnerStdoutR)
		_, _ = reader.Read()
		_ = runnerStdinW.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := proxies[0].Execute(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when launcher closes channel mid-call")
	}
	if !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "EOF") {
		t.Errorf("error should mention channel closure, got: %v", err)
	}
}
