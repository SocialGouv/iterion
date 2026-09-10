package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// #837: the launcher→runner direction had no clamp. A host-side tool
// whose result exceeds MaxEnvelopeLineBytes (an MCP call, a large file
// read, a big `git diff`) produced a line the runner's reader refuses —
// killing the WHOLE IPC channel, so the node died on a channel error
// instead of the model receiving a bounded result.
//
// The runner→launcher half (relayed events) was clamped by #811; this is
// its mirror, with the same marker discipline: truncate text with a
// marker naming the omitted bytes, never emit a broken JSON.

// oversizeToolResultFixture drives a REAL Multiplexer over pipes: the
// test plays the runner (emitting a tool_call, then reading the reply
// with the real EnvelopeReader), the multiplexer plays the launcher.
type oversizeToolResultFixture struct {
	t          *testing.T
	toRunner   *EnvelopeReader
	runnerSend *EnvelopeWriter
	done       chan error
}

func newToolCallFixture(t *testing.T, onToolCall func(context.Context, string, json.RawMessage) (string, error)) *oversizeToolResultFixture {
	t.Helper()
	runnerOutR, runnerOutW := io.Pipe() // runner → launcher
	launcherOutR, launcherOutW := io.Pipe()

	m := NewMultiplexer(runnerOutR, launcherOutW, MultiplexerHandler{OnToolCall: onToolCall})
	f := &oversizeToolResultFixture{
		t:          t,
		toRunner:   NewEnvelopeReader(launcherOutR),
		runnerSend: NewEnvelopeWriter(runnerOutW),
		done:       make(chan error, 1),
	}
	go func() {
		_, err := m.Run(context.Background())
		f.done <- err
	}()
	t.Cleanup(func() {
		_ = runnerOutW.Close()
		_ = launcherOutW.Close()
	})
	return f
}

// call emits a tool_call and returns the launcher's reply.
func (f *oversizeToolResultFixture) call(id, name string) (Envelope, error) {
	f.t.Helper()
	env, err := NewToolCallEnvelope(id, name, json.RawMessage(`{}`))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.runnerSend.Write(env); err != nil {
		f.t.Fatalf("write tool_call: %v", err)
	}
	type read struct {
		env Envelope
		err error
	}
	ch := make(chan read, 1)
	go func() {
		e, rerr := f.toRunner.Read()
		ch <- read{e, rerr}
	}()
	select {
	case r := <-ch:
		return r.env, r.err
	case <-time.After(10 * time.Second):
		f.t.Fatal("launcher never answered the tool_call")
		return Envelope{}, nil
	}
}

// A 5 MiB host-side tool result must reach the model BOUNDED and MARKED,
// and the channel must survive: today the line is written whole, the
// runner's reader refuses it (ErrEnvelopeLineTooLong) and the node dies.
func TestToolResult_OversizeIsClampedAndTheChannelSurvives(t *testing.T) {
	huge := strings.Repeat("x", 5*1024*1024)
	f := newToolCallFixture(t, func(context.Context, string, json.RawMessage) (string, error) {
		return huge, nil
	})

	env, err := f.call("call-1", "read_file")
	if err != nil {
		if errors.Is(err, ErrEnvelopeLineTooLong) {
			t.Fatalf("a 5 MiB tool result killed the IPC channel (%v) — the node dies on a wire error instead of the model getting a bounded result", err)
		}
		t.Fatalf("reading the tool_result failed: %v", err)
	}
	if env.Type != EnvelopeToolResult || env.ID != "call-1" {
		t.Fatalf("got envelope %+v, want a tool_result correlated to call-1", env)
	}
	var data ToolResultData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("tool_result payload is not decodable JSON: %v — a cut mid-document is never acceptable", err)
	}
	if len(data.Output) > MaxToolResultBytes+512 {
		t.Fatalf("clamped output is %d bytes, want ≤ MaxToolResultBytes (%d) plus the marker", len(data.Output), MaxToolResultBytes)
	}
	if !strings.Contains(data.Output, "5242880") {
		t.Fatalf("clamped output does not name the original size — the model cannot tell a short answer from one that was cut: %q", lastBytes(data.Output, 200))
	}
	if data.Error != "" {
		t.Fatalf("clamping turned a successful tool call into an error: %q", data.Error)
	}

	// The channel must still carry the NEXT call — one clamped result is
	// not a dead session.
	f2, err := f.call("call-2", "read_file")
	if err != nil {
		t.Fatalf("the channel died after the oversize result: %v", err)
	}
	if f2.ID != "call-2" {
		t.Fatalf("second reply correlated to %q, want call-2", f2.ID)
	}
}

