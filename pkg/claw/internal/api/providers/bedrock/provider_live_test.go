//go:build live
// +build live

// Live smoke test for AWS Bedrock. Skipped unless real AWS credentials and a
// Bedrock-accessible model are configured in the environment.
//
// Required environment variables:
//
//	AWS_REGION              — e.g. "us-east-1" (or AWS_DEFAULT_REGION)
//	AWS_ACCESS_KEY_ID       — IAM access key (or AWS_PROFILE / instance role)
//	AWS_SECRET_ACCESS_KEY   — paired secret
//	AWS_SESSION_TOKEN       — optional, for STS / role-chained credentials
//	BEDROCK_MODEL           — Bedrock model id, e.g.
//	                          "anthropic.claude-3-5-sonnet-20241022-v2:0"
//	                          or a cross-region inference profile such as
//	                          "us.anthropic.claude-sonnet-4-20250514-v1:0"
//
// Run with:
//
//	go test -tags live -run TestLiveStreamSmokeBedrock -v ./internal/api/providers/bedrock/...
//
// The test never fails when credentials are missing — it skips cleanly. It
// also never asserts on the exact text returned by the model, only that the
// stream produced at least one text delta and a stop event, which is enough
// to confirm authentication, request shape, and SSE decoding all work.
package bedrock

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestLiveStreamSmokeBedrock(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("BEDROCK_MODEL"))
	if model == "" {
		t.Skip("set BEDROCK_MODEL (e.g. anthropic.claude-3-5-sonnet-20241022-v2:0) to run this live test")
	}
	region := strings.TrimSpace(os.Getenv("AWS_REGION"))
	if region == "" {
		region = strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION"))
	}
	if region == "" {
		t.Skip("set AWS_REGION (or AWS_DEFAULT_REGION) to run this live test")
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" && os.Getenv("AWS_PROFILE") == "" && os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") == "" {
		t.Skip("set AWS credentials (AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY, or AWS_PROFILE, or instance role) to run this live test")
	}

	provider := New()
	client, err := provider.NewClient(api.ProviderConfig{
		Model:     model,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Bedrock NewClient: %v", err)
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
		t.Fatalf("Bedrock StreamResponse: %v", err)
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
		t.Fatalf("Bedrock stream produced error event: %s", errMsg)
	}
	if !gotText {
		t.Errorf("Bedrock stream produced no text_delta events")
	}
	if !gotStop {
		t.Errorf("Bedrock stream produced no message_stop event")
	}
}
