package runtime

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Session file format constants.
const (
	PrimarySessionExtension = "jsonl"
	LegacySessionExtension  = "json"
	LatestSessionReference  = "latest"
)

// sessionReferenceAliases are case-insensitive aliases that resolve to the
// most recently modified session.
var sessionReferenceAliases = []string{LatestSessionReference, "last", "recent"}

// SessionStore provides per-workspace session namespacing using an FNV-1a
// fingerprint of the workspace root path. This ensures parallel instances
// serving different workspaces never collide.
type SessionStore struct {
	// sessionsRoot is the fully resolved session namespace directory,
	// e.g. /home/user/project/.claw/sessions/a1b2c3d4e5f60718/
	sessionsRoot string
	// workspaceRoot is the canonical workspace path that was fingerprinted.
	workspaceRoot string
}

// SessionHandle references a session by ID and its on-disk path.
type SessionHandle struct {
	ID   string
	Path string
}

// ManagedSessionSummary is a lightweight summary of a managed session.
type ManagedSessionSummary struct {
	ID   string
	Path string
	// ModifiedEpochMillis is the file modification time in milliseconds since
	// the Unix epoch. Rust uses u128 but int64 covers 292 million years;
	// this struct is never serialized so there is no wire-format impact.
	ModifiedEpochMillis int64
	MessageCount        int
	// ParentSessionID is the session ID of the parent when this session was
	// forked. Empty string when unset (Rust uses Option<String>). Idiomatic
	// Go zero-value convention; struct is in-memory only.
	ParentSessionID string
	// BranchName is the human-readable branch label for forked sessions.
	// Empty string when unset (Rust uses Option<String>). Idiomatic Go
	// zero-value convention; struct is in-memory only.
	BranchName string
}

// LoadedManagedSession wraps a fully loaded session with its handle.
type LoadedManagedSession struct {
	Handle  SessionHandle
	Session *Session
}

// ForkedManagedSession represents a newly forked session with lineage.
type ForkedManagedSession struct {
	ParentSessionID string
	Handle          SessionHandle
	Session         *Session
	BranchName      string // empty if not named
}

// SessionControlErrorKind categorizes session control errors.
type SessionControlErrorKind int

const (
	SessionControlIO SessionControlErrorKind = iota
	SessionControlSession
	SessionControlFormat
)

// SessionControlError is a structured error for session control operations.
type SessionControlError struct {
	Kind    SessionControlErrorKind
	Message string
	Cause   error
}

func (e *SessionControlError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *SessionControlError) Unwrap() error {
	return e.Cause
}

func newSessionControlIOError(msg string, cause error) *SessionControlError {
	return &SessionControlError{Kind: SessionControlIO, Message: msg, Cause: cause}
}

func newSessionControlFormatError(msg string) *SessionControlError {
	return &SessionControlError{Kind: SessionControlFormat, Message: msg}
}

func newSessionControlSessionError(msg string, cause error) *SessionControlError {
	return &SessionControlError{Kind: SessionControlSession, Message: msg, Cause: cause}
}

// WorkspaceFingerprint produces a stable 16-char hex digest of the workspace
// path using FNV-1a (64-bit). This matches the Rust implementation exactly
// (same constants: offset=0xcbf29ce484222325, prime=0x100000001b3).
func WorkspaceFingerprint(workspaceRoot string) string {
	h := fnv.New64a()
	h.Write([]byte(workspaceRoot))
	return fmt.Sprintf("%016x", h.Sum64())
}

// NewSessionStoreFromCWD builds a SessionStore from the server's current
// working directory. The on-disk layout becomes <cwd>/.claw/sessions/<hash>/.
func NewSessionStoreFromCWD(cwd string) (*SessionStore, error) {
	sessionsRoot := filepath.Join(cwd, ".claw", "sessions", WorkspaceFingerprint(cwd))
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		return nil, newSessionControlIOError("create sessions dir", err)
	}
	return &SessionStore{
		sessionsRoot:  sessionsRoot,
		workspaceRoot: cwd,
	}, nil
}

// NewSessionStoreFromDataDir builds a SessionStore from an explicit data
// directory and workspace root. Layout: <dataDir>/sessions/<hash>/.
func NewSessionStoreFromDataDir(dataDir, workspaceRoot string) (*SessionStore, error) {
	sessionsRoot := filepath.Join(dataDir, "sessions", WorkspaceFingerprint(workspaceRoot))
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		return nil, newSessionControlIOError("create sessions dir", err)
	}
	return &SessionStore{
		sessionsRoot:  sessionsRoot,
		workspaceRoot: workspaceRoot,
	}, nil
}

// SessionsDir returns the fully resolved sessions directory for this namespace.
func (s *SessionStore) SessionsDir() string {
	return s.sessionsRoot
}

