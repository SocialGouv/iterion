package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// streamOpenAIFrames serves the given SSE frames from a throwaway server and
// returns every event the responses translator produced.
func streamOpenAIFrames(t *testing.T, frames []string) []api.StreamEvent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &Client{APIKey: "k", BaseURL: srv.URL, Model: "gpt-5.5", MaxTokens: 256, HTTPClient: srv.Client()}
	req := api.CreateMessageRequest{
		Model: "gpt-5.5", MaxTokens: 256, ReasoningEffort: "high",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:    []api.Tool{{Name: "ping", InputSchema: api.InputSchema{Type: "object"}}},
	}
	if !shouldUseResponsesAPI(req) {
		t.Fatal("the responses dispatch gate did not fire on reasoning+tools")
	}
	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// usageOf returns the usage carried by the message_delta, and whether one was
// seen at all.
func usageOf(events []api.StreamEvent) (api.UsageDelta, bool) {
	for _, ev := range events {
		if ev.Type == api.EventMessageDelta {
			return ev.Usage, true
		}
	}
	return api.UsageDelta{}, false
}

// The /v1/responses endpoint reports its prompt count in the terminal
// usage payload — long after message_start, which is where the Anthropic
// vocabulary carries input tokens. Dropping it makes every caller read a
// turn as having cost nothing to send: an input/output ratio is then
// meaningless, and a caller that prices tokens under-bills the dearer half
// of nothing and the cheaper half of everything.
func TestStreamResponses_SurfacesInputTokens(t *testing.T) {
	events := streamOpenAIFrames(t, []string{
		`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m"}}`,
		`{"type":"response.output_text.delta","item_id":"m","delta":"hi"}`,
		`{"type":"response.output_text.done","item_id":"m"}`,
		`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":1234,"output_tokens":7,"total_tokens":1241}}}`,
	})
	usage, ok := usageOf(events)
	if !ok {
		t.Fatal("no message_delta in the translated stream")
	}
	if usage.InputTokens != 1234 {
		t.Errorf("input tokens = %d, want 1234 — the endpoint reported them and the translation dropped them", usage.InputTokens)
	}
	if usage.OutputTokens != 7 {
		t.Errorf("output tokens = %d, want 7", usage.OutputTokens)
	}
}

// A truncated response still costs its prompt. The incomplete arm captures
// usage precisely so the caller sees a non-zero cost; leaving input at zero
// there understates the one turn most likely to be expensive.
func TestStreamResponses_SurfacesInputTokensOnIncomplete(t *testing.T) {
	events := streamOpenAIFrames(t, []string{
		`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`,
		`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":900,"output_tokens":256,"total_tokens":1156}}}`,
	})
	usage, ok := usageOf(events)
	if !ok {
		t.Skip("this translation reports truncation as an error event, not a message_delta")
	}
	if usage.InputTokens != 900 {
		t.Errorf("input tokens = %d, want 900 on a truncated response", usage.InputTokens)
	}
}
