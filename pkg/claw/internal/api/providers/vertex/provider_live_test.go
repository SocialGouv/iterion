//go:build live
// +build live

// Live smoke test for Google Cloud Vertex AI (Anthropic models). Skipped
// unless real GCP Application Default Credentials and a Vertex-accessible
// model are configured in the environment.
//
// Required environment variables:
//
//	GOOGLE_CLOUD_PROJECT          — GCP project id
//	GOOGLE_CLOUD_REGION           — optional, defaults to "us-east5"
//	GOOGLE_APPLICATION_CREDENTIALS — path to a service-account JSON, OR
//	                                 run `gcloud auth application-default login`
//	VERTEX_MODEL                  — Vertex/Anthropic model id, either canonical
//	                                 ("claude-sonnet-4-20250514") or the Vertex
//	                                 "@version" form ("claude-sonnet-4@20250514")
//
// Run with:
//
//	go test -tags live -run TestLiveStreamSmokeVertex -v ./internal/api/providers/vertex/...
//
// The test never fails when credentials are missing — it skips cleanly. It
// also never asserts on the exact text returned by the model, only that the
// stream produced at least one text delta and a stop event, which is enough
// to confirm ADC, the streamRawPredict URL shape, and SSE decoding all work.
package vertex

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestLiveStreamSmokeVertex(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("VERTEX_MODEL"))
	if model == "" {
		t.Skip("set VERTEX_MODEL (e.g. claude-sonnet-4-20250514) to run this live test")
	}
	if strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")) == "" {
		t.Skip("set GOOGLE_CLOUD_PROJECT to run this live test")
	}
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		// ADC may also resolve via `gcloud auth application-default login` or
		// workload identity. We don't fail here, but we hint.
		t.Logf("GOOGLE_APPLICATION_CREDENTIALS is unset; relying on ambient ADC (gcloud / workload identity)")
	}

	provider := New()
	client, err := provider.NewClient(api.ProviderConfig{
		Model:     model,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Vertex NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := client.StreamResponse(ctx, api.CreateMessageRequest{
		Model:     model,
		MaxTokens: 64,
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Say hi in 5 words."}}},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Vertex StreamResponse: %v", err)
	}

	var (
		gotText bool
		gotStop bool
		errMsg  string
	)
	for ev := range events {
		switch ev.Type {
		case api.EventContentBlockDelta:
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				gotText = true
			}
		case api.EventMessageStop:
			gotStop = true
		case api.EventError:
			errMsg = ev.ErrorMessage
		}
	}
	if errMsg != "" {
		t.Fatalf("Vertex stream produced error event: %s", errMsg)
	}
	if !gotText {
		t.Errorf("Vertex stream produced no text_delta events")
	}
	if !gotStop {
		t.Errorf("Vertex stream produced no message_stop event")
	}
}
