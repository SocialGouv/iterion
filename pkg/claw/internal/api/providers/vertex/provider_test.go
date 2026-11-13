package vertex

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
	"golang.org/x/oauth2"
)

func TestMapModelID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-sonnet-4-20250514", "claude-sonnet-4@20250514"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5@20251001"},
		{"claude-sonnet-4@20250514", "claude-sonnet-4@20250514"},        // already vertex-shaped
		{"vertex/claude-sonnet-4-20250514", "claude-sonnet-4@20250514"}, // strip routing prefix
		{"claude-sonnet-4", "claude-sonnet-4"},                          // no date suffix
		{"", ""},
		// Trailing version suffix is preserved verbatim. The "-YYYYMMDD"
		// run is rewritten to "@YYYYMMDD" even when it isn't the last
		// hyphen-separated segment (mirrors Bedrock's "-v1:0" idiom).
		{"claude-haiku-4-5-20251001-v1", "claude-haiku-4-5@20251001-v1"},
	}
	for _, c := range cases {
		if got := MapModelID(c.in); got != c.want {
			t.Errorf("MapModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProviderIdentity(t *testing.T) {
	p := New()
	if p.Name() != "vertex" {
		t.Errorf("Name() = %q, want vertex", p.Name())
	}
	if p.AuthMethod() != api.AuthMethodADC {
		t.Errorf("AuthMethod() = %v, want %v", p.AuthMethod(), api.AuthMethodADC)
	}
}

func TestNewClientMissingProject(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_REGION", "")
	p := New()
	_, err := p.NewClient(api.ProviderConfig{Model: "claude-sonnet-4-20250514"})
	if err == nil {
		t.Fatalf("expected error when GOOGLE_CLOUD_PROJECT is empty")
	}
	if !strings.Contains(err.Error(), "GOOGLE_CLOUD_PROJECT") {
		t.Errorf("expected error to mention GOOGLE_CLOUD_PROJECT, got %v", err)
	}
}

func TestEndpointURL(t *testing.T) {
	c := &Client{
		Project: "my-proj",
		Region:  "us-east5",
		Model:   "claude-sonnet-4@20250514",
	}
	got := c.endpoint()
	want := "https://us-east5-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-east5/publishers/anthropic/models/claude-sonnet-4@20250514:streamRawPredict"
	if got != want {
		t.Errorf("endpoint:\n got: %s\nwant: %s", got, want)
	}

	// Override base URL
	c.BaseURL = "http://localhost:9999"
	got = c.endpoint()
	if !strings.HasPrefix(got, "http://localhost:9999/v1/projects/my-proj/locations/us-east5/") {
		t.Errorf("BaseURL override not honoured: %s", got)
	}
}

func TestBuildRequest(t *testing.T) {
	c := &Client{Project: "p", Region: "us-east5", Model: "claude-sonnet-4@20250514", MaxTokens: 0}
	temp := 0.5
	body, err := c.buildRequest(api.CreateMessageRequest{
		MaxTokens:   1024,
		System:      "You are a test.",
		Messages:    []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var version string
	_ = json.Unmarshal(parsed["anthropic_version"], &version)
	if version != anthropicVertexVersion {
		t.Errorf("anthropic_version = %q, want %q", version, anthropicVertexVersion)
	}
	if _, ok := parsed["model"]; ok {
		t.Errorf("vertex body must NOT contain a model field (it is in the URL path)")
	}
	var maxToks int
	_ = json.Unmarshal(parsed["max_tokens"], &maxToks)
	if maxToks != 1024 {
		t.Errorf("max_tokens = %d, want 1024", maxToks)
	}
	var stream bool
	_ = json.Unmarshal(parsed["stream"], &stream)
	if !stream {
		t.Errorf("stream should be true")
	}
}

// TestStreamResponseError verifies that non-2xx responses surface as APIError.
func TestStreamResponseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`))
	}))
	defer srv.Close()

	c := &Client{
		Project:     "test-proj",
		Region:      "us-east5",
		Model:       "claude-sonnet-4@20250514",
		BaseURL:     srv.URL,
		HTTPClient:  srv.Client(),
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "stub", Expiry: time.Now().Add(time.Hour)}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.StreamResponse(ctx, api.CreateMessageRequest{
		MaxTokens: 16,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatalf("expected error from 429 response")
	}
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.APIError, got %T: %v", err, err)
	}
	if apiErr.Provider != "vertex" {
		t.Errorf("Provider = %q, want vertex", apiErr.Provider)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if !apiErr.Retryable {
		t.Errorf("429 should be retryable")
	}
	if !strings.Contains(apiErr.Message, "quota exceeded") {
		t.Errorf("Message = %q, want to contain 'quota exceeded'", apiErr.Message)
	}
}

// TestStreamResponseLive is a smoke test against the real Vertex AI service.
// It is skipped unless RUN_LIVE_VERTEX=1 and GOOGLE_CLOUD_PROJECT are set, and
// is also skipped under `go test -short`.
func TestStreamResponseLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in -short mode")
	}
	if os.Getenv("RUN_LIVE_VERTEX") != "1" {
		t.Skip("set RUN_LIVE_VERTEX=1 to run the live Vertex smoke test")
	}
	if os.Getenv("GOOGLE_CLOUD_PROJECT") == "" {
		t.Skip("GOOGLE_CLOUD_PROJECT not set")
	}
	p := New()
	model := os.Getenv("VERTEX_MODEL")
	if model == "" {
		model = "claude-sonnet-4@20250514"
	}
	clientI, err := p.NewClient(api.ProviderConfig{Model: model, MaxTokens: 64})
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