// WorkspaceRoot returns the workspace root this store is bound to.
func (s *SessionStore) WorkspaceRoot() string {
	return s.workspaceRoot
}

// CreateHandle creates a SessionHandle for the given session ID.
func (s *SessionStore) CreateHandle(sessionID string) SessionHandle {
	return SessionHandle{
		ID:   sessionID,
		Path: filepath.Join(s.sessionsRoot, sessionID+"."+PrimarySessionExtension),
	}
}

// ResolveReference resolves a session reference (alias, path, or direct ID)
// to a SessionHandle.
func (s *SessionStore) ResolveReference(reference string) (SessionHandle, error) {
	if IsSessionReferenceAlias(reference) {
		latest, err := s.LatestSession()
		if err != nil {
			return SessionHandle{}, err
		}
		return SessionHandle{ID: latest.ID, Path: latest.Path}, nil
	}

	// Check if it looks like a path (has extension or multiple components).
	candidate := reference
	if !filepath.IsAbs(reference) {
		candidate = filepath.Join(s.workspaceRoot, reference)
	}
	looksLikePath := filepath.Ext(reference) != "" || strings.Contains(reference, string(filepath.Separator))
	if _, err := os.Stat(candidate); err == nil {
		id := SessionIDFromPath(candidate)
		if id == "" {
			id = reference
		}
		return SessionHandle{ID: id, Path: candidate}, nil
	} else if looksLikePath {
		return SessionHandle{}, newSessionControlFormatError(FormatMissingSessionReference(reference))
	}

	// Try resolving as a managed session ID.
	path, err := s.ResolveManagedPath(reference)
	if err != nil {
		return SessionHandle{}, err
	}
	id := SessionIDFromPath(path)
	if id == "" {
		id = reference
	}
	return SessionHandle{ID: id, Path: path}, nil
}

// ResolveManagedPath resolves a session ID to its on-disk path, checking
// .jsonl (primary) first, then .json (legacy fallback).
func (s *SessionStore) ResolveManagedPath(sessionID string) (string, error) {
	for _, ext := range []string{PrimarySessionExtension, LegacySessionExtension} {
		path := filepath.Join(s.sessionsRoot, sessionID+"."+ext)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", newSessionControlFormatError(FormatMissingSessionReference(sessionID))
}

// ListSessions returns all managed sessions in this namespace, sorted by
// modification time (newest first).
func (s *SessionStore) ListSessions() ([]ManagedSessionSummary, error) {
	return listManagedSessionsInDir(s.sessionsRoot)
}

// LatestSession returns the most recently modified session.
func (s *SessionStore) LatestSession() (ManagedSessionSummary, error) {
	sessions, err := s.ListSessions()
	if err != nil {
		return ManagedSessionSummary{}, err
	}
	if len(sessions) == 0 {
		return ManagedSessionSummary{}, newSessionControlFormatError(FormatNoManagedSessions())
	}
	return sessions[0], nil
}

// LoadSession resolves a reference and loads the full session.
func (s *SessionStore) LoadSession(reference string) (LoadedManagedSession, error) {
	handle, err := s.ResolveReference(reference)
	if err != nil {
		return LoadedManagedSession{}, err
	}
	session, err := loadSessionFromPath(handle.Path)
	if err != nil {
		return LoadedManagedSession{}, newSessionControlSessionError("load session", err)
	}
	return LoadedManagedSession{
		Handle: SessionHandle{
			ID:   session.ID,
			Path: handle.Path,
		},
		Session: session,
	}, nil
}

// ForkSession forks a session within the same namespace.
func (s *SessionStore) ForkSession(session *Session, branchName string) (ForkedManagedSession, error) {
	parentSessionID := session.ID
	forked := session.ForkSession(branchName)
	handle := s.CreateHandle(forked.ID)

	bn := ""
	if forked.Fork != nil {
		bn = forked.Fork.BranchName
	}

	if err := SaveSessionJSONL(s.sessionsRoot, forked); err != nil {
		return ForkedManagedSession{}, newSessionControlSessionError("save forked session", err)
	}

	return ForkedManagedSession{
		ParentSessionID: parentSessionID,
		Handle:          handle,
		Session:         forked,
		BranchName:      bn,
	}, nil
}

// --- Free functions ---

// SessionsDir returns the managed sessions directory for the current directory.
func SessionsDirFromCWD() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", newSessionControlIOError("get cwd", err)
	}
	return ManagedSessionsDirFor(cwd)
}

// ManagedSessionsDirFor returns the sessions directory for a base directory,
// creating it if it doesn't exist.
func ManagedSessionsDirFor(baseDir string) (string, error) {
	path := filepath.Join(baseDir, ".claw", "sessions")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", newSessionControlIOError("create sessions dir", err)
	}
	return path, nil
}

