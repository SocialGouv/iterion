package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// resolveAndCaptureUA resolves the built-in anthropic factory against a stub
// server and returns the User-Agent the resulting claw client actually sent.
func resolveAndCaptureUA(t *testing.T) string {
	t.Helper()
	uaCh := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case uaCh <- r.Header.Get("User-Agent"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer ts.Close()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", ts.URL)

	client, err := NewRegistry().Resolve("anthropic/claude-test")
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	ch, err := client.StreamResponse(context.Background(), api.CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}
	for range ch { // drain so the stream goroutine finishes
	}
	select {
	case ua := <-uaCh:
		return ua
	case <-time.After(5 * time.Second):
		t.Fatal("stub server never received a request")
		return ""
	}
}

// TestRegistry_ClientIdentity verifies the iterion-side plumbing: the default
// honest claw User-Agent reaches the wire, and ITERION_LLM_USER_AGENT
// overrides it (docs/backends.md § Client identity).
func TestRegistry_ClientIdentity(t *testing.T) {
	t.Setenv("ITERION_LLM_USER_AGENT", "")
	t.Setenv("CLAW_USER_AGENT", "")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	if ua := resolveAndCaptureUA(t); !strings.HasPrefix(ua, "claw-code-go/") {
		t.Errorf("default User-Agent = %q, want claw-code-go/<version>", ua)
	}

	t.Setenv("ITERION_LLM_USER_AGENT", "iterion-test-agent/1.0")
	if ua := resolveAndCaptureUA(t); ua != "iterion-test-agent/1.0" {
		t.Errorf("ITERION_LLM_USER_AGENT override: User-Agent = %q, want iterion-test-agent/1.0", ua)
	}
}
