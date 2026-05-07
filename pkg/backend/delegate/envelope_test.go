package delegate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestEnvelopeRoundTripAllTypes(t *testing.T) {
	build := func(env Envelope, err error) Envelope {
		t.Helper()
		if err != nil {
			t.Fatalf("build envelope: %v", err)
		}
		return env
	}
	cases := []struct {
		name string
		env  Envelope
	}{
		{"task", build(NewTaskEnvelope(IOTask{NodeID: "node-1", Model: "anthropic/claude-sonnet-4-6"}))},
		{"tool_call", build(NewToolCallEnvelope("call-1", "Bash", json.RawMessage(`{"command":"echo hi"}`)))},
		{"tool_result", build(NewToolResultEnvelope("call-1", "hi\n", ""))},
		{"tool_result_err", build(NewToolResultEnvelope("call-1", "", "permission denied"))},
		{"ask_user", build(NewAskUserEnvelope("ask-1", AskUserData{
			Reason:    "need confirmation",
			Questions: []AskUserQuestion{{Question: "ok?", Options: []AskUserQuestionOption{{Label: "yes"}, {Label: "no"}}}},
		}))},
		{"ask_user_answer", build(NewAskUserAnswerEnvelope("ask-1", map[string]string{"ok?": "yes"}))},
		{"session_capture", NewSessionCaptureEnvelope(json.RawMessage(`{"messages":[{"role":"user"}]}`))},
		{"session_replay", NewSessionReplayEnvelope(json.RawMessage(`{"messages":[{"role":"assistant"}]}`))},
		{"event", build(NewEventEnvelope("tool_called", map[string]interface{}{"name": "Bash"}))},
		{"result", build(NewResultEnvelope(IOResult{BackendName: "claw", Tokens: 1234}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewEnvelopeWriter(&buf)
			if err := w.Write(tc.env); err != nil {
				t.Fatalf("write: %v", err)
			}
			r := NewEnvelopeReader(&buf)
			got, err := r.Read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Type != tc.env.Type {
				t.Errorf("Type = %q, want %q", got.Type, tc.env.Type)
			}
			if got.ID != tc.env.ID {
				t.Errorf("ID = %q, want %q", got.ID, tc.env.ID)
			}
			if !bytes.Equal(got.Data, tc.env.Data) {
				t.Errorf("Data mismatch.\n got=%s\nwant=%s", got.Data, tc.env.Data)
			}
		})
	}
}

func TestEnvelopeReader_EOFOnCleanClose(t *testing.T) {
	r := NewEnvelopeReader(strings.NewReader(""))
	_, err := r.Read()
	if !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF, got %v", err)
	}
}

func TestEnvelopeReader_LineTooLong(t *testing.T) {
	// Craft a single line exceeding MaxEnvelopeLineBytes — even if it
	// doesn't parse as JSON, the cap kicks in first inside the scanner.
	huge := bytes.Repeat([]byte("x"), MaxEnvelopeLineBytes+10)
	r := NewEnvelopeReader(bytes.NewReader(huge))
	_, err := r.Read()
	if !errors.Is(err, ErrEnvelopeLineTooLong) {
		t.Errorf("want ErrEnvelopeLineTooLong, got %v", err)
	}
}

func TestEnvelopeReader_MalformedJSONReportsLine(t *testing.T) {
	r := NewEnvelopeReader(strings.NewReader("not-json\n"))
	_, err := r.Read()
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode envelope") {
		t.Errorf("error should mention decode failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not-json") {
		t.Errorf("error should include the offending line, got: %v", err)
	}
}

func TestEnvelopeWriter_ConcurrentSafe(t *testing.T) {
	// 8 goroutines × 100 envelopes each; each line must be a valid
	// envelope on the receiving side (no interleaving).
	var buf bytes.Buffer
	w := NewEnvelopeWriter(&buf)
	const goroutines = 8
	const perGoroutine = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				env, err := NewToolCallEnvelope("call", "Bash", json.RawMessage(`{"command":"echo hi"}`))
				if err != nil {
					t.Errorf("build envelope: %v", err)
					return
				}
				if err := w.Write(env); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	r := NewEnvelopeReader(&buf)
	count := 0
	for {
		env, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read iter %d: %v", count, err)
		}
		if env.Type != EnvelopeToolCall {
			t.Errorf("iter %d: Type = %q, want tool_call", count, env.Type)
		}
		count++
	}
	if want := goroutines * perGoroutine; count != want {
		t.Errorf("decoded %d envelopes, want %d (lines interleaved?)", count, want)
	}
}
