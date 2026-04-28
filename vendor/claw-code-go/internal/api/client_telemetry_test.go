package api

import (
	"claw-code-go/internal/apikit"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// collectEvents drains the event channel with a timeout.
func collectEvents(ch <-chan StreamEvent, timeout time.Duration) []StreamEvent {
	var events []StreamEvent
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timer.C:
			return events
		}
	}
}

// ssePayload builds a raw SSE frame with event type and JSON data.
func ssePayload(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

func TestStreamResponse_TracerRecordsHTTPLifecycle(t *testing.T) {
	// Build a fake SSE stream with message_start -> content_block_start ->
	// content_block_delta -> content_block_stop -> message_delta -> message_stop.
	body := strings.Join([]string{
		ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`),
		ssePayload("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","id":"blk_1"}}`),
		ssePayload("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`),
		ssePayload("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ssePayload("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`),
		ssePayload("message_stop", `{"type":"message_stop"}`),
	}, "")

	reqID := "req-abc-123"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", reqID)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
	defer ts.Close()

	sink := &apikit.MemoryTelemetrySink{}
	tracer := apikit.NewSessionTracer("sess-1", sink)

	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL
	client.Tracer = tracer

	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}

	events := collectEvents(ch, 5*time.Second)
	if len(events) == 0 {
		t.Fatal("expected at least one stream event")
	}

	// Check telemetry events: should have started, succeeded, and traces.
	telEvents := sink.Events()
	var hasStarted, hasSucceeded bool
	for _, te := range telEvents {
		switch te.Type {
		case apikit.EventTypeHTTPRequestStarted:
			hasStarted = true
			if te.Method != "POST" || te.Path != "/v1/messages" {
				t.Errorf("started event: unexpected method=%s path=%s", te.Method, te.Path)
			}
		case apikit.EventTypeHTTPRequestSucceeded:
			hasSucceeded = true
			if te.Status != 200 {
				t.Errorf("succeeded event: expected status 200, got %d", te.Status)
			}
			if te.RequestID != reqID {
				t.Errorf("succeeded event: expected request_id %q, got %q", reqID, te.RequestID)
			}
		}
	}
	if !hasStarted {
		t.Error("missing http_request_started telemetry event")
	}
	if !hasSucceeded {
		t.Error("missing http_request_succeeded telemetry event")
	}
}

func TestStreamResponse_TracerRecordsFailedRequest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer ts.Close()

	sink := &apikit.MemoryTelemetrySink{}
	tracer := apikit.NewSessionTracer("sess-2", sink)

	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL
	client.Tracer = tracer

	_, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected error for 429 response")
	}

	telEvents := sink.Events()
	var hasFailed bool
	for _, te := range telEvents {
		if te.Type == apikit.EventTypeHTTPRequestFailed {
			hasFailed = true
			if !te.Retryable {
				t.Error("429 should be marked retryable")
			}
			if !strings.Contains(te.Error, "429") {
				t.Errorf("failed event error should contain status code, got: %s", te.Error)
			}
		}
	}
	if !hasFailed {
		t.Error("missing http_request_failed telemetry event")
	}
}

func TestStreamResponse_TracerRecords5xxRetryable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`bad gateway`))
	}))
	defer ts.Close()

	sink := &apikit.MemoryTelemetrySink{}
	tracer := apikit.NewSessionTracer("sess-3", sink)

	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL
	client.Tracer = tracer

	_, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected error for 502 response")
	}

	for _, te := range sink.Events() {
		if te.Type == apikit.EventTypeHTTPRequestFailed {
			if !te.Retryable {
				t.Error("502 should be marked retryable")
			}
			return
		}
	}
	t.Error("missing http_request_failed telemetry event for 502")
}

func TestStreamResponse_4xxNonRetryable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer ts.Close()

	sink := &apikit.MemoryTelemetrySink{}
	tracer := apikit.NewSessionTracer("sess-4", sink)

	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL
	client.Tracer = tracer

	_, _ = client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})

	for _, te := range sink.Events() {
		if te.Type == apikit.EventTypeHTTPRequestFailed {
			if te.Retryable {
				t.Error("400 should NOT be marked retryable")
			}
			return
		}
	}
	t.Error("missing http_request_failed telemetry event for 400")
}

func TestStreamResponse_SseParserChunkedDelivery(t *testing.T) {
	// Simulate chunked SSE delivery: the server sends data in small pieces
	// that don't align with frame boundaries.
	fullBody := strings.Join([]string{
		ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`),
		ssePayload("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","id":"blk_1"}}`),
		ssePayload("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chunk1"}}`),
		ssePayload("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chunk2"}}`),
		ssePayload("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ssePayload("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`),
		ssePayload("message_stop", `{"type":"message_stop"}`),
	}, "")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Send in small chunks to simulate chunked transfer
		data := []byte(fullBody)
		chunkSize := 30
		for i := 0; i < len(data); i += chunkSize {
			end := i + chunkSize
			if end > len(data) {
				end = len(data)
			}
			w.Write(data[i:end])
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer ts.Close()

	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL

	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}

	events := collectEvents(ch, 5*time.Second)

	// Verify we got all expected event types
	typeCount := make(map[StreamEventType]int)
	for _, ev := range events {
		typeCount[ev.Type]++
	}

	if typeCount[EventMessageStart] != 1 {
		t.Errorf("expected 1 message_start, got %d", typeCount[EventMessageStart])
	}
	if typeCount[EventContentBlockDelta] != 2 {
		t.Errorf("expected 2 content_block_delta, got %d", typeCount[EventContentBlockDelta])
	}
	if typeCount[EventMessageStop] != 1 {
		t.Errorf("expected 1 message_stop, got %d", typeCount[EventMessageStop])
	}

	// Verify text deltas
	var texts []string
	for _, ev := range events {
		if ev.Type == EventContentBlockDelta && ev.Delta.Text != "" {
			texts = append(texts, ev.Delta.Text)
		}
	}
	if len(texts) != 2 || texts[0] != "chunk1" || texts[1] != "chunk2" {
		t.Errorf("unexpected text deltas: %v", texts)
	}
}