// CreateManagedSessionHandle creates a session handle for the current directory.
func CreateManagedSessionHandle(sessionID string) (SessionHandle, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return SessionHandle{}, newSessionControlIOError("get cwd", err)
	}
	return CreateManagedSessionHandleFor(cwd, sessionID)
}

// CreateManagedSessionHandleFor creates a session handle for a given base directory.
func CreateManagedSessionHandleFor(baseDir, sessionID string) (SessionHandle, error) {
	dir, err := ManagedSessionsDirFor(baseDir)
	if err != nil {
		return SessionHandle{}, err
	}
	return SessionHandle{
		ID:   sessionID,
		Path: filepath.Join(dir, sessionID+"."+PrimarySessionExtension),
	}, nil
}

// ResolveSessionReference resolves a reference from the current directory.
func ResolveSessionReference(reference string) (SessionHandle, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return SessionHandle{}, newSessionControlIOError("get cwd", err)
	}
	return ResolveSessionReferenceFor(cwd, reference)
}

// ResolveSessionReferenceFor resolves a reference from a given base directory.
func ResolveSessionReferenceFor(baseDir, reference string) (SessionHandle, error) {
	if IsSessionReferenceAlias(reference) {
		latest, err := LatestManagedSessionFor(baseDir)
		if err != nil {
			return SessionHandle{}, err
		}
		return SessionHandle{ID: latest.ID, Path: latest.Path}, nil
	}

	candidate := reference
	if !filepath.IsAbs(reference) {
		candidate = filepath.Join(baseDir, reference)
	}
	looksLikePath := filepath.Ext(reference) != "" || strings.Contains(reference, string(filepath.Separator))
	if _, err := os.Stat(candidate); err == nil {
		id := SessionIDFromPath(candidate)
		if id == "" {
			id = reference
		}
		return SessionHandle{ID: id, Path: candidate}, nil
	} else if looksLikePath {
		return SessionHandle{}, newSessionControlFormatError(FormatMissingSessionReference(reference))
	}

	path, err := ResolveManagedSessionPathFor(baseDir, reference)
	if err != nil {
		return SessionHandle{}, err
	}
	id := SessionIDFromPath(path)
	if id == "" {
		id = reference
	}
	return SessionHandle{ID: id, Path: path}, nil
}

// ResolveManagedSessionPath resolves a session path from the current directory.
func ResolveManagedSessionPath(sessionID string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", newSessionControlIOError("get cwd", err)
	}
	return ResolveManagedSessionPathFor(cwd, sessionID)
}

// ResolveManagedSessionPathFor resolves a session path from a given base directory.
func ResolveManagedSessionPathFor(baseDir, sessionID string) (string, error) {
	dir, err := ManagedSessionsDirFor(baseDir)
	if err != nil {
		return "", err
	}
	for _, ext := range []string{PrimarySessionExtension, LegacySessionExtension} {
		path := filepath.Join(dir, sessionID+"."+ext)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", newSessionControlFormatError(FormatMissingSessionReference(sessionID))
}

// ListManagedSessions lists all sessions from the current directory.
func ListManagedSessions() ([]ManagedSessionSummary, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, newSessionControlIOError("get cwd", err)
	}
	return ListManagedSessionsFor(cwd)
}

// ListManagedSessionsFor lists all sessions from a given base directory.
func ListManagedSessionsFor(baseDir string) ([]ManagedSessionSummary, error) {
	dir, err := ManagedSessionsDirFor(baseDir)
	if err != nil {
		return nil, err
	}
	return listManagedSessionsInDir(dir)
}

// LatestManagedSession returns the most recent session from the current directory.
func LatestManagedSession() (ManagedSessionSummary, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return ManagedSessionSummary{}, newSessionControlIOError("get cwd", err)
	}
	return LatestManagedSessionFor(cwd)
}

// LatestManagedSessionFor returns the most recent session from a given base directory.
func LatestManagedSessionFor(baseDir string) (ManagedSessionSummary, error) {
	sessions, err := ListManagedSessionsFor(baseDir)
	if err != nil {
		return ManagedSessionSummary{}, err
	}
	if len(sessions) == 0 {
		return ManagedSessionSummary{}, newSessionControlFormatError(FormatNoManagedSessions())
	}
	return sessions[0], nil
}

// LoadManagedSession loads a session from the current directory by reference.
func LoadManagedSession(reference string) (LoadedManagedSession, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return LoadedManagedSession{}, newSessionControlIOError("get cwd", err)
	}
	return LoadManagedSessionFor(cwd, reference)
}

