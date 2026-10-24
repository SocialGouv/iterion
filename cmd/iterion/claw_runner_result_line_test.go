package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// An oversized business answer still crosses as a small terminal result: its
// spend survives, while the text does not. The runner returns the original
// typed line refusal so the launcher books the result and fails the node.
func TestClawRunner_OversizedTextResultEmitsMeteredFallback(t *testing.T) {
	answer := strings.Repeat("x", delegate.MaxEnvelopeLineBytes+300_000)
	serveOpenAIChunk(t, map[string]any{"content": answer}, "stop")

	got := collectOversizedResult(t, nil, delegate.IOTask{
		NodeID: "copi", SessionSlot: "assistant_conversation",
		Model: "openai/gpt-5", UserPrompt: "hello",
	})
	assertOversizedResultFallback(t, got)
	if got.result.Tokens != 1500 {
		t.Errorf("Tokens = %d, want the billed 1500", got.result.Tokens)
	}
	if got.result.BackendName != delegate.BackendClaw {
		t.Errorf("BackendName = %q, want %q", got.result.BackendName, delegate.BackendClaw)
	}
	if len(got.result.Output) != 3 {
		t.Fatalf("fallback Output = %#v, want only _tokens/_model/_cost_usd", got.result.Output)
	}
	if got.result.Output["_tokens"] != float64(1500) {
		t.Errorf("_tokens = %#v, want 1500", got.result.Output["_tokens"])
	}
	if got.result.Output["_model"] != "openai/gpt-5" {
		t.Errorf("_model = %#v, want openai/gpt-5", got.result.Output["_model"])
	}
	if usd, ok := got.result.Output["_cost_usd"].(float64); !ok || usd <= 0 {
		t.Errorf("_cost_usd = %#v, want the positive priced spend", got.result.Output["_cost_usd"])
	}
}

