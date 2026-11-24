package vertex

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// The vertex body has no effort field, and the client says so
// (api.EffortWire): a node's record never claims a level vertex did not send.
func TestVertexSendsNoEffortAndSaysSo(t *testing.T) {
	c := &Client{Project: "p", Region: "us-east5", Model: "claude-opus-5-5@20260101"}
	body, err := c.buildRequest(api.CreateMessageRequest{
		MaxTokens: 16, ReasoningEffort: "high",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"output_config", "reasoning_effort", "reasoning", "thinking"} {
		if _, ok := parsed[field]; ok {
			t.Errorf("the body carries %q: %s", field, body)
		}
	}
	if got := c.WireReasoningEffort(c.Model, "high"); got != "" {
		t.Errorf("WireReasoningEffort = %q, want empty: the body carries no effort", got)
	}
}
