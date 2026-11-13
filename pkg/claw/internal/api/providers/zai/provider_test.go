package zai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// ssePayload builds a raw SSE frame with event type and JSON data — the same
// helper shape the api client's own tests use, so the stub answers with a
// stream the shared client genuinely parses.
func ssePayload(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

func TestProviderName(t *testing.T) {
	if got := New().Name(); got != "zai" {
		t.Errorf("Name() = %q, want %q", got, "zai")
	}
}

func TestProviderAuthMethod(t *testing.T) {
	if got := New().AuthMethod(); got != api.AuthMethodAPIKey {
		t.Errorf("AuthMethod() = %q, want %q", got, api.AuthMethodAPIKey)
	}
}

func TestNewClientDefaultsToZaiEndpoint(t *testing.T) {
	c, err := New().NewClient(api.ProviderConfig{APIKey: "zai-key", Model: "glm-5.3"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ac, ok := c.(*api.Client)
	if !ok {
		t.Fatalf("NewClient returned %T, want *api.Client", c)
	}
	if ac.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want the z.ai endpoint %q", ac.BaseURL, DefaultBaseURL)
	}
	if ac.APIKey != "zai-key" {
		t.Errorf("APIKey = %q, want the caller's key", ac.APIKey)
	}
	if ac.Model != "glm-5.3" {
		t.Errorf("Model = %q, want it passed verbatim", ac.Model)
	}
}

func TestNewClientHonoursBaseURLOverride(t *testing.T) {
	c, err := New().NewClient(api.ProviderConfig{APIKey: "zai-key", Model: "glm-5.3", BaseURL: "https://open.bigmodel.cn/api/anthropic"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ac := c.(*api.Client)
	if ac.BaseURL != "https://open.bigmodel.cn/api/anthropic" {
		t.Errorf("BaseURL = %q, want the caller's override", ac.BaseURL)
	}
}

// The wire proof: a z.ai client answers on its own endpoint with the account
// key in the x-api-key header and the model carried in the body verbatim —
// a wrong header or a mangled body is exactly what "unknown provider" used
// to hide.
func TestNewClientSendsXAPIKeyOnZaiWire(t *testing.T) {
	var gotKey, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Join([]string{
			ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
			ssePayload("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","id":"blk"}}`),
			ssePayload("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			ssePayload("content_block_stop", `{"type":"content_block_stop","index":0}`),
			ssePayload("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
			ssePayload("message_stop", `{"type":"message_stop"}`),
		}, "")))
	}))
	defer ts.Close()

	c, err := New().NewClient(api.ProviderConfig{APIKey: "zai-key", Model: "glm-5.3", BaseURL: ts.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ac := c.(*api.Client)
	ch, err := ac.StreamResponse(context.Background(), api.CreateMessageRequest{Model: "glm-5.3", MaxTokens: 8, Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "ping"}}}}})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	for range ch { // drain; the assertions live on the request the server saw
	}
	if gotKey != "zai-key" {
		t.Errorf("x-api-key = %q, want the account key", gotKey)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("request path = %q, want the Anthropic-compatible /v1/messages", gotPath)
	}
	if !strings.Contains(gotBody, `"model":"glm-5.3"`) {
		t.Errorf("request body %q does not carry the model verbatim", gotBody)
	}
}
