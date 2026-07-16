package codexsdk

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"github.com/stretchr/testify/require"
)

const testCreateThreadsTable = `CREATE TABLE threads (
	id TEXT PRIMARY KEY,
	rollout_path TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	source TEXT NOT NULL,
	model_provider TEXT NOT NULL,
	cwd TEXT NOT NULL,
	title TEXT NOT NULL,
	sandbox_policy TEXT NOT NULL,
	approval_mode TEXT NOT NULL,
	tokens_used INTEGER NOT NULL DEFAULT 0,
	has_user_event INTEGER NOT NULL DEFAULT 0,
	archived INTEGER NOT NULL DEFAULT 0,
	archived_at INTEGER,
	git_sha TEXT,
	git_branch TEXT,
	git_origin_url TEXT,
	cli_version TEXT NOT NULL DEFAULT '',
	first_user_message TEXT NOT NULL DEFAULT '',
	agent_nickname TEXT,
	agent_role TEXT,
	memory_mode TEXT NOT NULL DEFAULT 'enabled'
)`

const testInsertThread = `INSERT INTO threads (
	id, rollout_path, created_at, updated_at, source,
	model_provider, cwd, title, sandbox_policy, approval_mode,
	tokens_used, archived, archived_at,
	git_sha, git_branch, git_origin_url,
	cli_version, first_user_message, agent_nickname, agent_role, memory_mode
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// setupTestDB creates a temporary codex home with a threads database.
func setupTestDB(t *testing.T) string {
	t.Helper()

	codexHome := t.TempDir()

	dbPath := filepath.Join(codexHome, "state_5.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	defer db.Close()

	_, err = db.Exec(testCreateThreadsTable)
	require.NoError(t, err)

	return codexHome
}

// insertThread inserts a thread row into the test database.
func insertThread(t *testing.T, codexHome string, args ...any) {
	t.Helper()

	dbPath := filepath.Join(codexHome, "state_5.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	defer db.Close()

	_, err = db.Exec(testInsertThread, args...)
	require.NoError(t, err)
}

func TestStatSession_Found(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)

	createdAt := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2025, 6, 15, 11, 30, 0, 0, time.UTC)
	branch := "main"
	sha := "abc123"
	origin := "https://github.com/test/repo"

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440000",
		"/nonexistent/rollout.jsonl",
		createdAt.Unix(), updatedAt.Unix(),
		"cli", "openai", "/home/user/project",
		"Test session", "workspace-write", "full-auto",
		1500, 0, nil,
		&sha, &branch, &origin,
		"0.103.0", "Hello world", nil, nil, "enabled",
	)

	ctx := context.Background()

	stat, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440000",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", stat.SessionID)
	require.Equal(t, createdAt, stat.CreatedAt)
	require.Equal(t, updatedAt, stat.UpdatedAt)
	require.Equal(t, "Test session", stat.Title)
	require.Equal(t, "cli", stat.Source)
	require.Equal(t, "openai", stat.ModelProvider)
	require.Equal(t, "/home/user/project", stat.Cwd)
	require.Equal(t, "workspace-write", stat.SandboxPolicy)
	require.Equal(t, "full-auto", stat.ApprovalMode)
	require.Equal(t, int64(1500), stat.TokensUsed)
	require.False(t, stat.Archived)
	require.Nil(t, stat.ArchivedAt)
	require.NotNil(t, stat.GitSHA)
	require.Equal(t, "abc123", *stat.GitSHA)
	require.NotNil(t, stat.GitBranch)
	require.Equal(t, "main", *stat.GitBranch)
	require.NotNil(t, stat.GitOriginURL)
	require.Equal(t, "https://github.com/test/repo", *stat.GitOriginURL)
	require.Equal(t, "0.103.0", stat.CLIVersion)
	require.Equal(t, "Hello world", stat.FirstUserMessage)
	require.Nil(t, stat.AgentNickname)
	require.Nil(t, stat.AgentRole)
	require.Equal(t, "enabled", stat.MemoryMode)
}

func TestStatSession_WithRolloutFile(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)

	rolloutDir := t.TempDir()
	rolloutPath := filepath.Join(rolloutDir, "session.jsonl")

	err := os.WriteFile(rolloutPath, []byte(`{"type":"test"}`+"\n"), 0o644)
	require.NoError(t, err)

	now := time.Now()

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440010",
		rolloutPath,
		now.Unix(), now.Unix(),
		"cli", "openai", "/tmp/project",
		"Rollout test", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "Hello", nil, nil, "enabled",
	)

	ctx := context.Background()

	stat, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440010",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.Equal(t, rolloutPath, stat.RolloutPath)
	require.Greater(t, stat.SizeBytes, int64(0))
	require.False(t, stat.LastModified.IsZero())
}

func TestStatSession_NotFound(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)
	ctx := context.Background()

	_, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440099",
		WithCodexHome(codexHome),
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestStatSession_InvalidSessionID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	_, err := StatSession(ctx, "not-a-uuid")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid session ID")
	require.False(t, errors.Is(err, ErrSessionNotFound))
}

func TestStatSession_NoDatabaseFile(t *testing.T) {
	t.Parallel()

	codexHome := t.TempDir()
	ctx := context.Background()

	_, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440000",
		WithCodexHome(codexHome),
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestStatSession_CustomCodexHome(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)

	now := time.Now().Unix()

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440020",
		"/path/to/rollout.jsonl",
		now, now,
		"cli", "openai", "/home/user/project",
		"Custom home", "read-only", "full-auto",
		0, 0, nil,
		nil, nil, nil,
		"0.103.0", "Hello", nil, nil, "enabled",
	)

	ctx := context.Background()

	stat, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440020",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440020", stat.SessionID)
}

func TestStatSession_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440000",
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestStatSession_NullableFields(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)

	now := time.Now().Unix()

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440030",
		"/path/to/session.jsonl",
		now, now,
		"cli", "openai", "/home/user/project",
		"Nullable test", "read-only", "full-auto",
		0, 0, nil,
		nil, nil, nil,
		"0.103.0", "", nil, nil, "enabled",
	)

	ctx := context.Background()

	stat, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440030",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.Nil(t, stat.ArchivedAt)
	require.Nil(t, stat.GitSHA)
	require.Nil(t, stat.GitBranch)
	require.Nil(t, stat.GitOriginURL)
	require.Nil(t, stat.AgentNickname)
	require.Nil(t, stat.AgentRole)
}

func TestStatSession_ArchivedSession(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)

	now := time.Now()
	archivedAt := now.Add(-time.Hour)

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440040",
		"/path/to/archived.jsonl",
		now.Unix(), now.Unix(),
		"vscode", "openai", "/home/user/project",
		"Archived session", "workspace-write", "full-auto",
		500, 1, archivedAt.Unix(),
		nil, nil, nil,
		"0.103.0", "Hello", nil, nil, "enabled",
	)

	ctx := context.Background()

	stat, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440040",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.True(t, stat.Archived)
	require.NotNil(t, stat.ArchivedAt)
	require.Equal(t, archivedAt.Unix(), stat.ArchivedAt.Unix())
}

func TestStatSession_MissingRolloutFile(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)

	now := time.Now().Unix()

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440050",
		"/nonexistent/path/rollout.jsonl",
		now, now,
		"cli", "openai", "/home/user/project",
		"Missing rollout", "read-only", "full-auto",
		0, 0, nil,
		nil, nil, nil,
		"0.103.0", "Hello", nil, nil, "enabled",
	)

	ctx := context.Background()

	stat, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440050",
		WithCodexHome(codexHome),
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), stat.SizeBytes)
	require.True(t, stat.LastModified.IsZero())
}

func TestStatSession_ProjectScoped(t *testing.T) {
	t.Parallel()

	codexHome := setupTestDB(t)

	now := time.Now().Unix()

	insertThread(t, codexHome,
		"550e8400-e29b-41d4-a716-446655440060",
		"/path/to/rollout.jsonl",
		now, now,
		"cli", "openai", "/home/user/project-a",
		"Project A", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "Hello A", nil, nil, "enabled",
	)

	ctx := context.Background()

	// Matching project returns the session.
	stat, err := StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440060",
		WithCodexHome(codexHome),
		WithCwd("/home/user/project-a"),
	)
	require.NoError(t, err)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440060", stat.SessionID)

	// Different project returns not found.
	_, err = StatSession(ctx,
		"550e8400-e29b-41d4-a716-446655440060",
		WithCodexHome(codexHome),
		WithCwd("/home/user/project-b"),
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrSessionNotFound))
}
