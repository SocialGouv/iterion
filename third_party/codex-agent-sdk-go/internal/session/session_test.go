package session

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	_ "github.com/glebarez/go-sqlite"
	"github.com/stretchr/testify/require"
)

const createThreadsTable = `CREATE TABLE threads (
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

const insertThread = `INSERT INTO threads (
	id, rollout_path, created_at, updated_at, source,
	model_provider, cwd, title, sandbox_policy, approval_mode,
	tokens_used, archived, archived_at,
	git_sha, git_branch, git_origin_url,
	cli_version, first_user_message, agent_nickname, agent_role, memory_mode
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// createTestDB creates a temporary SQLite database with the threads table.
func createTestDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), databaseFile)

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	defer db.Close()

	_, err = db.Exec(createThreadsTable)
	require.NoError(t, err)

	return dbPath
}

// insertTestThread inserts a thread row into the test database.
func insertTestThread(t *testing.T, dbPath string, args ...any) {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	defer db.Close()

	_, err = db.Exec(insertThread, args...)
	require.NoError(t, err)
}

func TestDefaultCodexHome(t *testing.T) {
	t.Parallel()

	home, err := DefaultCodexHome()
	require.NoError(t, err)

	userHome, err := os.UserHomeDir()
	require.NoError(t, err)

	require.Equal(t, filepath.Join(userHome, ".codex"), home)
}

func TestDatabasePath(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/home/user/.codex/state_5.sqlite", DatabasePath("/home/user/.codex"))
}

func TestLookupThread_Found(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)

	createdAt := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2025, 6, 15, 11, 30, 0, 0, time.UTC)
	branch := "main"
	sha := "abc123"
	origin := "https://github.com/test/repo"

	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440000",
		"/home/user/.codex/sessions/abc.jsonl",
		createdAt.Unix(), updatedAt.Unix(),
		"cli", "openai", "/home/user/project",
		"Test session", "workspace-write", "full-auto",
		1500, 0, nil,
		&sha, &branch, &origin,
		"0.103.0", "Hello world", nil, nil, "enabled",
	)

	ctx := context.Background()

	row, err := LookupThread(ctx, dbPath, "550e8400-e29b-41d4-a716-446655440000", "")
	require.NoError(t, err)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", row.ID)
	require.Equal(t, "/home/user/.codex/sessions/abc.jsonl", row.RolloutPath)
	require.Equal(t, createdAt, row.CreatedAt)
	require.Equal(t, updatedAt, row.UpdatedAt)
	require.Equal(t, "Test session", row.Title)
	require.Equal(t, "cli", row.Source)
	require.Equal(t, "openai", row.ModelProvider)
	require.Equal(t, "/home/user/project", row.Cwd)
	require.Equal(t, "workspace-write", row.SandboxPolicy)
	require.Equal(t, "full-auto", row.ApprovalMode)
	require.Equal(t, int64(1500), row.TokensUsed)
	require.False(t, row.Archived)
	require.Nil(t, row.ArchivedAt)
	require.NotNil(t, row.GitSHA)
	require.Equal(t, "abc123", *row.GitSHA)
	require.NotNil(t, row.GitBranch)
	require.Equal(t, "main", *row.GitBranch)
	require.NotNil(t, row.GitOriginURL)
	require.Equal(t, "https://github.com/test/repo", *row.GitOriginURL)
	require.Equal(t, "0.103.0", row.CLIVersion)
	require.Equal(t, "Hello world", row.FirstUserMessage)
	require.Nil(t, row.AgentNickname)
	require.Nil(t, row.AgentRole)
	require.Equal(t, "enabled", row.MemoryMode)
}

func TestLookupThread_NotFound(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)
	ctx := context.Background()

	_, err := LookupThread(ctx, dbPath, "550e8400-e29b-41d4-a716-446655440000", "")
	require.Error(t, err)
	require.True(t, errors.Is(err, sdkerrors.ErrSessionNotFound))
}

func TestLookupThread_NoDatabaseFile(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "nonexistent.sqlite")
	ctx := context.Background()

	_, err := LookupThread(ctx, dbPath, "550e8400-e29b-41d4-a716-446655440000", "")
	require.Error(t, err)
	require.True(t, errors.Is(err, sdkerrors.ErrSessionNotFound))
}