// A pause whose saved conversation is over the cap cannot be resumed from a
// truncated snapshot. Its fallback therefore removes both the pause markers
// and the resume state, reports the refusal, and keeps the terminal account.
func TestClawRunner_OversizedPendingConversationEmitsNonResumableFallback(t *testing.T) {
	answer := strings.Repeat("x", delegate.MaxEnvelopeLineBytes+300_000)
	serveOpenAIChunk(t, map[string]any{
		"role":    "assistant",
		"content": answer,
		"tool_calls": []map[string]any{{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": `{"command":"pwd"}`},
		}},
	}, "tool_calls")

	got := collectOversizedResult(t, []delegate.Envelope{{
		Type: delegate.EnvelopePermissionPolicy,
		Data: json.RawMessage(`{"mode":"ask"}`),
	}}, delegate.IOTask{
		NodeID: "copi", Model: "openai/gpt-5", UserPrompt: "hello",
		HasTools: true, ToolMaxSteps: 5,
		ToolDefs: []delegate.IOToolDef{{Name: "bash", Description: "shell",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
	})
	assertOversizedResultFallback(t, got)
	if len(got.result.PendingConversation) != 0 || got.result.PendingToolUseID != "" {
		t.Errorf("fallback still carries resumable pause state: conversation=%d bytes, tool=%q",
			len(got.result.PendingConversation), got.result.PendingToolUseID)
	}
	if _, paused := got.result.Output["_needs_interaction"]; paused {
		t.Errorf("fallback without its conversation still asks the runtime to pause: %#v", got.result.Output)
	}
}

func TestOversizedResultFallbackKeepsOnlyAccountingAndScalarMetadata(t *testing.T) {
	writeErr := errors.New("the original line refusal")
	original := delegate.IOResult{
		UnreportedCalls: 2,
		CostUSD:         0.25,
		Output: map[string]any{
			"text": "business output", "_needs_interaction": true,
			"_tokens": 1500, "_cost_usd": 0.25, "_model": "openai/gpt-5", "_usage_unreported_calls": 2,
		},
		Tokens: 1500, DurationMS: 42, ExitCode: 7, Stderr: "diagnostics",
		BackendName: delegate.BackendClaw, RawOutputLen: 5000,
		ParseFallback: true, FormattingPassUsed: true,
		SessionID: "session", SessionFingerprint: "fingerprint",
		PendingConversation: json.RawMessage(`[{"role":"assistant"}]`), PendingToolUseID: "toolu_1",
	}
	want := delegate.IOResult{
		UnreportedCalls: 2,
		CostUSD:         0.25,
		Output: map[string]any{
			"_tokens": 1500, "_cost_usd": 0.25, "_model": "openai/gpt-5", "_usage_unreported_calls": 2,
		},
		Tokens: 1500, DurationMS: 42, ExitCode: 7,
		BackendName: delegate.BackendClaw, RawOutputLen: 5000,
		ParseFallback: true, FormattingPassUsed: true,
		SessionID: "session", SessionFingerprint: "fingerprint",
		Error: writeErr.Error(),
	}

	got := oversizedResultFallback(original, writeErr)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback = %#v, want %#v", got, want)
	}
	env, err := delegate.NewResultEnvelope(got)
	if err != nil {
		t.Fatalf("build the fallback envelope: %v", err)
	}
	if err := delegate.CheckEnvelopeLine(env); err != nil {
		t.Fatalf("the scalar/accounting fallback does not fit the channel: %v", err)
	}
}

// A task that exactly fills the incoming line leaves no room at all for
// unbounded scalar provenance on the fallback. The runner degrades it to a
// bounded marker rather than losing the account a second time.
func TestClawRunner_OversizedResultFallbackBoundsLargeSessionID(t *testing.T) {
	var sessionID string
	task := taskSizedToLineCap(t, delegate.MaxEnvelopeLineBytes, func(width int) delegate.IOTask {
		sessionID = strings.Repeat("s", width)
		return delegate.IOTask{
			NodeID: "copi", SessionSlot: "assistant_conversation", SessionID: sessionID,
			Model: "openai/gpt-5", UserPrompt: "hello",
		}
	})

	answer := strings.Repeat("x", delegate.MaxEnvelopeLineBytes+300_000)
	serveOpenAIChunk(t, map[string]any{"content": answer}, "stop")
	got := collectOversizedResult(t, nil, task)
	assertOversizedResultFallback(t, got)
	if got.result.Tokens != 1500 {
		t.Fatalf("the result that crossed lost the bill: Tokens=%d", got.result.Tokens)
	}
	if got.result.SessionID == sessionID || !strings.Contains(got.result.SessionID, "omitted") ||
		!strings.Contains(got.result.SessionID, fmt.Sprint(len(sessionID))) || len(got.result.SessionID) >= 128 {
		t.Errorf("SessionID was not replaced by a bounded marker naming its size: got %d bytes, original %d",
			len(got.result.SessionID), len(sessionID))
	}
	if got.result.Output["_model"] != "openai/gpt-5" {
		t.Errorf("_model = %#v, want the still-affordable accounting model", got.result.Output["_model"])
	}
}

func TestOversizedResultFallbackBoundsLargeSessionFingerprint(t *testing.T) {
	fingerprint := strings.Repeat("f", delegate.MaxEnvelopeLineBytes)
	original := delegate.IOResult{
		CostUSD: 0.01,
		Output: map[string]any{
			"text":    strings.Repeat("x", delegate.MaxEnvelopeLineBytes+1),
			"_tokens": 7, "_cost_usd": 0.01, "_model": "openai/gpt-5",
		},
		Tokens: 7, BackendName: delegate.BackendClaw, SessionFingerprint: fingerprint,
	}
	env, err := delegate.NewResultEnvelope(original)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := delegate.CheckEnvelopeLine(env)
	if !errors.Is(writeErr, delegate.ErrEnvelopeLineTooLong) {
		t.Fatalf("scenario broken: original envelope must be over the line: %v", writeErr)
	}

	got := oversizedResultFallback(original, writeErr)
	if got.SessionFingerprint == fingerprint || !strings.Contains(got.SessionFingerprint, "omitted") ||
		!strings.Contains(got.SessionFingerprint, fmt.Sprint(len(fingerprint))) || len(got.SessionFingerprint) >= 128 {
		t.Fatalf("SessionFingerprint was not replaced by a bounded marker naming its size: got %d bytes", len(got.SessionFingerprint))
	}
	fallbackEnv, err := delegate.NewResultEnvelope(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := delegate.CheckEnvelopeLine(fallbackEnv); err != nil {
		t.Fatalf("the fingerprint-bounded fallback does not fit: %v", err)
	}
	if got.Tokens != 7 || got.Output["_cost_usd"] != 0.01 {
		t.Errorf("the bounded fallback lost the account: %+v", got)
	}
}

// The same exact-line edge through the workflow's model spec: once scalar
// metadata has been bounded, _model itself degrades last, after the account.
func TestClawRunner_OversizedResultFallbackBoundsLargeModel(t *testing.T) {
	var model string
	task := taskSizedToLineCap(t, delegate.MaxEnvelopeLineBytes, func(width int) delegate.IOTask {
		model = "openai/" + strings.Repeat("m", width)
		return delegate.IOTask{NodeID: "copi", Model: model, UserPrompt: "hello"}
	})

	answer := strings.Repeat("x", delegate.MaxEnvelopeLineBytes+300_000)
	serveOpenAIChunk(t, map[string]any{"content": answer}, "stop")
	got := collectOversizedResult(t, nil, task)
	assertOversizedResultFallback(t, got)
	if got.result.Tokens != 1500 || got.result.Output["_tokens"] != float64(1500) {
		t.Fatalf("the result that crossed lost the bill: Tokens=%d Output=%#v", got.result.Tokens, got.result.Output)
	}
	gotModel, ok := got.result.Output["_model"].(string)
	if !ok || gotModel == model || !strings.Contains(gotModel, "omitted") ||
		!strings.Contains(gotModel, fmt.Sprint(len(model))) || len(gotModel) >= 128 {
		t.Errorf("_model was not replaced by a bounded marker naming its size: got %d bytes, original %d",
			len(gotModel), len(model))
	}
}

func TestEmitOversizedResultFallbackJoinsAndLogsTheBoundedBackendError(t *testing.T) {
	backendError := "provider exploded: " + strings.Repeat("e", oversizedResultBackendErrorBytes-len("provider exploded: "))
	original := delegate.IOResult{
		CostUSD: 0.01,
		Output: map[string]any{
			"text":    strings.Repeat("x", delegate.MaxEnvelopeLineBytes+1),
			"_tokens": 7, "_cost_usd": 0.01, "_model": "openai/gpt-5",
		},
		Tokens: 7, BackendName: delegate.BackendClaw, Error: backendError,
	}
	originalEnv, err := delegate.NewResultEnvelope(original)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := delegate.CheckEnvelopeLine(originalEnv)
	if !errors.Is(writeErr, delegate.ErrEnvelopeLineTooLong) {
		t.Fatalf("scenario broken: original envelope must be over the line: %v", writeErr)
	}

	var stdout, stderr bytes.Buffer
	dispatcher := newProxyDispatcher(strings.NewReader(""), &stdout)
	emitOversizedResultFallback(dispatcher, &stderr, original, writeErr)

	env, readErr := delegate.NewEnvelopeReader(&stdout).Read()
	if readErr != nil {
		t.Fatalf("read the bounded fallback: %v", readErr)
	}
	var got delegate.IOResult
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("decode the bounded fallback: %v", err)
	}
	if !strings.HasPrefix(got.Error, writeErr.Error()+"\n") {
		t.Errorf("the channel refusal is not the primary error: %q", got.Error)
	}
	if !strings.Contains(got.Error, backendError) {
		t.Errorf("the in-bound backend error is not preserved as the joined suffix: %q", got.Error)
	}
	if len(got.Error) > len(writeErr.Error())+oversizedResultBackendErrorBytes+64 {
		t.Errorf("joined error is %d bytes, beyond its bound", len(got.Error))
	}
	if log := stderr.String(); !strings.Contains(log, backendError) {
		t.Errorf("the in-bound backend error was not logged: %q", log)
	}
}

func TestEmitOversizedResultFallbackOmitsAnOverboundBackendError(t *testing.T) {
	secret := "correct horse battery staple q9f8e7d6c5b4a39281706f5e4d3c2b1a"
	leakedPrefix := secret[:32]
	backendError := strings.Repeat("a", oversizedResultBackendErrorBytes-len(leakedPrefix)) +
		secret + strings.Repeat("b", 512)
	original := delegate.IOResult{
		CostUSD: 0.01,
		Output: map[string]any{
			"text":    strings.Repeat("x", delegate.MaxEnvelopeLineBytes+1),
			"_tokens": 7, "_cost_usd": 0.01, "_model": "openai/gpt-5",
		},
		Tokens: 7, BackendName: delegate.BackendClaw, Error: backendError,
	}
	originalEnv, err := delegate.NewResultEnvelope(original)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := delegate.CheckEnvelopeLine(originalEnv)
	if !errors.Is(writeErr, delegate.ErrEnvelopeLineTooLong) {
		t.Fatalf("scenario broken: original envelope must be over the line: %v", writeErr)
	}

	var stdout, stderr bytes.Buffer
	dispatcher := newProxyDispatcher(strings.NewReader(""), &stdout)
	emitOversizedResultFallback(dispatcher, &stderr, original, writeErr)
	env, readErr := delegate.NewEnvelopeReader(&stdout).Read()
	if readErr != nil {
		t.Fatalf("read the bounded fallback: %v", readErr)
	}
	var got delegate.IOResult
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("decode the bounded fallback: %v", err)
	}

	marker := omittedOversizedResultField("backend error", len(backendError))
	if !strings.Contains(got.Error, marker) || !strings.Contains(stderr.String(), marker) {
		t.Errorf("the over-bound backend error did not become the bounded omission marker: result=%q stderr=%q",
			got.Error, stderr.String())
	}
	if strings.Contains(got.Error+stderr.String(), leakedPrefix) {
		t.Fatalf("the fallback leaked the %d-byte secret prefix %q", len(leakedPrefix), leakedPrefix)
	}
	if got.Tokens != 7 || got.Output["_cost_usd"] != 0.01 {
		t.Errorf("the omission marker lost the account: %+v", got)
	}
}