// LoadManagedSessionFor loads a session from a given base directory by reference.
func LoadManagedSessionFor(baseDir, reference string) (LoadedManagedSession, error) {
	handle, err := ResolveSessionReferenceFor(baseDir, reference)
	if err != nil {
		return LoadedManagedSession{}, err
	}
	session, err := loadSessionFromPath(handle.Path)
	if err != nil {
		return LoadedManagedSession{}, newSessionControlSessionError("load session", err)
	}
	return LoadedManagedSession{
		Handle: SessionHandle{
			ID:   session.ID,
			Path: handle.Path,
		},
		Session: session,
	}, nil
}

// ForkManagedSession forks a session in the current directory.
func ForkManagedSession(session *Session, branchName string) (ForkedManagedSession, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return ForkedManagedSession{}, newSessionControlIOError("get cwd", err)
	}
	return ForkManagedSessionFor(cwd, session, branchName)
}

// ForkManagedSessionFor forks a session in a given base directory.
func ForkManagedSessionFor(baseDir string, session *Session, branchName string) (ForkedManagedSession, error) {
	parentSessionID := session.ID
	forked := session.ForkSession(branchName)
	handle, err := CreateManagedSessionHandleFor(baseDir, forked.ID)
	if err != nil {
		return ForkedManagedSession{}, err
	}

	bn := ""
	if forked.Fork != nil {
		bn = forked.Fork.BranchName
	}

	dir := filepath.Dir(handle.Path)
	if err := SaveSessionJSONL(dir, forked); err != nil {
		return ForkedManagedSession{}, newSessionControlSessionError("save forked session", err)
	}

	return ForkedManagedSession{
		ParentSessionID: parentSessionID,
		Handle:          handle,
		Session:         forked,
		BranchName:      bn,
	}, nil
}

// --- Utility functions ---

// IsManagedSessionFile returns true if the path has a .jsonl or .json extension.
func IsManagedSessionFile(path string) bool {
	ext := filepath.Ext(path)
	return ext == "."+PrimarySessionExtension || ext == "."+LegacySessionExtension
}

// IsSessionReferenceAlias returns true if the reference is one of the
// case-insensitive aliases (latest, last, recent).
func IsSessionReferenceAlias(reference string) bool {
	for _, alias := range sessionReferenceAliases {
		if strings.EqualFold(reference, alias) {
			return true
		}
	}
	return false
}

// SessionIDFromPath extracts the session ID from a file path by stripping
// the .jsonl or .json extension.
func SessionIDFromPath(path string) string {
	base := filepath.Base(path)
	for _, ext := range []string{"." + PrimarySessionExtension, "." + LegacySessionExtension} {
		if strings.HasSuffix(base, ext) {
			return strings.TrimSuffix(base, ext)
		}
	}
	return ""
}

// FormatMissingSessionReference returns a helpful error message for a missing
// session reference.
func FormatMissingSessionReference(reference string) string {
	return fmt.Sprintf(
		"session not found: %s\nHint: managed sessions live in .claw/sessions/. Try `%s` for the most recent session or `/session list` in the REPL.",
		reference, LatestSessionReference,
	)
}

// FormatNoManagedSessions returns a helpful error message when no managed
// sessions exist.
func FormatNoManagedSessions() string {
	return fmt.Sprintf(
		"no managed sessions found in .claw/sessions/\nStart `claw` to create a session, then rerun with `--resume %s`.",
		LatestSessionReference,
	)
}

// --- Internal helpers ---

// listManagedSessionsInDir scans a directory for session files and returns
// summaries sorted by modification time (newest first).
func listManagedSessionsInDir(dir string) ([]ManagedSessionSummary, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, newSessionControlIOError("read sessions dir", err)
	}

	var sessions []ManagedSessionSummary
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !IsManagedSessionFile(path) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		modifiedMs := info.ModTime().UnixMilli()

		// Try loading the session to extract metadata.
		id := SessionIDFromPath(path)
		messageCount := 0
		parentSessionID := ""
		branchName := ""

		session, err := loadSessionFromPath(path)
		if err == nil {
			id = session.ID
			messageCount = len(session.Messages)
			if session.Fork != nil {
				parentSessionID = session.Fork.ParentSessionID
				branchName = session.Fork.BranchName
			}
		} else if id == "" {
			id = "unknown"
		}

		sessions = append(sessions, ManagedSessionSummary{
			ID:                  id,
			Path:                path,
			ModifiedEpochMillis: modifiedMs,
			MessageCount:        messageCount,
			ParentSessionID:     parentSessionID,
			BranchName:          branchName,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].ModifiedEpochMillis != sessions[j].ModifiedEpochMillis {
			return sessions[i].ModifiedEpochMillis > sessions[j].ModifiedEpochMillis
		}
		return sessions[i].ID > sessions[j].ID
	})

	return sessions, nil
}

// loadSessionFromPath loads a session from a file path, supporting both
// JSONL and legacy JSON formats via ParseJSONL's first-byte detection.
func loadSessionFromPath(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseJSONL(data)
}
