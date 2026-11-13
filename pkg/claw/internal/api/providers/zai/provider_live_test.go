package zai

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// The falsifier for the routing claim itself: a REAL call through the zai
// provider — real endpoint, real account key, glm-5.3 served. Guarded by
// ZAI_LIVE_PROBE=1 so CI never spends the account; run it before pushing:
//
//	ZAI_LIVE_PROBE=1 go test -run TestLiveZai -v ./internal/api/providers/zai/
//
// What it proves (and what a stub cannot): the wire shape z.ai actually
// accepts — header, path, model id — for the exact pair this provider
// exists to route. A stub proves the provider calls its own client; only
// the live endpoint proves z.ai answers "glm-5.3" on this wire.
func TestLiveZaiStream(t *testing.T) {
	key := os.Getenv("ZAI_API_KEY")
	if key == "" || os.Getenv("ZAI_LIVE_PROBE") == "" {
		t.Skip("set ZAI_API_KEY + ZAI_LIVE_PROBE=1 to run the live probe")
	}
	c, err := New().NewClient(api.ProviderConfig{APIKey: key, Model: "glm-5.3"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := c.StreamResponse(ctx, api.CreateMessageRequest{
		Model:     "glm-5.3",
		MaxTokens: 512,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Say OK."}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	saw, text := 0, ""
	kinds := map[string]int{}
	for ev := range ch {
		saw++
		kinds[string(ev.Type)]++
		if ev.Type == "content_block_delta" {
			if ev.Delta.Text != "" {
				text += ev.Delta.Text
			}
			kinds["delta:"+string(ev.Delta.Type)]++
		}
		if ev.Type == "error" {
			t.Fatalf("stream error event: %+v", ev)
		}
	}
	if saw == 0 {
		t.Fatal("no stream events received")
	}
	if text == "" {
		t.Fatalf("no text deltas received — event kinds: %v (glm-5.3 is a reasoning model: raise MaxTokens so thinking leaves room for text)", kinds)
	}
	t.Logf("live glm-5.3 answered %d events %v, text=%q", saw, kinds, text)
}
