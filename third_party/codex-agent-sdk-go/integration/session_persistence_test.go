//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func TestSessionPersistence_ListAndReadMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	var sessionID string

	client := codexsdk.NewClient()
	defer client.Close()

	err = client.Start(ctx,
		codexsdk.WithCwd(cwd),
		codexsdk.WithPermissionMode("bypassPermissions"),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Start failed: %v", err)
	}

	err = client.Query(ctx, codexsdk.Text("Reply with the single word persistence."))
	require.NoError(t, err)

	for msg, err := range client.ReceiveResponse(ctx) {
		require.NoError(t, err)

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			sessionID = result.SessionID
		}
	}

	require.NotEmpty(t, sessionID, "query should produce a session ID")

	var stat *codexsdk.SessionStat

	require.Eventually(t, func() bool {
		var statErr error
		stat, statErr = codexsdk.StatSession(ctx, sessionID, codexsdk.WithCwd(cwd))

		return statErr == nil
	}, 10*time.Second, 250*time.Millisecond, "session should become visible in local state")

	require.Equal(t, sessionID, stat.SessionID)
	require.Equal(t, cwd, stat.Cwd)

	sessions, err := codexsdk.ListSessions(ctx, codexsdk.WithCwd(cwd))
	require.NoError(t, err)
	require.NotEmpty(t, sessions)

	foundSession := false
	for _, session := range sessions {
		if session.SessionID == sessionID {
			foundSession = true
			break
		}
	}

	require.True(t, foundSession, "new session should be listed")

	var messages []codexsdk.Message

	require.Eventually(t, func() bool {
		var messagesErr error
		messages, messagesErr = codexsdk.GetSessionMessages(ctx, sessionID, codexsdk.WithCwd(cwd))

		return messagesErr == nil && len(messages) > 0
	}, 10*time.Second, 250*time.Millisecond, "persisted rollout should become readable")

	var sawAssistant bool
	var sawTaskStarted bool
	var sawTaskComplete bool

	for _, msg := range messages {
		switch msg.(type) {
		case *codexsdk.AssistantMessage:
			sawAssistant = true
		case *codexsdk.TaskStartedMessage:
			sawTaskStarted = true
		case *codexsdk.TaskCompleteMessage:
			sawTaskComplete = true
		case *codexsdk.ResultMessage:
			t.Fatalf("persisted rollout should not synthesize ResultMessage values")
		}
	}

	require.True(t, sawAssistant, "persisted rollout should include assistant output")
	require.True(t, sawTaskStarted, "persisted rollout should include task lifecycle start events")
	require.True(t, sawTaskComplete, "persisted rollout should include task lifecycle completion events")
}
