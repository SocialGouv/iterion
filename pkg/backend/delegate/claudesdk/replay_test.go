package claudesdk

import (
	"slices"
	"testing"
)

// The lines CLI 2.1.280 streams (probe-replay, probe-f9c): a prompt replayed
// with --replay-user-messages carries its content as a bare string.
func TestAReplayedPromptParses(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"Reply with the single word PING."},"session_id":"s1","parent_tool_use_id":null,"uuid":"u-between","timestamp":"2026-09-29T18:00:00.000Z","isReplay":true}`)
	msg, err := unmarshalMessage(line)
	if err != nil {
		t.Fatalf("unmarshalMessage: %v", err)
	}
	um, ok := msg.(*UserMessage)
	if !ok || !um.IsReplay || um.UUID != "u-between" {
		t.Fatalf("msg = %#v, want a replayed user message carrying the sent uuid", msg)
	}
	if um.Message == nil || len(um.Message.Content) != 1 {
		t.Fatalf("content = %#v, want one text block", um.Message)
	}
	if tb, ok := um.Message.Content[0].(*TextBlock); !ok || tb.Text != "Reply with the single word PING." {
		t.Fatalf("block = %#v", um.Message.Content[0])
	}
}

func TestSessionStateParses(t *testing.T) {
	msg, err := unmarshalMessage([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle","uuid":"x","session_id":"s1"}`))
	if err != nil {
		t.Fatalf("unmarshalMessage: %v", err)
	}
	sm, _ := msg.(*SystemMessage)
	if state, ok := sm.SessionState(); !ok || state != "idle" {
		t.Fatalf("SessionState() = %q, %v", state, ok)
	}
	msg, _ = unmarshalMessage([]byte(`{"type":"system","subtype":"init","session_id":"s1"}`))
	if _, ok := msg.(*SystemMessage).SessionState(); ok {
		t.Fatal("an init is not a session state")
	}
}

func TestTheReplayFlagReachesOnlyAStreamingSession(t *testing.T) {
	cfg := processConfig{ReplayUserMessages: true}
	if !slices.Contains(buildArgs(cfg, true), "--replay-user-messages") {
		t.Fatal("a streaming session lost --replay-user-messages")
	}
	if slices.Contains(buildArgs(cfg, false), "--replay-user-messages") {
		t.Fatal("a one-shot prompt got --replay-user-messages, which the CLI refuses without stream-json input")
	}
}
