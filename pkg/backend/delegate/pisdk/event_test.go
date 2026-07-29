package pisdk

import (
	"strings"
	"testing"
)

func TestDecodeStreamHeader(t *testing.T) {
	s := DecodeStreamString(`{"type":"session","version":3,"id":"sess-1","timestamp":"t","cwd":"/w"}
{"type":"agent_start"}`)
	if s.Header == nil {
		t.Fatal("Header = nil, want the print-mode session header")
	}
	if s.Header.ID != "sess-1" || s.Header.Version != 3 {
		t.Errorf("Header = %+v, want id sess-1 version 3", *s.Header)
	}
	if len(s.Events) != 1 || s.Events[0].Type != EventAgentStart {
		t.Errorf("Events = %+v, want a single agent_start", s.Events)
	}
}

// pi ships roughly weekly on a 0.x line. A variant this port does not model
// must be inert, never a parse failure that fails a whole run.
func TestDecodeStreamToleratesUnknownVariants(t *testing.T) {
	s := DecodeStreamString(`{"type":"brand_new_event_from_a_future_pi","payload":{"deeply":{"nested":[1,2]}}}
{"type":"agent_settled"}`)
	if len(s.Events) != 2 {
		t.Fatalf("Events = %d, want 2 (the unknown one preserved, not dropped)", len(s.Events))
	}
	if s.Events[0].Type != "brand_new_event_from_a_future_pi" {
		t.Errorf("unknown event type = %q, want it preserved", s.Events[0].Type)
	}
	if len(s.Events[0].Raw) == 0 {
		t.Error("Raw is empty — an unmodelled variant becomes uninspectable")
	}
	if len(s.Unparsed) != 0 {
		t.Errorf("Unparsed = %v, want empty (valid JSON is not unparsed)", s.Unparsed)
	}
}

func TestDecodeStreamNonJSON(t *testing.T) {
	s := DecodeStreamString("pi: command not found\n")
	if len(s.Events) != 0 {
		t.Errorf("Events = %+v, want none", s.Events)
	}
	if len(s.Unparsed) != 1 || s.Unparsed[0] != "pi: command not found" {
		t.Errorf("Unparsed = %v, want the raw line", s.Unparsed)
	}
}

// pi's framing is LF-only. U+2028/U+2029 are legal inside JSON strings, and
// breaking on them (as Node's readline does) would corrupt the payload.
func TestDecodeStreamSplitsOnLFOnly(t *testing.T) {
	s := DecodeStreamString("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"a b\"}]}}\n")
	if len(s.Events) != 1 {
		t.Fatalf("Events = %d, want 1 — a line separator inside a JSON string split the line", len(s.Events))
	}
	if got := s.Events[0].Message.Text(); got != "a b" {
		t.Errorf("text = %q, want the separator preserved verbatim", got)
	}
}

// A single tool_execution_end carrying a large file read is routinely
// megabytes; bufio.Scanner's default 64 KiB cap would truncate it silently.
func TestDecodeStreamHandlesHugeLines(t *testing.T) {
	huge := strings.Repeat("x", 1<<20) // 1 MiB
	s := DecodeStreamString(`{"type":"tool_execution_end","toolCallId":"t1","toolName":"read","isError":false,"result":"` + huge + `"}` + "\n")
	if len(s.Events) != 1 {
		t.Fatalf("Events = %d, want 1 — the line was truncated", len(s.Events))
	}
	if len(s.Events[0].Result) < 1<<20 {
		t.Errorf("result length = %d, want >= 1 MiB intact", len(s.Events[0].Result))
	}
}

func TestFinalTranscriptSkipsRetriedAttempts(t *testing.T) {
	s := DecodeStreamString(`{"type":"agent_end","willRetry":true,"messages":[{"role":"assistant","responseId":"bad"}]}
{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","responseId":"good"}]}`)
	got := s.FinalTranscript()
	if len(got) != 1 || got[0].ResponseID != "good" {
		t.Errorf("FinalTranscript = %+v, want only the surviving attempt", got)
	}
}

func TestAssistantMessagesDeduplicates(t *testing.T) {
	// The same responseId appears in a message_end and again in agent_end's
	// transcript. Counting it twice would double the reported bill.
	s := DecodeStreamString(`{"type":"message_end","message":{"role":"assistant","responseId":"r1","usage":{"input":10,"output":1,"totalTokens":11,"cost":{"total":1}}}}
{"type":"agent_end","willRetry":false,"messages":[{"role":"user","content":"hi"},{"role":"assistant","responseId":"r1","usage":{"input":10,"output":1,"totalTokens":11,"cost":{"total":1}}}]}`)
	got := s.AssistantMessages()
	if len(got) != 1 {
		t.Fatalf("AssistantMessages = %d, want 1", len(got))
	}
	if got[0].Role != "assistant" {
		t.Errorf("role = %q, want the user message filtered out", got[0].Role)
	}
}

func TestAssistantMessagesFallsBackToMessageEnd(t *testing.T) {
	// A truncated or crashed run never emits agent_end.
	s := DecodeStreamString(`{"type":"message_end","message":{"role":"assistant","responseId":"r1","content":[{"type":"text","text":"hi"}]}}`)
	got := s.AssistantMessages()
	if len(got) != 1 || got[0].Text() != "hi" {
		t.Errorf("AssistantMessages = %+v, want the message_end fallback", got)
	}
}

func TestUsageReasoningIsSubsetOfOutput(t *testing.T) {
	reasoning := 12
	u := Usage{Input: 100, Output: 30, CacheRead: 5, CacheWrite: 2, Reasoning: &reasoning}
	if u.ReasoningTokens() != 12 {
		t.Errorf("ReasoningTokens = %d, want 12", u.ReasoningTokens())
	}
	if (Usage{}).ReasoningTokens() != 0 {
		t.Error("ReasoningTokens on a provider reporting none must be 0, not a panic")
	}
	if got := u.ContextTokens(); got != 107 {
		t.Errorf("ContextTokens = %d, want 107 (input+cacheRead+cacheWrite)", got)
	}
}

func TestMessageText(t *testing.T) {
	s := DecodeStreamString(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"answer"},{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"ls"}}]}}`)
	if got := s.Events[0].Message.Text(); got != "answer" {
		t.Errorf("Text = %q, want only the text blocks (no thinking, no tool call)", got)
	}
}

func TestMessageContentAsBareString(t *testing.T) {
	// pi types a user message's content as `string | Content[]`.
	s := DecodeStreamString(`{"type":"message_start","message":{"role":"user","content":"plain"}}`)
	if got := s.Events[0].Message.Text(); got != "plain" {
		t.Errorf("Text = %q, want plain", got)
	}
}

func TestStopReasonFailed(t *testing.T) {
	for _, r := range []StopReason{StopError, StopAborted} {
		if !r.Failed() {
			t.Errorf("%q.Failed() = false, want true", r)
		}
	}
	for _, r := range []StopReason{StopStop, StopToolUse, StopLength, StopPending} {
		if r.Failed() {
			t.Errorf("%q.Failed() = true, want false", r)
		}
	}
}

func TestEffectiveModel(t *testing.T) {
	if got := (Message{Model: "auto"}).EffectiveModel(); got != "auto" {
		t.Errorf("EffectiveModel = %q, want auto", got)
	}
	if got := (Message{Model: "auto", ResponseModel: "anthropic/claude-opus-4-8"}).EffectiveModel(); got != "anthropic/claude-opus-4-8" {
		t.Errorf("EffectiveModel = %q, want the model that actually served", got)
	}
}
