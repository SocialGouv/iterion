//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// TestQuery_CloseMidStream tests that closing the client mid-stream
// during a real query terminates cleanly without hanging processes.
func TestQuery_CloseMidStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := codexsdk.NewClient()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Connect failed: %v", err)
	}

	err = client.Query(ctx, codexsdk.Text("Write a short story about a robot. Include at least 3 paragraphs."))
	require.NoError(t, err, "Query should succeed")

	receiveDone := make(chan struct{})

	var receivedCount int

	var receivedTypes []string

	go func() {
		defer close(receiveDone)

		for msg, err := range client.ReceiveMessages(ctx) {
			if err != nil {
				t.Logf("ReceiveMessages error: %v", err)

				return
			}

			receivedCount++
			receivedTypes = append(receivedTypes, msg.MessageType())

			if receivedCount >= 2 {
				return
			}
		}
	}()

	select {
	case <-receiveDone:
		// Got some messages
	case <-time.After(30 * time.Second):
		t.Fatal("Timeout waiting for messages")
	}

	t.Logf("Received %d messages before close: %v", receivedCount, receivedTypes)

	closeStart := time.Now()
	err = client.Close()
	closeDuration := time.Since(closeStart)

	require.NoError(t, err, "Close should succeed")
	t.Logf("Close completed in %v", closeDuration)

	require.Less(t, closeDuration, 10*time.Second,
		"Close should complete quickly, not wait for full response")

	require.Greater(t, receivedCount, 0, "Should have received messages before close")
}

// TestClient_ContextCancelDuringQuery tests that context cancellation
// during an active query terminates cleanly.
func TestClient_ContextCancelDuringQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	queryCtx, queryCancel := context.WithCancel(ctx)

	err = client.Query(queryCtx, codexsdk.Text("Explain quantum computing in detail."))
	require.NoError(t, err, "Query should succeed")

	receiveDone := make(chan struct{})

	var receivedCount int

	var gotContextError bool

	go func() {
		defer close(receiveDone)

		for _, err := range client.ReceiveMessages(queryCtx) {
			if err != nil {
				if err == context.Canceled {
					gotContextError = true
				}

				return
			}

			receivedCount++

			if receivedCount >= 2 {
				queryCancel()
			}
		}
	}()

	select {
	case <-receiveDone:
		// Success
	case <-time.After(30 * time.Second):
		t.Fatal("Timeout waiting for receiver to complete")
	}

	t.Logf("Received %d messages, gotContextError: %v", receivedCount, gotContextError)

	require.True(t, receivedCount > 0 || gotContextError,
		"Should have received messages or context error")
}

// TestClient_RapidCloseReopen tests rapid close and reopen doesn't cause issues.
func TestClient_RapidCloseReopen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for i := range 3 {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			client := codexsdk.NewClient()

			err := client.Start(ctx,
				codexsdk.WithPermissionMode("bypassPermissions"),
			)
			if err != nil {
				skipIfCLINotInstalled(t, err)
				t.Fatalf("Connect failed: %v", err)
			}

			err = client.Query(ctx, codexsdk.Text("Say 'hello'"))
			require.NoError(t, err)

			for msg, err := range client.ReceiveMessages(ctx) {
				if err != nil {
					break
				}

				t.Logf("Got message type: %s", msg.MessageType())

				break
			}

			err = client.Close()
			require.NoError(t, err)
		})
	}
}
