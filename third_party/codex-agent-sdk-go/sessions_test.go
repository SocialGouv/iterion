package codexsdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestListSessions(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)
	now := time.Now().UTC()

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440100",
		"/nonexistent/older.jsonl",
		now.Add(-2*time.Hour).Unix(), now.Add(-2*time.Hour).Unix(),
		"cli", "openai", "/home/user/project-a",
		"Older session", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "older", nil, nil, "enabled",
	)
	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440101",
		"/nonexistent/newer.jsonl",
		now.Add(-time.Hour).Unix(), now.Unix(),
		"vscode", "openai", "/home/user/project-b",
		"Newer session", "workspace-write", "full-auto",
		200, 0, nil,
		nil, nil, nil,
		"0.103.0", "newer", nil, nil, "enabled",
	)

	sessions, err := ListSessions(context.Background(), WithCodexHome(codexHome))
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440101", sessions[0].SessionID)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440100", sessions[1].SessionID)
}

func TestListSessions_WithCwd(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)
	now := time.Now().Unix()

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440102",
		"/nonexistent/a.jsonl",
		now, now,
		"cli", "openai", "/home/user/project-a",
		"Project A", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "a", nil, nil, "enabled",
	)
	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440103",
		"/nonexistent/b.jsonl",
		now, now,
		"cli", "openai", "/home/user/project-b",
		"Project B", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "b", nil, nil, "enabled",
	)

	sessions, err := ListSessions(
		context.Background(),
		WithCodexHome(codexHome),
		WithCwd("/home/user/project-a"),
	)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440102", sessions[0].SessionID)
}

func TestListSessions_NoDatabase(t *testing.T) {
	t.Parallel()

	sessions, err := ListSessions(context.Background(), WithCodexHome(t.TempDir()))
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestGetSessionMessages(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)
	rolloutPath := filepath.Join(t.TempDir(), "session.jsonl")

	content := "" +
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread_123\"}}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn_123\",\"collaboration_mode_kind\":\"default\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"hi\"}]}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":\"turn_123\",\"last_agent_message\":\"done\"}}\n"

	err := os.WriteFile(rolloutPath, []byte(content), 0o644)
	require.NoError(t, err)

	now := time.Now().Unix()
	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440104",
		rolloutPath,
		now, now,
		"cli", "openai", "/home/user/project",
		"Session", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "hello", nil, nil, "enabled",
	)

	messages, err := GetSessionMessages(
		context.Background(),
		"550e8400-e29b-41d4-a716-446655440104",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	require.IsType(t, &TaskStartedMessage{}, messages[0])
	require.IsType(t, &UserMessage{}, messages[1])
	require.IsType(t, &AssistantMessage{}, messages[2])
	require.IsType(t, &TaskCompleteMessage{}, messages[3])

	taskComplete, ok := messages[3].(*TaskCompleteMessage)
	require.True(t, ok)
	require.Equal(t, "turn_123", taskComplete.TurnID)
	require.NotNil(t, taskComplete.LastAgentMessage)
	require.Equal(t, "done", *taskComplete.LastAgentMessage)
}

func TestGetSessionMessages_FallsBackToGenericParser(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)
	rolloutPath := filepath.Join(t.TempDir(), "session.jsonl")

	content := "" +
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread_123\"}}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\",\"text\":\"skip me\"}}\n" +
		"{\"type\":\"thread.started\"}\n" +
		"{\"type\":\"turn.completed\",\"session_id\":\"session_123\",\"result\":\"done\"}\n"

	err := os.WriteFile(rolloutPath, []byte(content), 0o644)
	require.NoError(t, err)

	now := time.Now().Unix()
	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440105",
		rolloutPath,
		now, now,
		"cli", "openai", "/home/user/project",
		"Session", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "hello", nil, nil, "enabled",
	)

	messages, err := GetSessionMessages(
		context.Background(),
		"550e8400-e29b-41d4-a716-446655440105",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.IsType(t, &SystemMessage{}, messages[0])
	require.IsType(t, &ResultMessage{}, messages[1])

	result, ok := messages[1].(*ResultMessage)
	require.True(t, ok)
	require.NotNil(t, result.Result)
	require.Equal(t, "done", *result.Result)
	require.Equal(t, "session_123", result.SessionID)
}

func TestGetSessionMessages_NotFound(t *testing.T) {
	t.Parallel()

	_, err := GetSessionMessages(
		context.Background(),
		"550e8400-e29b-41d4-a716-446655440199",
		WithCodexHome(setupTestDB(t)),
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSessionNotFound))
}
