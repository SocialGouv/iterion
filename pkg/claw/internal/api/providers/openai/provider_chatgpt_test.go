package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// TestNewClient_ChatGPTOAuthMode covers the AuthMode discrimination in
// NewClient: providing (OAuthToken + ChatGPT account_id) flips the client
// onto the ChatGPT-Codex backend with the right base URL; missing either
// keeps the legacy API-key flow.
func TestNewClient_ChatGPTOAuthMode(t *testing.T) {
	tests := []struct {
		name        string
		cfg         api.ProviderConfig
		wantMode    AuthMode
		wantBaseURL string
		wantErr     bool
	}{
		{
			name: "oauth + account id → chatgpt mode + codex base url",
			cfg: api.ProviderConfig{
				OAuthToken:             "oauth-tok",
				OpenAIChatGPTAccountID: "acct-1",
				Model:                  "gpt-5.5",
			},
			wantMode:    AuthModeChatGPTOAuth,
			wantBaseURL: chatgptCodexBaseURL,
		},
		{
			name: "api key only → api_key mode + default openai base url",
			cfg: api.ProviderConfig{
				APIKey: "sk-test",
				Model:  "gpt-5.5",
			},
			wantMode:    AuthModeAPIKey,
			wantBaseURL: defaultBaseURL,
		},
		{
			name: "oauth without account id falls back to api key path (rejected when no key)",
			cfg: api.ProviderConfig{
				OAuthToken: "oauth-tok",
				Model:      "gpt-5.5",
			},
			wantErr: true,
		},
		{
			name: "no credentials → error",
			cfg: api.ProviderConfig{
				Model: "gpt-5.5",
			},
			wantErr: true,
		},
		{
			name: "explicit BaseURL overrides default in chatgpt mode",
			cfg: api.ProviderConfig{
				OAuthToken:             "oauth-tok",
				OpenAIChatGPTAccountID: "acct-1",
				BaseURL:                "https://chatgpt.example.test/backend-api/codex",
				Model:                  "gpt-5.5",
			},
			wantMode:    AuthModeChatGPTOAuth,
			wantBaseURL: "https://chatgpt.example.test/backend-api/codex",
		},
	}

	p := New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := p.NewClient(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got client=%v", client)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			c := client.(*Client)
			if c.AuthMode != tt.wantMode {
				t.Errorf("AuthMode = %q, want %q", c.AuthMode, tt.wantMode)
			}
			if c.BaseURL != tt.wantBaseURL {
				t.Errorf("BaseURL = %q, want %q", c.BaseURL, tt.wantBaseURL)
			}
			if tt.wantMode == AuthModeChatGPTOAuth {
				if c.OAuthToken != tt.cfg.OAuthToken {
					t.Errorf("OAuthToken not copied through")
				}
				if c.ChatGPTAccountID != tt.cfg.OpenAIChatGPTAccountID {
					t.Errorf("ChatGPTAccountID not copied through")
				}
				if c.ClientVersion == "" {
					t.Error("ClientVersion should default to a non-empty value")
				}
			}
		})
	}
}

