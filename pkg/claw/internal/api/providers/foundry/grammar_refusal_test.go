package foundry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// A grammar refusal served as a 500 is not a transient fault: Foundry's
// chat-completions wire reports it non-retryable.
func TestStreamResponse_AGrammarRefusalIsNotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"OpenAIException - The provided JSON schema contains features not supported by xgrammar.","code":"500"}}`))
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, Deployment: "dep-1", APIVersion: defaultAPIVersion, APIKey: "k", HTTPClient: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.StreamResponse(ctx, api.CreateMessageRequest{
		MaxTokens: 16,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *api.APIError", err, err)
	}
	if !apiErr.IsGrammarRefusal() || apiErr.IsRetryable() {
		t.Errorf("err = %+v, want a non-retryable grammar refusal", apiErr)
	}
}
