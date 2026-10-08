package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// TestOpenAIStreamUsageKnob drives the openai/ factories, not claw directly:
// a gateway answering chat completions records every request body it
// receives, so the wire carries (or not) stream_options.include_usage.
// Unset the knob and the wire must stay exactly what it is today — providers
// that reject stream_options must never see it appear unasked.
//
// Both constructors that forward OPENAI_BASE_URL carry the field: the env
// factory (registry.go's r.providers["openai"]) and the BYOK one
// (r.providersWithKey["openai"], the path a run's own key takes). They hold
// separate copies of the config, so each group below must redden on its own
// under a mute of its own site.
func TestOpenAIStreamUsageKnob(t *testing.T) {
	// Neutralise the credential lookups so the env factory resolves through
	// its env path (OPENAI_API_KEY), never the ctx codex forfait.
	SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
		return func(string) string { return "" }, true
	})
	SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
		return func(string) string { return "" }, true
	})
	t.Cleanup(func() {
		SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false })
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	})

	cases := []struct {
		name      string
		knob      string
		wantUsage bool
	}{
		{"knob unset keeps today's wire", "", false},
		{"knob=1 requests include_usage", "1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A registry caches its resolved clients for its lifetime, so
			// each subtest resolves through a fresh one.
			reg := NewRegistry()
			assertOpenAIStreamUsageWire(t, tc.knob, tc.wantUsage, func() (api.APIClient, error) {
				return reg.ResolveWithContext(context.Background(), "openai/usage-knob-probe")
			})
		})
	}
	// The BYOK twin: invoked with the run's own key, so ResolveWithContext's
	// credential lookups are out of the picture entirely.
	for _, tc := range cases {
		t.Run("byok: "+tc.name, func(t *testing.T) {
			reg := NewRegistry()
			assertOpenAIStreamUsageWire(t, tc.knob, tc.wantUsage, func() (api.APIClient, error) {
				return reg.providersWithKey["openai"]("usage-knob-probe", "test-key")
			})
		})
	}
}

// assertOpenAIStreamUsageWire streams one request against a recording
// gateway and asserts the wire: with the knob set the request must ask for
// stream_options.include_usage, unset it must carry no stream_options at
// all. client is invoked after the env is pinned.
func assertOpenAIStreamUsageWire(t *testing.T, knob string, wantUsage bool, client func() (api.APIClient, error)) {
	t.Helper()

	var mu sync.Mutex
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		mu.Lock()
		raw = body
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
	t.Setenv("ITERION_OPENAI_STREAM_USAGE", knob)

	c, err := client()
	if err != nil {
		t.Fatalf("resolve openai client: %v", err)
	}
	ch, err := c.StreamResponse(context.Background(), api.CreateMessageRequest{
		Model:     "usage-knob-probe",
		MaxTokens: 16,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "ping"}}}},
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	for ev := range ch {
		if ev.Type == api.EventError {
			t.Fatalf("stream error: %s", ev.ErrorMessage)
		}
	}

	mu.Lock()
	sent := raw
	mu.Unlock()
	var wire struct {
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(sent, &wire); err != nil {
		t.Fatalf("decode captured request body: %v", err)
	}
	if !wantUsage {
		if wire.StreamOptions != nil {
			t.Errorf("knob unset: request carries stream_options %+v; the default wire must stay exact", *wire.StreamOptions)
		}
		return
	}
	if wire.StreamOptions == nil || !wire.StreamOptions.IncludeUsage {
		t.Errorf("knob=1: request carries no stream_options.include_usage — gateway token usage stays unreported")
	}
}
