package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDefaultUserAgent_HonestClawIdentity(t *testing.T) {
	ua := DefaultUserAgent()
	if !strings.HasPrefix(ua, "claw-code-go/") {
		t.Errorf("DefaultUserAgent() = %q, want claw-code-go/<version>", ua)
	}
	if strings.TrimPrefix(ua, "claw-code-go/") == "" {
		t.Errorf("DefaultUserAgent() = %q, version part is empty", ua)
	}
}

func TestResolveIdentity_UserAgentPrecedence(t *testing.T) {
	t.Setenv(EnvCustomHeaders, "")
	resolve := func(explicit, fallback string) string {
		t.Helper()
		id, err := ResolveIdentity(explicit, fallback, nil)
		if err != nil {
			t.Fatalf("ResolveIdentity error: %v", err)
		}
		return id.UserAgent
	}

	t.Setenv(EnvUserAgent, "")
	if got := resolve("", DefaultUserAgent()); got != DefaultUserAgent() {
		t.Errorf("no override: got %q, want default %q", got, DefaultUserAgent())
	}
	if got := resolve("", "fallback/3.0"); got != "fallback/3.0" {
		t.Errorf("custom fallback: got %q, want fallback/3.0", got)
	}

	t.Setenv(EnvUserAgent, "env-agent/1.0")
	if got := resolve("", DefaultUserAgent()); got != "env-agent/1.0" {
		t.Errorf("env override: got %q, want env-agent/1.0", got)
	}
	if got := resolve("explicit-agent/2.0", DefaultUserAgent()); got != "explicit-agent/2.0" {
		t.Errorf("explicit override: got %q, want explicit-agent/2.0 (explicit wins over env)", got)
	}
	if got := resolve("", "fallback/3.0"); got != "env-agent/1.0" {
		t.Errorf("env beats custom fallback: got %q", got)
	}
}

func TestParseCustomHeaders(t *testing.T) {
	headers, err := ParseCustomHeaders("User-Agent: my-tool/1.0\r\n\n  X-Custom : some: value \n")
	if err != nil {
		t.Fatalf("ParseCustomHeaders error: %v", err)
	}
	if got := headers["User-Agent"]; got != "my-tool/1.0" {
		t.Errorf("User-Agent = %q", got)
	}
	// Value is cut at the FIRST colon; the rest (including further colons) is the value.
	if got := headers["X-Custom"]; got != "some: value" {
		t.Errorf("X-Custom = %q", got)
	}

	if h, err := ParseCustomHeaders("   \n\n"); err != nil || h != nil {
		t.Errorf("blank input: got (%v, %v), want (nil, nil)", h, err)
	}

	for _, malformed := range []string{"no-colon-line", ": value-without-name"} {
		if _, err := ParseCustomHeaders(malformed); err == nil {
			t.Errorf("ParseCustomHeaders(%q): expected explicit error, got nil", malformed)
		}
	}
}

func TestResolveIdentity_ExtraHeadersExplicitWinsOverEnv(t *testing.T) {
	t.Setenv(EnvUserAgent, "")
	t.Setenv(EnvCustomHeaders, "X-From-Env: env\nX-Shared: env")
	id, err := ResolveIdentity("", DefaultUserAgent(), map[string]string{"X-Shared": "explicit", "X-From-Cfg": "cfg"})
	if err != nil {
		t.Fatalf("ResolveIdentity error: %v", err)
	}
	want := map[string]string{"X-From-Env": "env", "X-Shared": "explicit", "X-From-Cfg": "cfg"}
	for k, v := range want {
		if id.ExtraHeaders[k] != v {
			t.Errorf("ExtraHeaders[%q] = %q, want %q", k, id.ExtraHeaders[k], v)
		}
	}

	t.Setenv(EnvCustomHeaders, "malformed-line-without-colon")
	if _, err := ResolveIdentity("", DefaultUserAgent(), nil); err == nil {
		t.Error("malformed env: expected explicit error, got nil")
	}
}

// streamIdentityRequest runs one StreamResponse call against a stub server and
// returns the headers the anthropic client actually sent.
func streamIdentityRequest(t *testing.T, configure func(*Client)) http.Header {
	t.Helper()
	var captured http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(ssePayload("message_stop", `{"type":"message_stop"}`)))
	}))
	defer ts.Close()

	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL
	if configure != nil {
		configure(client)
	}
	ch, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}
	collectEvents(ch, 5*time.Second)
	return captured
}

func TestStreamResponse_SendsIdentityHeaders(t *testing.T) {
	t.Setenv(EnvUserAgent, "")
	t.Setenv(EnvCustomHeaders, "")

	// Default: honest claw identity (previously Go-http-client leaked).
	h := streamIdentityRequest(t, nil)
	if got := h.Get("User-Agent"); got != DefaultUserAgent() {
		t.Errorf("default User-Agent = %q, want %q", got, DefaultUserAgent())
	}

	// Explicit Client.UserAgent override.
	h = streamIdentityRequest(t, func(c *Client) { c.UserAgent = "my-tool/9.9" })
	if got := h.Get("User-Agent"); got != "my-tool/9.9" {
		t.Errorf("override User-Agent = %q, want my-tool/9.9", got)
	}

	// CLAW_USER_AGENT env override.
	t.Setenv(EnvUserAgent, "env-tool/1.2")
	h = streamIdentityRequest(t, nil)
	if got := h.Get("User-Agent"); got != "env-tool/1.2" {
		t.Errorf("env User-Agent = %q, want env-tool/1.2", got)
	}

	// ANTHROPIC_CUSTOM_HEADERS is applied last: overrides the UA and any
	// default header, and adds arbitrary headers — Claude Code parity.
	t.Setenv(EnvCustomHeaders, "User-Agent: custom-headers-tool/3.0\nX-Team: iterion")
	h = streamIdentityRequest(t, nil)
	if got := h.Get("User-Agent"); got != "custom-headers-tool/3.0" {
		t.Errorf("custom-headers User-Agent = %q, want custom-headers-tool/3.0", got)
	}
	if got := h.Get("X-Team"); got != "iterion" {
		t.Errorf("X-Team = %q, want iterion", got)
	}

	// A malformed ANTHROPIC_CUSTOM_HEADERS is an explicit request error.
	t.Setenv(EnvCustomHeaders, "garbage")
	client := NewClient("sk-test", "claude-test")
	if _, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	}); err == nil || !strings.Contains(err.Error(), EnvCustomHeaders) {
		t.Errorf("malformed %s: got err %v, want explicit parse error", EnvCustomHeaders, err)
	}
}
