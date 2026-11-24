package providers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/anthropic"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/openai"
)

// Inspect the actual request rather than the carrier alone: OpenAI has a
// second conversion after InputSchema, and chat/responses nest tools differently.
// The local server only acknowledges the request; it does not claim that a
// remote provider or model accepts every recursive JSON Schema.
func TestNamedSchemaReachesProviderRequests(t *testing.T) {
	raw, err := os.ReadFile("../testdata/named_schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema api.InputSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		provider api.Provider
		wire     string
		path     string
		response string
	}{
		{"anthropic", anthropic.New(), "", "/v1/messages", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"openai-chat", openai.New(), api.OpenAIWireChat, "/v1/chat/completions", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		{"openai-responses", openai.New(), api.OpenAIWireResponses, "/v1/responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type request struct {
				path string
				body map[string]any
				err  error
			}
			observed := make(chan request, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				err := json.NewDecoder(r.Body).Decode(&body)
				observed <- request{r.URL.Path, body, err}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()
			client, err := tc.provider.NewClient(api.ProviderConfig{
				APIKey: "test-key", Model: "test-model", BaseURL: srv.URL,
				NoAmbientHeaders: true, OpenAIWireAPI: tc.wire,
			})
			if err != nil {
				t.Fatal(err)
			}
			events, err := client.StreamResponse(t.Context(), api.CreateMessageRequest{
				Model: "test-model", MaxTokens: 16,
				Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Return a tree."}}}},
				Tools:    []api.Tool{{Name: "result", InputSchema: schema}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for event := range events {
				if event.Type == api.EventError {
					t.Fatalf("stream error: %s", event.ErrorMessage)
				}
			}
			var got request
			select {
			case got = <-observed:
			default:
				t.Fatal("provider did not send a request")
			}
			if got.err != nil || got.path != tc.path {
				t.Fatalf("request path=%q, want %q; decode error=%v", got.path, tc.path, got.err)
			}
			tools, ok := got.body["tools"].([]any)
			if !ok || len(tools) != 1 {
				t.Fatalf("request tools=%#v", got.body["tools"])
			}
			tool, ok := tools[0].(map[string]any)
			if !ok {
				t.Fatalf("tool=%#v", tools[0])
			}
			var parameters any
			switch tc.wire {
			case api.OpenAIWireChat:
				function, ok := tool["function"].(map[string]any)
				if !ok {
					t.Fatalf("chat function=%#v", tool["function"])
				}
				parameters = function["parameters"]
			case api.OpenAIWireResponses:
				parameters = tool["parameters"]
			default:
				parameters = tool["input_schema"]
			}
			if !reflect.DeepEqual(parameters, want) {
				encoded, _ := json.Marshal(parameters)
				t.Fatalf("named schema changed on %s request: got %s; want %s", tc.path, encoded, raw)
			}
		})
	}
}
