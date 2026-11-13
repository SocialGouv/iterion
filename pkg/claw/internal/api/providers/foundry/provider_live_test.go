//go:build live
// +build live

// Live smoke test for Microsoft Azure AI Foundry / Azure OpenAI. Skipped
// unless a real Azure OpenAI endpoint and a deployed model are configured in
// the environment.
//
// Required environment variables:
//
//	AZURE_OPENAI_ENDPOINT     — resource URL, e.g. "https://my-resource.openai.azure.com"
//	AZURE_OPENAI_API_KEY      — optional; if unset, the provider falls back
//	                            to azidentity.DefaultAzureCredential (Azure AD).
//	AZURE_OPENAI_DEPLOYMENT   — deployment name (overridden by FOUNDRY_MODEL when set)
//	AZURE_OPENAI_API_VERSION  — optional, defaults to "2024-08-01-preview"
//	FOUNDRY_MODEL             — optional; deployment name passed via cfg.Model
//	                            (e.g. "gpt-4o", "gpt-5-mini-deployment").
//	                            A leading "foundry/" prefix is stripped.
//
// Run with:
//
//	go test -tags live -run TestLiveStreamSmokeFoundry -v ./internal/api/providers/foundry/...
//
// The test never fails when credentials are missing — it skips cleanly. It
// also never asserts on the exact text returned by the model, only that the
// stream produced at least one text delta and a stop event, which is enough
// to confirm the Azure URL shape, auth header, and SSE decoding all work.
package foundry

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestLiveStreamSmokeFoundry(t *testing.T) {
	if strings.TrimSpace(os.Getenv("AZURE_OPENAI_ENDPOINT")) == "" {
		t.Skip("set AZURE_OPENAI_ENDPOINT (e.g. https://my-resource.openai.azure.com) to run this live test")
	}
	deployment := strings.TrimSpace(os.Getenv("FOUNDRY_MODEL"))
	if deployment == "" {
		deployment = strings.TrimSpace(os.Getenv("AZURE_OPENAI_DEPLOYMENT"))
	}
	if deployment == "" {
		t.Skip("set FOUNDRY_MODEL or AZURE_OPENAI_DEPLOYMENT (deployment name) to run this live test")
	}
	if os.Getenv("AZURE_OPENAI_API_KEY") == "" {
		// Auth may still resolve via DefaultAzureCredential (managed identity,
		// az CLI, env-cred chain). We don't fail here, but we hint.
		t.Logf("AZURE_OPENAI_API_KEY is unset; relying on azidentity.DefaultAzureCredential")
	}

	provider := New()
	client, err := provider.NewClient(api.ProviderConfig{
		Model:     deployment,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Foundry NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := client.StreamResponse(ctx, api.CreateMessageRequest{
		Model:     deployment,
		MaxTokens: 64,
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Say hi in 5 words."}}},
		},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Foundry StreamResponse: %v", err)
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
		t.Fatalf("Foundry stream produced error event: %s", errMsg)
	}
	if !gotText {
		t.Errorf("Foundry stream produced no text_delta events")
	}
	if !gotStop {
		t.Errorf("Foundry stream produced no message_stop event")
	}
}