// The error channel carries tool output too (a failing command's stderr),
// so it is clamped by the same rule — an oversize error message would
// kill the IPC exactly as an oversize output does.
func TestToolResult_OversizeErrorIsClamped(t *testing.T) {
	huge := strings.Repeat("e", 5*1024*1024)
	f := newToolCallFixture(t, func(context.Context, string, json.RawMessage) (string, error) {
		return "", errors.New(huge)
	})

	env, err := f.call("call-err", "bash")
	if err != nil {
		t.Fatalf("a 5 MiB tool ERROR killed the IPC channel: %v", err)
	}
	var data ToolResultData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("tool_result payload is not decodable JSON: %v", err)
	}
	if len(data.Error) > MaxToolResultBytes+512 {
		t.Fatalf("clamped error is %d bytes, want ≤ MaxToolResultBytes (%d) plus the marker", len(data.Error), MaxToolResultBytes)
	}
	if !strings.Contains(data.Error, "5242880") {
		t.Fatalf("clamped error does not name the original size: %q", lastBytes(data.Error, 200))
	}
}

// A result that fits must cross untouched — no marker, no truncation on
// the ordinary path.
func TestToolResult_InBudgetIsUntouched(t *testing.T) {
	f := newToolCallFixture(t, func(context.Context, string, json.RawMessage) (string, error) {
		return "ok\n", nil
	})
	env, err := f.call("call-small", "bash")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var data ToolResultData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Output != "ok\n" {
		t.Fatalf("in-budget output was altered: %q", data.Output)
	}
}

// The ask_user payload carries the pre-pause LLM state the runner
// rebuilds a typed *ErrAskUser from: cutting it would resume onto a
// corrupted conversation. When it alone overflows the line, the result
// must become an explicit tool ERROR naming the size — the node then
// fails with a reason, and the channel lives.
func TestClampToolResult_OversizeAskUserBecomesAnExplicitError(t *testing.T) {
	conv := json.RawMessage(`{"messages":"` + strings.Repeat("m", 5*1024*1024) + `"}`)
	got := ClampToolResult(ToolResultData{AskUser: &AskUserToolFail{
		Question:     "continue?",
		Conversation: conv,
	}})
	if got.AskUser != nil {
		t.Fatal("an over-cap ask_user payload was forwarded — the line kills the channel")
	}
	if got.Error == "" {
		t.Fatal("an over-cap ask_user payload was dropped with no error — the runner would wait on a reply that says nothing")
	}
	if !strings.Contains(got.Error, "5242") {
		t.Fatalf("the refusal does not name the payload size: %q", got.Error)
	}
	if !toolResultFitsOneLine(got) {
		t.Fatal("the refusal itself does not fit one line")
	}
}

// An ask_user payload that fits crosses whole — the refusal above is for
// the overflow case only, never the ordinary pause.
func TestClampToolResult_InBudgetAskUserCrossesWhole(t *testing.T) {
	conv := json.RawMessage(`{"messages":["hi"]}`)
	got := ClampToolResult(ToolResultData{AskUser: &AskUserToolFail{
		Question:     "continue?",
		Conversation: conv,
	}})
	if got.AskUser == nil {
		t.Fatal("an in-budget ask_user payload was refused — every sandboxed pause would break")
	}
	if string(got.AskUser.Conversation) != string(conv) {
		t.Fatalf("conversation altered: %s", got.AskUser.Conversation)
	}
}

// The writer is the chokepoint EVERY envelope crosses, in both
// directions. A line over the cap must fail AT THE SOURCE, naming the
// envelope type and its size — not silently, and not as an opaque
// ErrEnvelopeLineTooLong on the peer, which reports the reader's view of
// a payload it never saw and names nothing that produced it.
func TestEnvelopeWriter_RefusesAnOversizeLineAtTheSource(t *testing.T) {
	var sink strings.Builder
	w := NewEnvelopeWriter(&sink)
	env := Envelope{
		Type: EnvelopeSessionReplay,
		Data: json.RawMessage(`"` + strings.Repeat("z", 5*1024*1024) + `"`),
	}
	err := w.Write(env)
	if err == nil {
		t.Fatal("an over-cap line was written — the peer's reader fails the WHOLE channel on it, with no idea what produced it")
	}
	if !errors.Is(err, ErrEnvelopeLineTooLong) {
		t.Fatalf("write refusal must carry ErrEnvelopeLineTooLong, got %v", err)
	}
	if !strings.Contains(err.Error(), string(EnvelopeSessionReplay)) {
		t.Fatalf("write refusal must name the envelope type that produced it, got %v", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("a refused envelope still wrote %d bytes — a partial line desynchronises the channel", sink.Len())
	}
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
