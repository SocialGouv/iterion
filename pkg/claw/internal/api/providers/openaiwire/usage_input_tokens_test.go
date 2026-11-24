package openaiwire

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// The chat-completions stream reports prompt_tokens in its terminal usage
// chunk. The translation parsed the field and discarded it, on the stated
// premise that input tokens "travel via the provider's own request
// bookkeeping" — there is no such bookkeeping: the only other carrier is
// message_start's InputTokens, which this path never sets.
func TestStreamEvents_SurfacesPromptTokens(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":4321,"completion_tokens":9,"total_tokens":4330}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(frames)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	ch := make(chan api.StreamEvent, 32)
	go StreamEvents(context.Background(), resp, ch)

	var usage api.UsageDelta
	seen := false
	for ev := range ch {
		if ev.Type == api.EventMessageDelta {
			usage, seen = ev.Usage, true
		}
	}
	if !seen {
		t.Fatal("no message_delta in the translated stream")
	}
	if usage.InputTokens != 4321 {
		t.Errorf("input tokens = %d, want 4321 — the chunk reported them and the translation dropped them", usage.InputTokens)
	}
	if usage.OutputTokens != 9 {
		t.Errorf("output tokens = %d, want 9", usage.OutputTokens)
	}
}
