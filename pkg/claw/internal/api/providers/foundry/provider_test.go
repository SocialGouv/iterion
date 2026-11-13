package foundry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestMapModelID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"foundry/gpt-5.4-mini-prod", "gpt-5.4-mini-prod"},
		{"gpt-5.4-mini-prod", "gpt-5.4-mini-prod"},
		{"openai/gpt-4o", "openai/gpt-4o"}, // unknown prefix preserved
		{"", ""},
	}
	for _, c := range cases {
		if got := MapModelID(c.in); got != c.want {
			t.Errorf("MapModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProviderIdentity(t *testing.T) {
	p := New()
	if p.Name() != "foundry" {
		t.Errorf("Name() = %q, want foundry", p.Name())
	}
	if p.AuthMethod() != api.AuthMethodAzureIdentity {
		t.Errorf("AuthMethod() = %v, want %v", p.AuthMethod(), api.AuthMethodAzureIdentity)
	}
}

func TestNewClientMissingEndpoint(t *testing.T) {
	t.Setenv("AZURE_OPENAI_ENDPOINT", "")
	t.Setenv("AZURE_OPENAI_DEPLOYMENT", "")
	t.Setenv("AZURE_OPENAI_API_KEY", "")
	p := New()
	_, err := p.NewClient(api.ProviderConfig{Model: "foundry/dep"})
	if err == nil {
		t.Fatalf("expected error when AZURE_OPENAI_ENDPOINT is empty")
	}
	if !strings.Contains(err.Error(), "AZURE_OPENAI_ENDPOINT") {
		t.Errorf("expected error to mention AZURE_OPENAI_ENDPOINT, got %v", err)
	}
}

func TestNewClientMissingDeployment(t *testing.T) {
	t.Setenv("AZURE_OPENAI_ENDPOINT", "https://my-resource.openai.azure.com")
	t.Setenv("AZURE_OPENAI_DEPLOYMENT", "")
	t.Setenv("AZURE_OPENAI_API_KEY", "stub")
	p := New()
	_, err := p.NewClient(api.ProviderConfig{})
	if err == nil {
		t.Fatalf("expected error when deployment is unspecified")
	}
	if !strings.Contains(err.Error(), "deployment") {
		t.Errorf("expected error to mention deployment, got %v", err)
	}
}

func TestNewClientWithAPIKey(t *testing.T) {
	t.Setenv("AZURE_OPENAI_ENDPOINT", "https://my-resource.openai.azure.com")
	t.Setenv("AZURE_OPENAI_DEPLOYMENT", "")
	t.Setenv("AZURE_OPENAI_API_KEY", "stub-key")
	p := New()
	cli, err := p.NewClient(api.ProviderConfig{Model: "foundry/dep-1", MaxTokens: 256})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c := cli.(*Client)
	if c.Endpoint != "https://my-resource.openai.azure.com" {
		t.Errorf("Endpoint = %q", c.Endpoint)
	}
	if c.Deployment != "dep-1" {
		t.Errorf("Deployment = %q", c.Deployment)
	}
	if c.APIKey != "stub-key" {
		t.Errorf("APIKey = %q", c.APIKey)
	}
	if c.APIVersion != defaultAPIVersion {
		t.Errorf("APIVersion = %q, want %q", c.APIVersion, defaultAPIVersion)
	}
}

func TestEndpointURL(t *testing.T) {
	c := &Client{
		Endpoint:   "https://my-resource.openai.azure.com",
		Deployment: "gpt-5.4-mini-prod",
		APIVersion: "2024-08-01-preview",
	}
	got := c.endpoint()
	want := "https://my-resource.openai.azure.com/openai/deployments/gpt-5.4-mini-prod/chat/completions?api-version=2024-08-01-preview"
	if got != want {
		t.Errorf("endpoint:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildRequest(t *testing.T) {
	c := &Client{Endpoint: "https://x", Deployment: "d", APIVersion: defaultAPIVersion}
	body, err := c.buildRequest(api.CreateMessageRequest{
		MaxTokens: 256,
		System:    "You are a test.",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := parsed["model"]; ok {
		t.Errorf("foundry body must NOT contain model field (it is in the URL path as deployment)")
	}
	var stream bool
	_ = json.Unmarshal(parsed["stream"], &stream)
	if !stream {
		t.Errorf("stream should be true")
	}
	var maxToks int
	_ = json.Unmarshal(parsed["max_tokens"], &maxToks)
	if maxToks != 256 {
		t.Errorf("max_tokens = %d, want 256", maxToks)
	}

	// Verify messages: system prefix, then user.
	var msgs []map[string]any
	_ = json.Unmarshal(parsed["messages"], &msgs)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Errorf("messages[0].role = %v, want system", msgs[0]["role"])
	}
	if msgs[1]["role"] != "user" {
		t.Errorf("messages[1].role = %v, want user", msgs[1]["role"])
	}
}

// TestStreamResponseError verifies non-2xx responses become *api.APIError.
func TestStreamResponseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify URL shape: /openai/deployments/{dep}/chat/completions
		if !strings.HasPrefix(r.URL.Path, "/openai/deployments/dep-1/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("api-version") != defaultAPIVersion {
			t.Errorf("api-version query = %q", r.URL.Query().Get("api-version"))
		}
		if r.Header.Get("api-key") != "stub-key" {
			t.Errorf("api-key header = %q", r.Header.Get("api-key"))
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"backend down","code":"InternalServerError"}}`))
	}))
	defer srv.Close()

	c := &Client{
		Endpoint:   srv.URL,
		Deployment: "dep-1",
		APIVersion: defaultAPIVersion,
		APIKey:     "stub-key",
		HTTPClient: srv.Client(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.StreamResponse(ctx, api.CreateMessageRequest{
		MaxTokens: 16,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatalf("expected error from 500 response")
	}
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.APIError, got %T: %v", err, err)
	}
	if apiErr.Provider != "foundry" {
		t.Errorf("Provider = %q, want foundry", apiErr.Provider)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
	if !apiErr.Retryable {
		t.Errorf("500 should be retryable")
	}
	if !strings.Contains(apiErr.Message, "backend down") {
		t.Errorf("Message = %q, want to contain 'backend down'", apiErr.Message)
	}
}

// TestStreamResponseLive — opt-in smoke test against a real Azure OpenAI deployment.
func TestStreamResponseLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in -short mode")
	}
	if os.Getenv("RUN_LIVE_FOUNDRY") != "1" {
		t.Skip("set RUN_LIVE_FOUNDRY=1 to run the live Foundry smoke test")
	}
	if os.Getenv("AZURE_OPENAI_ENDPOINT") == "" || os.Getenv("AZURE_OPENAI_DEPLOYMENT") == "" {
		t.Skip("AZURE_OPENAI_ENDPOINT or AZURE_OPENAI_DEPLOYMENT not set")
	}
	p := New()
	clientI, err := p.NewClient(api.ProviderConfig{MaxTokens: 64})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ch, err := clientI.StreamResponse(ctx, api.CreateMessageRequest{
		MaxTokens: 64,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Say hi in one word."}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	for ev := range ch {
		if ev.Type == api.EventError {
			t.Errorf("stream error: %s", ev.ErrorMessage)
		}
	}
}

// TestConvertTools_PropagatesMarshalError pins the contract that tool
// conversion bubbles up json.Marshal failures rather than silently
// dropping the offending tool. Silently skipping a tool causes the model
// to emit tool_use calls for an undeclared name, which derails the
// conversation. We trigger a marshal failure by stuffing a chan into
// Property.Enum (chan values are not JSON-marshallable).
func TestConvertTools_PropagatesMarshalError(t *testing.T) {
	c := &Client{
		Endpoint:   "https://x",
		Deployment: "dep-1",
		APIVersion: defaultAPIVersion,
		APIKey:     "stub",
	}
	req := api.CreateMessageRequest{
		MaxTokens: 16,
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []api.Tool{
			{
				Name:        "broken",
				Description: "schema with unmarshallable enum",
				InputSchema: api.InputSchema{
					Type: "object",
					Properties: map[string]api.Property{
						"x": {Type: "string", Enum: []any{make(chan int)}},
					},
				},
			},
		},
	}

	if _, err := c.buildRequest(req); err == nil {
		t.Fatal("expected buildRequest to fail when a tool's input schema cannot be marshalled")
	} else if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error %q should mention the offending tool name %q", err.Error(), "broken")
	}
}
