package codexsdk

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/session"
)

func resolveCodexHome(options *CodexAgentOptions) (string, error) {
	codexHome := options.CodexHome
	if codexHome != "" {
		return codexHome, nil
	}

	return session.DefaultCodexHome()
}

func buildSessionStat(row *session.ThreadRow) *SessionStat {
	return &SessionStat{
		SessionID:        row.ID,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
		Title:            row.Title,
		Source:           row.Source,
		ModelProvider:    row.ModelProvider,
		Cwd:              row.Cwd,
		SandboxPolicy:    row.SandboxPolicy,
		ApprovalMode:     row.ApprovalMode,
		TokensUsed:       row.TokensUsed,
		Archived:         row.Archived,
		ArchivedAt:       row.ArchivedAt,
		RolloutPath:      row.RolloutPath,
		GitSHA:           row.GitSHA,
		GitBranch:        row.GitBranch,
		GitOriginURL:     row.GitOriginURL,
		CLIVersion:       row.CLIVersion,
		FirstUserMessage: row.FirstUserMessage,
		AgentNickname:    row.AgentNickname,
		AgentRole:        row.AgentRole,
		MemoryMode:       row.MemoryMode,
	}
}

func applyRolloutFileStat(stat *SessionStat) {
	fi, err := os.Stat(stat.RolloutPath)
	if err == nil {
		stat.SizeBytes = fi.Size()
		stat.LastModified = fi.ModTime()
	}
}

// SessionStat contains metadata about a Codex CLI session read from the
// local SQLite database (~/.codex/state_5.sqlite).
type SessionStat struct {
	// SessionID is the UUID of the session thread.
	SessionID string

	// SizeBytes is the size of the rollout JSONL file in bytes (0 if missing).
	SizeBytes int64

	// LastModified is the modification time of the rollout JSONL file.
	LastModified time.Time

	// CreatedAt is when the session was created (from the database).
	CreatedAt time.Time

	// UpdatedAt is when the session was last updated (from the database).
	UpdatedAt time.Time

	// Title is the session title.
	Title string

	// Source indicates how the session was created ("cli", "vscode", etc.).
	Source string

	// ModelProvider is the model provider used for the session.
	ModelProvider string

	// Cwd is the working directory for the session.
	Cwd string

	// SandboxPolicy is the sandbox policy applied to the session.
	SandboxPolicy string

	// ApprovalMode is the approval mode for the session.
	ApprovalMode string

	// TokensUsed is the total number of tokens consumed by the session.
	TokensUsed int64

	// Archived indicates whether the session has been archived.
	Archived bool

	// ArchivedAt is when the session was archived (nil if not archived).
	ArchivedAt *time.Time

	// RolloutPath is the path to the session's rollout JSONL file.
	RolloutPath string

	// GitSHA is the git commit SHA at session creation (nil if unavailable).
	GitSHA *string

	// GitBranch is the git branch at session creation (nil if unavailable).
	GitBranch *string

	// GitOriginURL is the git remote origin URL (nil if unavailable).
	GitOriginURL *string

	// CLIVersion is the version of the Codex CLI used for the session.
	CLIVersion string

	// FirstUserMessage is the first message sent by the user.
	FirstUserMessage string

	// AgentNickname is the agent's display name (nil if not set).
	AgentNickname *string

	// AgentRole is the agent's role designation (nil if not set).
	AgentRole *string

	// MemoryMode is the memory mode setting for the session.
	MemoryMode string
}

// StatSession reads session metadata from the Codex CLI's local SQLite
// database. The sessionID must be a valid UUID. Use WithCwd to filter by
// project directory and WithCodexHome to override the default ~/.codex
// location.
//
// Returns ErrSessionNotFound when the session does not exist or the
// database file is missing.
func StatSession(
	ctx context.Context,
	sessionID string,
	opts ...Option,
) (*SessionStat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := uuid.Parse(sessionID); err != nil {
		return nil, fmt.Errorf("invalid session ID %q: %w", sessionID, err)
	}

	o := applyAgentOptions(opts)

	codexHome, err := resolveCodexHome(o)
	if err != nil {
		return nil, fmt.Errorf("resolving codex home: %w", err)
	}

	dbPath := session.DatabasePath(codexHome)

	row, err := session.LookupThread(ctx, dbPath, sessionID, o.Cwd)
	if err != nil {
		return nil, err
	}

	stat := buildSessionStat(row)
	applyRolloutFileStat(stat)

	return stat, nil
}
