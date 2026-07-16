//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// TestDynamicControl_SetPermissionMode tests changing permission mode mid-session.
func TestDynamicControl_SetPermissionMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("default"),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Connect failed: %v", err)
	}

	err = client.SetPermissionMode(ctx, "bypassPermissions")
	require.NoError(t, err, "SetPermissionMode should succeed")

	err = client.Query(ctx, codexsdk.Text("Say 'permission changed'"))
	require.NoError(t, err, "Query should succeed after SetPermissionMode")

	var messages []codexsdk.Message
	for msg, err := range client.ReceiveResponse(ctx) {
		require.NoError(t, err, "ReceiveResponse should succeed")
		messages = append(messages, msg)
	}

	require.NotEmpty(t, messages, "Should receive messages")
}

// TestDynamicControl_SetModel tests switching model during session.
func TestDynamicControl_SetModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Connect failed: %v", err)
	}

	err = client.SetModel(ctx, new("codex-mini-latest"))
	require.NoError(t, err, "SetModel should succeed")

	err = client.Query(ctx, codexsdk.Text("Say 'model changed'"))
	require.NoError(t, err, "Query should succeed after SetModel")

	var messages []codexsdk.Message
	for msg, err := range client.ReceiveResponse(ctx) {
		require.NoError(t, err, "ReceiveResponse should succeed")
		messages = append(messages, msg)
	}

	require.NotEmpty(t, messages, "Should receive messages")
}

// TestDynamicControl_Interrupt tests interrupting a long-running query.
func TestDynamicControl_Interrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Connect failed: %v", err)
	}

	err = client.Query(ctx,
		codexsdk.Text("Write a very long essay about the history of computing, including many details."))
	require.NoError(t, err, "Query should succeed")

	time.Sleep(500 * time.Millisecond)

	err = client.Interrupt(ctx)
	require.NoError(t, err, "Interrupt should succeed")

	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			break
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			t.Logf("Session ended: isError=%v", result.IsError)

			break
		}
	}
}
