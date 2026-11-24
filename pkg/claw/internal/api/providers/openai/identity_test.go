package openai

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// TestNewClient_UserAgentResolution verifies the per-mode identity defaults
// and the override precedence (cfg.UserAgent > CLAW_USER_AGENT > mode default).
func TestNewClient_UserAgentResolution(t *testing.T) {
	t.Setenv(api.EnvUserAgent, "")
	t.Setenv(api.EnvCustomHeaders, "")

	newClient := func(t *testing.T, cfg api.ProviderConfig) *Client {
		t.Helper()
		c, err := New().NewClient(cfg)
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		return c.(*Client)
	}

	// API-key mode defaults to the honest claw identity.
	c := newClient(t, api.ProviderConfig{APIKey: "sk-test"})
	if !strings.HasPrefix(c.Identity.UserAgent, "claw-code-go/") {
		t.Errorf("api-key mode UserAgent = %q, want claw-code-go/<version>", c.Identity.UserAgent)
	}

	// ChatGPT-OAuth mode defaults to the codex identity the backend requires.
	oauthCfg := api.ProviderConfig{
		OAuthToken:             "tok",
		OpenAIChatGPTAccountID: "acct",
		OpenAIClientVersion:    "0.131.0",
	}
	c = newClient(t, oauthCfg)
	if c.Identity.UserAgent != "codex_cli_rs/0.131.0" {
		t.Errorf("oauth mode UserAgent = %q, want codex_cli_rs/0.131.0", c.Identity.UserAgent)
	}

	// Explicit override wins in both modes (operator decision).
	oauthCfg.UserAgent = "my-tool/1.0"
	if c = newClient(t, oauthCfg); c.Identity.UserAgent != "my-tool/1.0" {
		t.Errorf("oauth explicit override UserAgent = %q, want my-tool/1.0", c.Identity.UserAgent)
	}

	// Env override beats the mode default.
	t.Setenv(api.EnvUserAgent, "env-tool/2.0")
	c = newClient(t, api.ProviderConfig{APIKey: "sk-test"})
	if c.Identity.UserAgent != "env-tool/2.0" {
		t.Errorf("env override UserAgent = %q, want env-tool/2.0", c.Identity.UserAgent)
	}
}

// TestIdentityApply_ExtraHeadersLast verifies extra headers are able to
// override the resolved User-Agent (ANTHROPIC_CUSTOM_HEADERS parity).
func TestIdentityApply_ExtraHeadersLast(t *testing.T) {
	c := &Client{
		Identity: api.Identity{
			UserAgent:    "resolved/1.0",
			ExtraHeaders: map[string]string{"User-Agent": "extra/2.0", "X-Team": "iterion"},
		},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example/", nil)
	c.Identity.Apply(req.Header)
	if got := req.Header.Get("User-Agent"); got != "extra/2.0" {
		t.Errorf("User-Agent = %q, want extra/2.0 (extra headers win)", got)
	}
	if got := req.Header.Get("X-Team"); got != "iterion" {
		t.Errorf("X-Team = %q, want iterion", got)
	}
}
