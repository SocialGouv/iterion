// Package session provides SQLite-based session lookup for Codex CLI threads.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sdkerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	_ "github.com/glebarez/go-sqlite"
)

// databaseFile is the name of the Codex state database.
const databaseFile = "state_5.sqlite"

// sqliteDriverName is the database/sql driver name used to open
// SQLite databases. The caller must ensure a compatible driver is
// registered under this name (e.g. glebarez/go-sqlite via blank import).
const sqliteDriverName = "sqlite"

// ThreadRow represents a row from the Codex threads table.
type ThreadRow struct {
	ID               string
	RolloutPath      string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Title            string
	Source           string
	ModelProvider    string
	Cwd              string
	SandboxPolicy    string
	ApprovalMode     string
	TokensUsed       int64
	Archived         bool
	ArchivedAt       *time.Time
	GitSHA           *string
	GitBranch        *string
	GitOriginURL     *string
	CLIVersion       string
	FirstUserMessage string
	AgentNickname    *string
	AgentRole        *string
	MemoryMode       string
}

type scanner interface {
	Scan(dest ...any) error
}

// DefaultCodexHome returns the default Codex home directory (~/.codex).
func DefaultCodexHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}

	return filepath.Join(home, ".codex"), nil
}

// DatabasePath returns the full path to the Codex state database.
func DatabasePath(codexHome string) string {
	return filepath.Join(codexHome, databaseFile)
}

func scanThreadRow(row scanner) (*ThreadRow, error) {
	var t ThreadRow

	var (
		createdAtUnix, updatedAtUnix int64
		archivedInt                  int64
		archivedAtUnix               *int64
	)

	err := row.Scan(
		&t.ID, &t.RolloutPath, &createdAtUnix, &updatedAtUnix,
		&t.Title, &t.Source, &t.ModelProvider, &t.Cwd,
		&t.SandboxPolicy, &t.ApprovalMode, &t.TokensUsed,
		&archivedInt, &archivedAtUnix,
		&t.GitSHA, &t.GitBranch, &t.GitOriginURL,
		&t.CLIVersion, &t.FirstUserMessage, &t.AgentNickname,
		&t.AgentRole, &t.MemoryMode,
	)
	if err != nil {
		return nil, err
	}

	t.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	t.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
	t.Archived = archivedInt != 0

	if archivedAtUnix != nil {
		at := time.Unix(*archivedAtUnix, 0).UTC()
		t.ArchivedAt = &at
	}

	return &t, nil
}

// LookupThread queries the Codex threads table for a session by ID.
// When projectPath is non-empty, it also filters by the cwd column.
// Returns ErrSessionNotFound when no matching row exists or the database
// file is missing.
func LookupThread(
	ctx context.Context,
	dbPath string,
	sessionID string,
	projectPath string,
) (*ThreadRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(
			"database not found at %s: %w", dbPath, sdkerrors.ErrSessionNotFound,
		)
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", dbPath)

	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	query := `SELECT
		id, rollout_path, created_at, updated_at, title, source,
		model_provider, cwd, sandbox_policy, approval_mode,
		tokens_used, archived, archived_at,
		git_sha, git_branch, git_origin_url,
		cli_version, first_user_message, agent_nickname,
		agent_role, memory_mode
	FROM threads WHERE id = ?`

	args := []any{sessionID}

	if projectPath != "" {
		query += " AND cwd = ?"

		args = append(args, projectPath)
	}

	row := db.QueryRowContext(ctx, query, args...)

	t, err := scanThreadRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf(
				"session %s: %w", sessionID, sdkerrors.ErrSessionNotFound,
			)
		}

		return nil, fmt.Errorf("querying thread: %w", err)
	}

	return t, nil
}

// ListThreads returns all known threads, newest first.
// When projectPath is non-empty, results are filtered by exact cwd match.
func ListThreads(ctx context.Context, dbPath string, projectPath string) ([]ThreadRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", dbPath)

	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	query := `SELECT
		id, rollout_path, created_at, updated_at, title, source,
		model_provider, cwd, sandbox_policy, approval_mode,
		tokens_used, archived, archived_at,
		git_sha, git_branch, git_origin_url,
		cli_version, first_user_message, agent_nickname,
		agent_role, memory_mode
	FROM threads`

	var args []any

	if projectPath != "" {
		query += " WHERE cwd = ?"

		args = append(args, projectPath)
	}

	query += " ORDER BY updated_at DESC, created_at DESC"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying threads: %w", err)
	}
	defer rows.Close()

	threads := make([]ThreadRow, 0, 16)

	for rows.Next() {
		row, err := scanThreadRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning thread row: %w", err)
		}

		threads = append(threads, *row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating thread rows: %w", err)
	}

	return threads, nil
}