func TestStreamResponse_AuthSourceAPIKey(t *testing.T) {
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)))
		w.Write([]byte(ssePayload("message_stop", `{"type":"message_stop"}`)))
	}))
	defer ts.Close()

	client := NewClient("", "claude-test")
	client.BaseURL = ts.URL
	client.Auth = APIKeyAuth("sk-structured")

	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}
	collectEvents(ch, 2*time.Second)

	if gotHeader != "sk-structured" {
		t.Errorf("expected x-api-key 'sk-structured', got %q", gotHeader)
	}
}

func TestStreamResponse_AuthSourceBearer(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)))
		w.Write([]byte(ssePayload("message_stop", `{"type":"message_stop"}`)))
	}))
	defer ts.Close()

	client := NewClient("", "claude-test")
	client.BaseURL = ts.URL
	client.Auth = BearerAuth("tok-xyz")

	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}
	collectEvents(ch, 2*time.Second)

	if gotAuth != "Bearer tok-xyz" {
		t.Errorf("expected Authorization 'Bearer tok-xyz', got %q", gotAuth)
	}
}

func TestStreamResponse_LegacyOAuthTokenFallback(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)))
		w.Write([]byte(ssePayload("message_stop", `{"type":"message_stop"}`)))
	}))
	defer ts.Close()

	client := NewClient("sk-ignored", "claude-test")
	client.BaseURL = ts.URL
	client.OAuthToken = "legacy-oauth"
	// Auth.Kind is AuthSourceNone (zero value), so legacy path is used

	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}
	collectEvents(ch, 2*time.Second)

	if gotAuth != "Bearer legacy-oauth" {
		t.Errorf("expected authorization 'Bearer legacy-oauth', got %q", gotAuth)
	}
}

func TestStreamResponse_LegacyAPIKeyFallback(t *testing.T) {
	var gotKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)))
		w.Write([]byte(ssePayload("message_stop", `{"type":"message_stop"}`)))
	}))
	defer ts.Close()

	client := NewClient("sk-legacy", "claude-test")
	client.BaseURL = ts.URL

	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}
	collectEvents(ch, 2*time.Second)

	if gotKey != "sk-legacy" {
		t.Errorf("expected x-api-key 'sk-legacy', got %q", gotKey)
	}
}

func TestWithTracer(t *testing.T) {
	sink := &apikit.MemoryTelemetrySink{}
	tracer := apikit.NewSessionTracer("sess-wt", sink)

	client := NewClient("sk-test", "claude-test")
	result := client.WithTracer(tracer)

	if result != client {
		t.Error("WithTracer should return the same client pointer")
	}
	if client.Tracer != tracer {
		t.Error("WithTracer should set the Tracer field")
	}
}

func TestStreamResponse_NoTracerNoPanic(t *testing.T) {
	// Ensure that when Tracer is nil, no panic occurs.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)))
		w.Write([]byte(ssePayload("message_stop", `{"type":"message_stop"}`)))
	}))
	defer ts.Close()

	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL
	// Tracer is nil (default)

	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}

	events := collectEvents(ch, 2*time.Second)
	if len(events) == 0 {
		t.Error("expected at least one event even without tracer")
	}
}

func TestIsRetryableStatus(t *testing.T) {
	cases := []struct {
		code     int
		expected bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{408, true},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
	}
	for _, tc := range cases {
		if got := isRetryableStatus(tc.code); got != tc.expected {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", tc.code, got, tc.expected)
		}
	}
}
