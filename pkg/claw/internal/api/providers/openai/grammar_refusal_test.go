package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// litellmGrammarRefusal is the body PLIAGE's LiteLLM serves, as a 500, when
// vLLM's grammar engine refuses the tools' schemas under a forced tool_choice.
const litellmGrammarRefusal = `{"error":{"message":"litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty:  (And (SearchRegex /^x$/) (Regex /(?s:.{1,253})/))","type":null,"param":null,"code":"500"}}`

// Both OpenAI wires — chat completions and Responses — report a grammar
// refusal as a non-retryable APIError: retrying the same request meets the
// same refusal.
func TestStreamResponse_AGrammarRefusalIsNotRetryable(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		req        api.CreateMessageRequest
	}{
		{"chat completions", "/v1/chat/completions", api.CreateMessageRequest{Model: "scaleway/qwen3.6-35b-a3b"}},
		{"responses", "/v1/responses", api.CreateMessageRequest{Model: "gpt-5.5", ReasoningEffort: "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("path = %s, want %s", r.URL.Path, tc.path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, litellmGrammarRefusal)
			}))
			defer srv.Close()

			client := &Client{APIKey: "k", BaseURL: srv.URL, Model: tc.req.Model, HTTPClient: srv.Client()}
			req := tc.req
			req.MaxTokens = 16
			req.Messages = []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}
			req.Tools = []api.Tool{{Name: "x", InputSchema: api.InputSchema{Type: "object"}}}
			req.ToolChoice = &api.ToolChoice{Type: "any"}

			_, err := client.StreamResponse(context.Background(), req)
			var apiErr *api.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *api.APIError", err, err)
			}
			if apiErr.StatusCode != http.StatusInternalServerError || !apiErr.IsGrammarRefusal() {
				t.Fatalf("err = %+v, want the 500 grammar refusal", apiErr)
			}
			if apiErr.IsRetryable() {
				t.Error("a grammar refusal must not be retryable")
			}
		})
	}
}
