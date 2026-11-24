package providers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/anthropic"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/bedrock"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/openai"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/zai"
)

// requestEffort reads the reasoning effort a request body carries, in the
// field its wire uses: Anthropic Messages output_config.effort, OpenAI chat
// reasoning_effort, OpenAI responses reasoning.effort.
func requestEffort(body map[string]any) string {
	if oc, ok := body["output_config"].(map[string]any); ok {
		e, _ := oc["effort"].(string)
		return e
	}
	if e, ok := body["reasoning_effort"].(string); ok {
		return e
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		e, _ := r["effort"].(string)
		return e
	}
	return ""
}

// TestEachClientReportsTheEffortItsRequestCarries sends one request through
// each client that answers api.EffortWire and compares the effort its body
// carried with the client's own answer: the report the claw backend folds
// into a node's record is the request's, model by model — a Claude model on
// the Anthropic Messages wire carries the level, a GLM id on the same wire
// (anthropic, z.ai) carries none, OpenAI's chat and responses wires carry
// it as named, and bedrock's body has no effort field at all.
//
// Mutation that reddens it: make anthropicWireEffort (internal/api/client.go)
// answer the level whatever the model, or bedrock's WireReasoningEffort
// answer the level.
func TestEachClientReportsTheEffortItsRequestCarries(t *testing.T) {
	const (
		anthropicStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		chatStream      = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		responsesStream = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	)
	for _, tc := range []struct {
		name     string
		provider api.Provider
		wire     string
		model    string
		stream   string
		want     string
	}{
		{"anthropic, a Claude model", anthropic.New(), "", "claude-opus-5-5", anthropicStream, "high"},
		{"anthropic, a GLM id", anthropic.New(), "", "glm-5.3", anthropicStream, ""},
		{"zai, a GLM id", zai.New(), "", "glm-5.3", anthropicStream, ""},
		{"openai chat", openai.New(), api.OpenAIWireChat, "gpt-6-astra", chatStream, "high"},
		{"openai responses", openai.New(), api.OpenAIWireResponses, "gpt-6-astra", responsesStream, "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := make(chan map[string]any, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode the request: %v", err)
				}
				bodies <- body
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.stream)
			}))
			defer srv.Close()
			client, err := tc.provider.NewClient(api.ProviderConfig{
				APIKey: "test-key", Model: tc.model, BaseURL: srv.URL, NoAmbientHeaders: true, OpenAIWireAPI: tc.wire,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := api.CreateMessageRequest{
				Model: tc.model, MaxTokens: 16, ReasoningEffort: "high",
				Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
			}
			events, err := client.StreamResponse(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			sent := requestEffort(<-bodies)
			reporter, ok := client.(api.EffortWire)
			if !ok {
				t.Fatalf("%T does not answer api.EffortWire", client)
			}
			if got := reporter.WireReasoningEffort(tc.model, "high"); got != sent || sent != tc.want {
				t.Errorf("the request carried %q, the client reports %q, want both %q", sent, got, tc.want)
			}
		})
	}

	t.Run("bedrock", func(t *testing.T) {
		body, err := bedrock.MarshalRequest(api.CreateMessageRequest{
			Model: "anthropic.claude-opus-5-5", MaxTokens: 16, ReasoningEffort: "high",
			Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatal(err)
		}
		sent := requestEffort(parsed)
		if got := (&bedrock.Client{}).WireReasoningEffort("anthropic.claude-opus-5-5", "high"); got != sent || sent != "" {
			t.Errorf("the body carried %q, the client reports %q, want both empty", sent, got)
		}
	})
}
