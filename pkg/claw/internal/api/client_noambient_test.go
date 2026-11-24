package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The ambient header block the operator's environment carries stays home
// when the endpoint is not the operator's choice: under NoAmbientHeaders the
// wire carries only the caller's own auth, its explicit headers and claw's
// honest identity. Without the flag the block still applies (control) — the
// same two-direction witness the OpenAI provider carries, here on the client
// every Anthropic-shaped provider builds.
func TestClientNoAmbientHeadersKeepsAmbientIdentityHome(t *testing.T) {
	t.Setenv(EnvCustomHeaders, "X-Operator-Secret: s3cr3t")
	t.Setenv(EnvUserAgent, "operator-agent/1")
	for _, noAmbient := range []bool{true, false} {
		t.Run(fmt.Sprintf("noAmbient=%t", noAmbient), func(t *testing.T) {
			var got http.Header
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				body := strings.Join([]string{
					ssePayload("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
					ssePayload("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
					ssePayload("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
					ssePayload("message_stop", `{"type":"message_stop"}`),
				}, "")
				io.WriteString(w, body)
			}))
			defer ts.Close()

			c := NewClient("sk-test", "claude-test")
			c.BaseURL = ts.URL
			c.NoAmbientHeaders = noAmbient
			c.ExtraHeaders = map[string]string{"X-Explicit": "yes"}
			ch, err := c.StreamResponse(t.Context(), CreateMessageRequest{
				Model:     "claude-test",
				MaxTokens: 16,
				Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
			})
			if err != nil {
				t.Fatalf("StreamResponse: %v", err)
			}
			for ev := range ch {
				if ev.Type == EventError {
					t.Fatalf("stream error: %s", ev.ErrorMessage)
				}
			}

			if got.Get("X-Explicit") != "yes" {
				t.Errorf("explicit header lost: %q", got.Get("X-Explicit"))
			}
			if noAmbient {
				if secret := got.Get("X-Operator-Secret"); secret != "" {
					t.Errorf("ambient credential header reached the endpoint: X-Operator-Secret=%q", secret)
				}
				if ua := got.Get("User-Agent"); ua == "operator-agent/1" {
					t.Errorf("ambient CLAW_USER_AGENT reached the endpoint: User-Agent=%q", ua)
				}
				if key := got.Get("X-Api-Key"); key != "sk-test" {
					t.Errorf("the client's own auth was withheld: X-Api-Key=%q", key)
				}
			} else {
				if got.Get("X-Operator-Secret") == "" && got.Get("User-Agent") != "operator-agent/1" {
					t.Errorf("control: the ambient block should still apply without the flag (headers %v)", got)
				}
			}
		})
	}
}

// ResolveIdentityFlagged is the one reader the flag's decision goes through:
// flagged, the environment is never read; unflagged, it merges under the
// explicit config, explicit winning on collision.
func TestResolveIdentityFlagged(t *testing.T) {
	t.Setenv(EnvCustomHeaders, "X-Operator-Secret: s3cr3t\nUser-Agent: ambient-ua/1")
	t.Setenv(EnvUserAgent, "operator-agent/1")
	explicit := map[string]string{"X-Explicit": "yes", "User-Agent": "explicit-ua/1"}

	flagged, err := ResolveIdentityFlagged(true, "", "fallback/1", explicit)
	if err != nil {
		t.Fatalf("flagged resolve: %v", err)
	}
	if _, ok := flagged.ExtraHeaders["X-Operator-Secret"]; ok {
		t.Errorf("flagged resolve read the ambient block: %v", flagged.ExtraHeaders)
	}
	if flagged.UserAgent != "fallback/1" {
		t.Errorf("flagged resolve read CLAW_USER_AGENT: User-Agent=%q", flagged.UserAgent)
	}
	if flagged.ExtraHeaders["X-Explicit"] != "yes" {
		t.Errorf("explicit header lost under the flag: %v", flagged.ExtraHeaders)
	}

	merged, err := ResolveIdentityFlagged(false, "", "fallback/1", explicit)
	if err != nil {
		t.Fatalf("unflagged resolve: %v", err)
	}
	if merged.ExtraHeaders["X-Operator-Secret"] != "s3cr3t" {
		t.Errorf("control: the ambient block should merge without the flag: %v", merged.ExtraHeaders)
	}
	if merged.UserAgent != "operator-agent/1" {
		t.Errorf("control: CLAW_USER_AGENT should win without an explicit UA, got %q", merged.UserAgent)
	}
	if merged.ExtraHeaders["User-Agent"] != "explicit-ua/1" {
		t.Errorf("explicit config should win the collision, got %q", merged.ExtraHeaders["User-Agent"])
	}
}