func taskSizedToLineCap(t *testing.T, initialWidth int, makeTask func(int) delegate.IOTask) delegate.IOTask {
	t.Helper()
	width := initialWidth
	for {
		if width < 0 {
			t.Fatal("the task cannot be resized down to the envelope line cap")
		}
		task := makeTask(width)
		env, err := delegate.NewTaskEnvelope(task)
		if err != nil {
			t.Fatal(err)
		}
		buf, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		line := len(buf) + 1
		if line == delegate.MaxEnvelopeLineBytes {
			if err := delegate.CheckEnvelopeLine(env); err != nil {
				t.Fatalf("the exact-size task does not fit: %v", err)
			}
			return task
		}
		width += delegate.MaxEnvelopeLineBytes - line
	}
}

type oversizedResultOutcome struct {
	envelope  delegate.Envelope
	result    delegate.IOResult
	sawResult bool
	readErr   error
	runErr    error
	stderr    string
}

func serveOpenAIChunk(t *testing.T, delta map[string]any, finishReason string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk, err := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"index": 0, "delta": delta, "finish_reason": finishReason,
			}},
			"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 300},
		})
		if err != nil {
			t.Errorf("marshal the provider's chunk: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
}

func collectOversizedResult(t *testing.T, preTask []delegate.Envelope, task delegate.IOTask) oversizedResultOutcome {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &lockedBuffer{}

	var (
		runErr error
		wg     sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = runClawRunner(context.Background(), stdinR, stdoutW, stderr)
		_ = stdoutW.Close()
	}()

	go func() {
		writer := delegate.NewEnvelopeWriter(stdinW)
		for _, env := range preTask {
			if err := writer.Write(env); err != nil {
				return
			}
		}
		taskEnv, err := delegate.NewTaskEnvelope(task)
		if err == nil {
			_ = writer.Write(taskEnv)
		}
	}()

	resultCh := make(chan oversizedResultOutcome, 1)
	go func() {
		reader := delegate.NewEnvelopeReader(stdoutR)
		for {
			env, err := reader.Read()
			if err != nil {
				resultCh <- oversizedResultOutcome{readErr: err}
				return
			}
			if env.Type == delegate.EnvelopeResult {
				var result delegate.IOResult
				readErr := json.Unmarshal(env.Data, &result)
				resultCh <- oversizedResultOutcome{
					envelope: env, result: result, sawResult: true, readErr: readErr,
				}
				return
			}
		}
	}()

	var got oversizedResultOutcome
	select {
	case got = <-resultCh:
	case <-time.After(120 * time.Second):
		t.Fatal("the runner neither answered nor exited")
	}
	_ = stdinW.Close()
	wg.Wait()
	got.runErr = runErr
	got.stderr = stderr.String()
	return got
}

func assertOversizedResultFallback(t *testing.T, got oversizedResultOutcome) {
	t.Helper()
	if !got.sawResult {
		t.Fatalf("the runner returned no IOResult beside its line refusal: run error=%v, read error=%v", got.runErr, got.readErr)
	}
	if got.readErr != nil {
		t.Fatalf("decode the fallback IOResult: %v", got.readErr)
	}
	if err := delegate.CheckEnvelopeLine(got.envelope); err != nil {
		t.Fatalf("the fallback result does not fit the channel: %v", err)
	}
	if !errors.Is(got.runErr, delegate.ErrEnvelopeLineTooLong) {
		t.Fatalf("the runner's exit does not carry the channel's typed refusal: %v", got.runErr)
	}
	if got.result.Error != got.runErr.Error() {
		t.Errorf("fallback Error = %q, want the original refusal %q", got.result.Error, got.runErr)
	}
	const prefix = `delegate: refusing to write a "result" envelope of `
	if !strings.HasPrefix(got.runErr.Error(), prefix) {
		t.Fatalf("the refusal does not name the result envelope and its size: %v", got.runErr)
	}
	var size, lineCap int
	if _, err := fmt.Sscanf(strings.TrimPrefix(got.runErr.Error(), prefix),
		"%d bytes (line cap %d)", &size, &lineCap); err != nil {
		t.Fatalf("the refusal does not carry its actual size and cap: %v (%v)", got.runErr, err)
	}
	if lineCap != delegate.MaxEnvelopeLineBytes || size <= lineCap {
		t.Errorf("refusal size/cap = %d/%d, want an actual size above the %d-byte cap",
			size, lineCap, delegate.MaxEnvelopeLineBytes)
	}
	if !strings.Contains(got.stderr, "write result envelope") ||
		!strings.Contains(got.stderr, fmt.Sprint(delegate.MaxEnvelopeLineBytes)) {
		t.Errorf("the container's own log does not say which envelope the channel refused, nor its cap: %q", got.stderr)
	}
}

// lockedBuffer is a stderr a test goroutine can read while the runner
// writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
