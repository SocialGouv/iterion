package api

import (
	"fmt"
	"strings"
	"testing"
)

func TestSseParser_SingleEvent(t *testing.T) {
	p := NewSseParser()
	chunk := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventContentBlockDelta {
		t.Errorf("expected type %s, got %s", EventContentBlockDelta, events[0].Type)
	}
	if events[0].Delta.Text != "hello" {
		t.Errorf("expected delta text 'hello', got %q", events[0].Delta.Text)
	}
}

func TestSseParser_MultipleEventsInOneChunk(t *testing.T) {
	p := NewSseParser()
	chunk := []byte(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
	)
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != EventMessageStart {
		t.Errorf("event[0] type: expected %s, got %s", EventMessageStart, events[0].Type)
	}
	if events[0].InputTokens != 42 {
		t.Errorf("event[0] input tokens: expected 42, got %d", events[0].InputTokens)
	}
	if events[1].Type != EventContentBlockDelta {
		t.Errorf("event[1] type: expected %s, got %s", EventContentBlockDelta, events[1].Type)
	}
}

func TestSseParser_ChunkedDelivery(t *testing.T) {
	p := NewSseParser()
	full := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"world\"}}\n\n"

	// Split in the middle of the data line
	split := len(full) / 2
	chunk1 := []byte(full[:split])
	chunk2 := []byte(full[split:])

	events, err := p.Push(chunk1)
	if err != nil {
		t.Fatalf("unexpected error on chunk1: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events from partial chunk, got %d", len(events))
	}

	events, err = p.Push(chunk2)
	if err != nil {
		t.Fatalf("unexpected error on chunk2: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event after completing frame, got %d", len(events))
	}
	if events[0].Delta.Text != "world" {
		t.Errorf("expected 'world', got %q", events[0].Delta.Text)
	}
}

func TestSseParser_CommentLinesSkipped(t *testing.T) {
	p := NewSseParser()
	chunk := []byte(": this is a comment\nevent: content_block_delta\n: another comment\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Delta.Text != "ok" {
		t.Errorf("expected 'ok', got %q", events[0].Delta.Text)
	}
}

func TestSseParser_MultiLineData(t *testing.T) {
	p := NewSseParser()
	// Two data: lines should be joined with \n
	chunk := []byte("data: {\"type\":\"content_block_delta\",\"index\":0,\ndata: \"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The joined data should be the two parts with \n between
	// This will fail JSON parse because the join creates invalid JSON,
	// but we're testing the joining behavior
	if len(events) == 0 && err == nil {
		// Expected: the JSON parse fails, which is returned as error
		// Let's test with valid multi-line scenario instead
	}

	// More realistic: multi-line data that forms valid JSON when joined
	p2 := NewSseParser()
	chunk2 := []byte("event: message_start\ndata: {\"type\":\"message_start\",\ndata: \"message\":{\"usage\":{\"input_tokens\":10}}}\n\n")
	events2, err2 := p2.Push(chunk2)
	// The data lines join to: {"type":"message_start",\n"message":{"usage":{"input_tokens":10}}}
	// The \n in JSON is whitespace, so this is valid
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if len(events2) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events2))
	}
	if events2[0].Type != EventMessageStart {
		t.Errorf("expected message_start, got %s", events2[0].Type)
	}
}

func TestSseParser_EmptyData(t *testing.T) {
	p := NewSseParser()
	// Frame with event but no data
	chunk := []byte("event: ping\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Ping events are filtered
	if len(events) != 0 {
		t.Fatalf("expected 0 events (ping filtered), got %d", len(events))
	}
}

func TestSseParser_DoneFiltered(t *testing.T) {
	p := NewSseParser()
	chunk := []byte("data: [DONE]\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events ([DONE] filtered), got %d", len(events))
	}
}

func TestSseParser_PingEventFiltered(t *testing.T) {
	p := NewSseParser()
	chunk := []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events (ping filtered), got %d", len(events))
	}
}

func TestSseParser_LargePayload(t *testing.T) {
	p := NewSseParser()
	// Build a payload > 1MB
	bigText := strings.Repeat("x", 1024*1024+100)
	data := fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"%s"}}`, bigText)
	chunk := []byte("event: content_block_delta\ndata: " + data + "\n\n")

	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if len(events[0].Delta.Text) != 1024*1024+100 {
		t.Errorf("expected text length %d, got %d", 1024*1024+100, len(events[0].Delta.Text))
	}
}

func TestSseParser_ErrorContextIncludesProviderModel(t *testing.T) {
	p := NewSseParser().WithContext("anthropic", "claude-3-opus")
	chunk := []byte("data: {invalid json}\n\n")
	_, err := p.Push(chunk)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "anthropic") {
		t.Errorf("error should contain provider name, got: %s", errStr)
	}
	if !strings.Contains(errStr, "claude-3-opus") {
		t.Errorf("error should contain model name, got: %s", errStr)
	}
}

func TestSseParser_ErrorWithoutContext(t *testing.T) {
	p := NewSseParser()
	chunk := []byte("data: not-json\n\n")
	_, err := p.Push(chunk)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	errStr := err.Error()
	if strings.Contains(errStr, "provider=") {
		t.Errorf("error should not contain provider context when not set, got: %s", errStr)
	}
}

func TestSseParser_FinishFlushesRemaining(t *testing.T) {
	p := NewSseParser()
	// Push data without trailing \n\n
	chunk := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"fin\"}}")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events before Finish, got %d", len(events))
	}

	events, err = p.Finish()
	if err != nil {
		t.Fatalf("unexpected error on Finish: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event after Finish, got %d", len(events))
	}
	if events[0].Delta.Text != "fin" {
		t.Errorf("expected 'fin', got %q", events[0].Delta.Text)
	}
}

func TestSseParser_FinishWithEmptyBuffer(t *testing.T) {
	p := NewSseParser()
	events, err := p.Finish()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events from empty Finish, got %d", len(events))
	}
}

func TestSseParser_CRLFDelimiters(t *testing.T) {
	p := NewSseParser()
	chunk := []byte("event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"crlf\"}}\r\n\r\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Delta.Text != "crlf" {
		t.Errorf("expected 'crlf', got %q", events[0].Delta.Text)
	}
}

func TestSseParser_MessageStopEvent(t *testing.T) {
	p := NewSseParser()
	chunk := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventMessageStop {
		t.Errorf("expected message_stop, got %s", events[0].Type)
	}
}

func TestSseParser_MessageDeltaWithStopReason(t *testing.T) {
	p := NewSseParser()
	chunk := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":15}}\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].StopReason != "end_turn" {
		t.Errorf("expected stop_reason 'end_turn', got %q", events[0].StopReason)
	}
	if events[0].Usage.OutputTokens != 15 {
		t.Errorf("expected output_tokens 15, got %d", events[0].Usage.OutputTokens)
	}
}

func TestSseParser_OnlyCommentFrame(t *testing.T) {
	p := NewSseParser()
	chunk := []byte(": just a comment\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for comment-only frame, got %d", len(events))
	}
}

func TestSseParser_DataWithNoSpaceAfterColon(t *testing.T) {
	p := NewSseParser()
	// Per SSE spec, "data:value" (no space) is valid
	chunk := []byte("data:{\"type\":\"message_stop\"}\n\n")
	events, err := p.Push(chunk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventMessageStop {
		t.Errorf("expected message_stop, got %s", events[0].Type)
	}
}
