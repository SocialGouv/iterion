package moonshot

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// The falsifier for the routing claim itself: a REAL call through the
// moonshot provider — real endpoint, real account key, kimi-k2 served.
// Guarded by MOONSHOT_LIVE_PROBE=1 so CI never spends the account; run it
// before pushing:
//
//	MOONSHOT_LIVE_PROBE=1 go test -run TestLiveMoonshot -v ./internal/api/providers/moonshot/
//
// What it proves (and what a stub cannot): the wire shape Moonshot actually
// accepts — header, path, model id — for the exact pair this provider exists
// to route. A stub proves the provider calls its own client; only the live
// endpoint proves Moonshot answers "kimi-k2" on this wire.
func TestLiveMoonshotStream(t *testing.T) {
	key := os.Getenv("MOONSHOT_API_KEY")
	if key == "" || os.Getenv("MOONSHOT_LIVE_PROBE") == "" {
		t.Skip("set MOONSHOT_API_KEY + MOONSHOT_LIVE_PROBE=1 to run the live probe")
	}
	model := os.Getenv("MOONSHOT_LIVE_MODEL")
	if model == "" {
		model = "kimi-k2"
	}
	c, err := New().NewClient(api.ProviderConfig{APIKey: key, Model: model})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := c.StreamResponse(ctx, api.CreateMessageRequest{
		Model:     model,
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
		t.Fatalf("no text deltas received — event kinds: %v (a thinking model needs a higher MaxTokens so reasoning leaves room for text)", kinds)
	}
	t.Logf("live %s answered %d events %v, text=%q", model, saw, kinds, text)
}