// TestNewClient_ChatGPTOAuthDefaultsToBakedVersion pins the identity claw
// presents when the caller cannot name a Codex CLI version: the baked
// release, on both the version header source and the User-Agent.
func TestNewClient_ChatGPTOAuthDefaultsToBakedVersion(t *testing.T) {
	client, err := New().NewClient(api.ProviderConfig{
		OAuthToken:             "oauth-tok",
		OpenAIChatGPTAccountID: "acct-1",
		Model:                  "gpt-5.5",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c := client.(*Client)
	if c.ClientVersion != api.ChatGPTClientVersion {
		t.Errorf("ClientVersion = %q, want the baked %q", c.ClientVersion, api.ChatGPTClientVersion)
	}
	if want := chatgptOriginator + "/" + api.ChatGPTClientVersion; c.Identity.UserAgent != want {
		t.Errorf("User-Agent = %q, want %q", c.Identity.UserAgent, want)
	}
}

// TestSetAuthHeaders_ChatGPTOAuth verifies that in OAuth mode the four
// masquerading headers (Authorization, ChatGPT-Account-ID, originator,
// version) plus User-Agent are written to the outgoing request.
func TestSetAuthHeaders_ChatGPTOAuth(t *testing.T) {
	c := &Client{
		AuthMode:         AuthModeChatGPTOAuth,
		OAuthToken:       "tok-123",
		ChatGPTAccountID: "acct-abc",
		ClientVersion:    "0.130.0",
		// as resolved by NewClient in OAuth mode
		Identity: api.Identity{UserAgent: "codex_cli_rs/0.130.0"},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	c.setAuthHeaders(req)
	c.Identity.Apply(req.Header)

	cases := map[string]string{
		"Authorization":      "Bearer tok-123",
		"ChatGPT-Account-ID": "acct-abc",
		"originator":         "codex_cli_rs",
		"version":            "0.130.0",
		"User-Agent":         "codex_cli_rs/0.130.0",
	}
	for header, want := range cases {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// TestSetAuthHeaders_APIKey verifies that in API-key mode we send only the
// Bearer header and none of the ChatGPT-specific masquerading headers.
func TestSetAuthHeaders_APIKey(t *testing.T) {
	c := &Client{
		AuthMode: AuthModeAPIKey,
		APIKey:   "sk-test",
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	c.setAuthHeaders(req)

	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-test")
	}
	for _, leaked := range []string{"ChatGPT-Account-ID", "originator", "version"} {
		if got := req.Header.Get(leaked); got != "" {
			t.Errorf("%s leaked in api_key mode: %q", leaked, got)
		}
	}
}

// TestResponsesEndpoint_PathPerMode verifies the URL suffix differs between
// the canonical OpenAI endpoint (/v1/responses) and the ChatGPT-Codex
// backend (/responses, since the base URL already includes the /codex path).
func TestResponsesEndpoint_PathPerMode(t *testing.T) {
	cases := []struct {
		mode    AuthMode
		baseURL string
		want    string
	}{
		{AuthModeAPIKey, "https://api.openai.com", "https://api.openai.com/v1/responses"},
		{AuthModeChatGPTOAuth, chatgptCodexBaseURL, chatgptCodexBaseURL + "/responses"},
	}
	for _, tt := range cases {
		t.Run(string(tt.mode), func(t *testing.T) {
			c := &Client{AuthMode: tt.mode, BaseURL: tt.baseURL}
			if got := c.responsesEndpoint(); got != tt.want {
				t.Errorf("responsesEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStreamResponse_ChatGPTOAuthForcesResponsesPath fires a real streaming
// request through a test server and verifies that ChatGPT-OAuth mode always
// reaches /responses (never /v1/chat/completions), with the masquerading
// headers and instructions fallback applied.
func TestStreamResponse_ChatGPTOAuthForcesResponsesPath(t *testing.T) {
	var (
		receivedPath string
		receivedAuth string
		receivedAcct string
		receivedOrig string
		receivedVer  string
		receivedBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		receivedAcct = r.Header.Get("ChatGPT-Account-ID")
		receivedOrig = r.Header.Get("originator")
		receivedVer = r.Header.Get("version")
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Minimal valid SSE: response.created → response.completed.
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"))
	}))
	defer srv.Close()

	client, err := New().NewClient(api.ProviderConfig{
		OAuthToken:             "oauth-tok",
		OpenAIChatGPTAccountID: "acct-1",
		BaseURL:                srv.URL,
		Model:                  "gpt-5.5",
		OpenAIClientVersion:    "0.130.0",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// No ReasoningEffort, no Tools — would route to chat/completions in
	// API-key mode. ChatGPT-OAuth must still go to /responses.
	ch, err := client.StreamResponse(context.Background(), api.CreateMessageRequest{
		Model:    "gpt-5.5",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		// System left blank to exercise the instructions fallback.
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	for range ch {
		// drain
	}

	if receivedPath != "/responses" {
		t.Errorf("server saw path = %q, want %q", receivedPath, "/responses")
	}
	if receivedAuth != "Bearer oauth-tok" {
		t.Errorf("Authorization = %q", receivedAuth)
	}
	if receivedAcct != "acct-1" {
		t.Errorf("ChatGPT-Account-ID = %q", receivedAcct)
	}
	if receivedOrig != "codex_cli_rs" {
		t.Errorf("originator = %q", receivedOrig)
	}
	if receivedVer != "0.130.0" {
		t.Errorf("version = %q", receivedVer)
	}

	// Instructions fallback must populate a non-empty system message.
	var body struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(receivedBody, &body); err != nil {
		t.Fatalf("decode body: %v (raw=%q)", err, string(receivedBody))
	}
	if strings.TrimSpace(body.Instructions) == "" {
		t.Errorf("instructions must be non-empty in ChatGPT-OAuth mode (fallback failed)")
	}
}
