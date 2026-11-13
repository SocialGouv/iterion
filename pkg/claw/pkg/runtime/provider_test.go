package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	pkgrt "github.com/SocialGouv/iterion/pkg/claw/pkg/runtime"
)

func TestSelectProviderAllNames(t *testing.T) {
	names := []struct {
		input    string
		wantName string
	}{
		{"anthropic", "anthropic"},
		{"openai", "openai"},
		{"xai", "openai"},       // xai uses openai provider
		{"dashscope", "openai"}, // dashscope uses openai provider
		{"zai", "zai"},
		{"moonshot", "moonshot"},
		{"bedrock", "bedrock"},
		{"vertex", "vertex"},
		{"foundry", "foundry"},
	}
	for _, tc := range names {
		p := pkgrt.SelectProvider(tc.input)
		if p == nil {
			t.Fatalf("SelectProvider(%q) returned nil", tc.input)
		}
		if got := p.Name(); got != tc.wantName {
			t.Errorf("SelectProvider(%q).Name() = %q, want %q", tc.input, got, tc.wantName)
		}
	}
}

func TestSelectProviderDefault(t *testing.T) {
	p := pkgrt.SelectProvider("")
	if p == nil {
		t.Fatal("SelectProvider(\"\") returned nil")
	}
	if got := p.Name(); got != "anthropic" {
		t.Errorf("default provider = %q, want anthropic", got)
	}
}

func TestNewNoAuthClient(t *testing.T) {
	c := pkgrt.NewNoAuthClient()
	if c == nil {
		t.Fatal("NewNoAuthClient returned nil")
	}
	// StreamResponse should return an error.
	_, err := c.StreamResponse(context.Background(), api.CreateMessageRequest{})
	if err == nil {
		t.Error("expected error from NoAuthClient")
	}
}

func TestPublicProviderConfigForwardsChatGPTAccountID(t *testing.T) {
	account := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account = r.Header.Get("ChatGPT-Account-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	}))
	defer srv.Close()
	client, err := pkgrt.NewProviderClient(&pkgrt.ProviderConfig{
		ProviderName: "openai", OAuthToken: "test-token", OpenAIChatGPTAccountID: "test-account",
		BaseURL: srv.URL, Model: "gpt-5.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := client.StreamResponse(context.Background(), api.CreateMessageRequest{System: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if account != "test-account" {
		t.Fatalf("account header = %q", account)
	}
}
