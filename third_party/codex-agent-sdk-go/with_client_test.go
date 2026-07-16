package codexsdk_test

import (
	"context"
	"errors"
	"testing"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func TestWithClient_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := codexsdk.WithClient(ctx, func(_ codexsdk.Client) error {
		t.Error("callback should not be called with cancelled context")

		return nil
	})
	if err == nil {
		t.Error("expected error for cancelled context")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestWithClient_CallbackError(t *testing.T) {
	// This test requires a mock transport to avoid needing the real CLI
	// For now, we skip this test if the CLI is not available
	t.Skip("requires mock transport or real CLI")
}

func TestWithClient_OptionsPassedToStart(t *testing.T) {
	// This test verifies options are correctly passed through
	// Requires mock transport
	t.Skip("requires mock transport or real CLI")
}
