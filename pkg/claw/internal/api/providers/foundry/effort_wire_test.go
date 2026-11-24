package foundry

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// The foundry body carries the effort as named, and the client says so
// (api.EffortWire).
func TestFoundrySendsTheEffortAsNamedAndSaysSo(t *testing.T) {
	c := &Client{Endpoint: "https://x", Deployment: "d", APIVersion: defaultAPIVersion}
	body, err := c.buildRequest(api.CreateMessageRequest{
		MaxTokens: 16, ReasoningEffort: "high",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := c.WireReasoningEffort("d", "high"); parsed.ReasoningEffort != "high" || got != parsed.ReasoningEffort {
		t.Errorf("the body carries %q, the client reports %q, want both high", parsed.ReasoningEffort, got)
	}
}