func TestLookupThread_ProjectScoped(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)

	now := time.Now().Unix()

	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440001",
		"/path/to/rollout.jsonl",
		now, now,
		"cli", "openai", "/home/user/project-a",
		"Session A", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "Hello A", nil, nil, "enabled",
	)

	ctx := context.Background()

	// Matching project path returns the thread.
	row, err := LookupThread(
		ctx, dbPath,
		"550e8400-e29b-41d4-a716-446655440001",
		"/home/user/project-a",
	)
	require.NoError(t, err)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440001", row.ID)

	// Different project path returns not found.
	_, err = LookupThread(
		ctx, dbPath,
		"550e8400-e29b-41d4-a716-446655440001",
		"/home/user/project-b",
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, sdkerrors.ErrSessionNotFound))
}

func TestLookupThread_ContextCancelled(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := LookupThread(ctx, dbPath, "550e8400-e29b-41d4-a716-446655440000", "")
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestLookupThread_ArchivedThread(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)

	now := time.Now()
	archivedAt := now.Add(-time.Hour)

	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440002",
		"/path/to/archived.jsonl",
		now.Unix(), now.Unix(),
		"vscode", "openai", "/home/user/project",
		"Archived session", "workspace-write", "full-auto",
		500, 1, archivedAt.Unix(),
		nil, nil, nil,
		"0.103.0", "Hello", nil, nil, "enabled",
	)

	ctx := context.Background()

	row, err := LookupThread(ctx, dbPath, "550e8400-e29b-41d4-a716-446655440002", "")
	require.NoError(t, err)
	require.True(t, row.Archived)
	require.NotNil(t, row.ArchivedAt)
	require.Equal(t, archivedAt.Unix(), row.ArchivedAt.Unix())
}

func TestLookupThread_NullableFields(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)

	now := time.Now().Unix()

	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440003",
		"/path/to/session.jsonl",
		now, now,
		"cli", "openai", "/home/user/project",
		"Session with nulls", "read-only", "full-auto",
		0, 0, nil,
		nil, // git_sha
		nil, // git_branch
		nil, // git_origin_url
		"0.103.0", "", nil, nil, "enabled",
	)

	ctx := context.Background()

	row, err := LookupThread(ctx, dbPath, "550e8400-e29b-41d4-a716-446655440003", "")
	require.NoError(t, err)
	require.Nil(t, row.ArchivedAt)
	require.Nil(t, row.GitSHA)
	require.Nil(t, row.GitBranch)
	require.Nil(t, row.GitOriginURL)
	require.Nil(t, row.AgentNickname)
	require.Nil(t, row.AgentRole)
}

func TestListThreads(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)
	now := time.Now().UTC()

	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440010",
		"/path/to/older.jsonl",
		now.Add(-2*time.Hour).Unix(), now.Add(-2*time.Hour).Unix(),
		"cli", "openai", "/home/user/project-a",
		"Older", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "older", nil, nil, "enabled",
	)
	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440011",
		"/path/to/newer.jsonl",
		now.Add(-time.Hour).Unix(), now.Unix(),
		"vscode", "openai", "/home/user/project-b",
		"Newer", "workspace-write", "full-auto",
		200, 0, nil,
		nil, nil, nil,
		"0.103.0", "newer", nil, nil, "enabled",
	)

	rows, err := ListThreads(context.Background(), dbPath, "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440011", rows[0].ID)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440010", rows[1].ID)
}

func TestListThreads_ProjectScoped(t *testing.T) {
	t.Parallel()

	dbPath := createTestDB(t)
	now := time.Now().Unix()

	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440012",
		"/path/to/a.jsonl",
		now, now,
		"cli", "openai", "/home/user/project-a",
		"Project A", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "a", nil, nil, "enabled",
	)
	insertTestThread(t, dbPath,
		"550e8400-e29b-41d4-a716-446655440013",
		"/path/to/b.jsonl",
		now, now,
		"cli", "openai", "/home/user/project-b",
		"Project B", "read-only", "full-auto",
		100, 0, nil,
		nil, nil, nil,
		"0.103.0", "b", nil, nil, "enabled",
	)

	rows, err := ListThreads(context.Background(), dbPath, "/home/user/project-a")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440012", rows[0].ID)
}

func TestListThreads_NoDatabaseFile(t *testing.T) {
	t.Parallel()

	rows, err := ListThreads(context.Background(), filepath.Join(t.TempDir(), "missing.sqlite"), "")
	require.NoError(t, err)
	require.Empty(t, rows)
}
