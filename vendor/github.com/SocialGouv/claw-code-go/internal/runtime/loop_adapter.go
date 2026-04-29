package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/plugins"
)

// LoopAdapter wraps a *ConversationLoop and implements the interface contracts
// required by slash command handlers. Each method nil-checks its subsystem
// and returns a typed LoopError rather than panicking.
type LoopAdapter struct {
	loop *ConversationLoop

	// Subsystem availability flags — checked once at construction.
	hasUsage   bool
	hasMCP     bool
	hasPermMgr bool

	// turnMu guards session-mutating operations during active turns.
	turnMu    sync.Mutex
	turnCount int // >0 means a turn is running

	// Session-level state for features not on ConversationLoop.
	tags          []string
	pinnedIndices []int
	bookmarks     []Bookmark
	focusPaths    []string
	chatMode      bool
	planMode      bool
	temperature   float64
	reasoningMode string
	tokenBudget   string
	rateLimitRPM  string
	notifications []string
	toggles       map[string]bool
	startTime     time.Time

	// Build-time info, set via SetBuildInfo().
	buildVersion string
	buildCommit  string

	// Marketplace plugin manager. Optional — wired by main when a
	// marketplace URL is configured. Used by the /store slash command.
	pluginManager *plugins.Manager
}

// Bookmark records a labeled position in conversation history.
type Bookmark struct {
	Label        string    `json:"label"`
	MessageIndex int       `json:"message_index"`
	CreatedAt    time.Time `json:"created_at"`
}

// NewLoopAdapter creates a LoopAdapter with subsystem availability flags.
// All fields on loop may be nil — the adapter never panics.
func NewLoopAdapter(loop *ConversationLoop) *LoopAdapter {
	if loop == nil {
		return &LoopAdapter{
			toggles:   make(map[string]bool),
			startTime: time.Now(),
		}
	}
	return &LoopAdapter{
		loop:       loop,
		hasUsage:   loop.Usage != nil,
		hasMCP:     loop.MCPRegistry != nil,
		hasPermMgr: loop.PermManager != nil,
		toggles:    make(map[string]bool),
		startTime:  time.Now(),
	}
}

// SetBuildInfo records the build-time version and commit for the adapter.
// Call this after construction with values from the ldflags-injected main vars.
func (a *LoopAdapter) SetBuildInfo(version, commit string) {
	a.buildVersion = version
	a.buildCommit = commit
}

// MarkTurnStarted increments the active turn counter.
func (a *LoopAdapter) MarkTurnStarted() {
	a.turnMu.Lock()
	a.turnCount++
	a.turnMu.Unlock()
}

// MarkTurnFinished decrements the active turn counter.
func (a *LoopAdapter) MarkTurnFinished() {
	a.turnMu.Lock()
	if a.turnCount > 0 {
		a.turnCount--
	}
	a.turnMu.Unlock()
}

// isTurnActive returns true if a turn is currently running.
func (a *LoopAdapter) isTurnActive() bool {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	return a.turnCount > 0
}

// requireNoActiveTurn returns ErrTurnActive if a turn is in progress.
func (a *LoopAdapter) requireNoActiveTurn() error {
	if a.isTurnActive() {
		return ErrTurnActive
	}
	return nil
}

// requireLoop returns an error if the underlying loop is nil.
func (a *LoopAdapter) requireLoop() error {
	if a.loop == nil {
		return NewLoopError(LoopErrSubsystemUnavailable, "loop", "conversation loop is nil")
	}
	return nil
}

// --- SessionManager interface ---

// SessionDir returns the directory holding saved sessions, so read-only
// callers (e.g. timeline / lineage renderers) can iterate JSON files
// without needing a SessionManager wrapper.
func (a *LoopAdapter) SessionDir() string {
	if a.loop == nil || a.loop.Config == nil {
		return ""
	}
	return a.loop.Config.SessionDir
}

// ListSessions returns saved session IDs.
func (a *LoopAdapter) ListSessions() ([]string, error) {
	if err := a.requireLoop(); err != nil {
		return nil, err
	}
	return ListSessions(a.loop.Config.SessionDir)
}

// LoadSession switches to the session with the given ID. Mutating — rejected during active turn.
func (a *LoopAdapter) LoadSession(id string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if err := a.requireNoActiveTurn(); err != nil {
		return err
	}
	sess, err := LoadSession(a.loop.Config.SessionDir, id)
	if err != nil {
		return &LoopError{
			Kind:      LoopErrSessionNotFound,
			Subsystem: "session",
			Message:   fmt.Sprintf("session %q not found", id),
			Cause:     err,
		}
	}
	a.loop.Session = sess
	// Restore usage from session.
	if a.hasUsage {
		a.loop.Usage.TotalInput = sess.TotalInputTokens
		a.loop.Usage.TotalOutput = sess.TotalOutputTokens
		a.loop.Usage.Turns = sess.TotalTurns
	}
	return nil
}

// ForkSession creates a fork of the current session. Mutating — rejected during active turn.
func (a *LoopAdapter) ForkSession(name string) (string, error) {
	if err := a.requireLoop(); err != nil {
		return "", err
	}
	if err := a.requireNoActiveTurn(); err != nil {
		return "", err
	}
	if a.loop.Session == nil {
		return "", NewLoopError(LoopErrSubsystemUnavailable, "session", "no active session")
	}
	forked := a.loop.Session.ForkSession(name)
	// Save the fork.
	if err := SaveSession(a.loop.Config.SessionDir, forked); err != nil {
		return "", WrapLoopError(LoopErrSessionNotFound, "session", "failed to save forked session", err)
	}
	// Switch to the fork.
	a.loop.Session = forked
	return forked.ID, nil
}

// DeleteSession removes a saved session. Mutating — rejected during active turn.
func (a *LoopAdapter) DeleteSession(id string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if err := a.requireNoActiveTurn(); err != nil {
		return err
	}
	path := filepath.Join(a.loop.Config.SessionDir, id+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return &LoopError{
				Kind:      LoopErrSessionNotFound,
				Subsystem: "session",
				Message:   fmt.Sprintf("session %q not found", id),
				Cause:     err,
			}
		}
		return WrapLoopError(LoopErrSessionNotFound, "session", "failed to delete session", err)
	}
	return nil
}

// RenameSession renames the current session. Mutating — rejected during active turn.
func (a *LoopAdapter) RenameSession(name string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if err := a.requireNoActiveTurn(); err != nil {
		return err
	}
	if a.loop.Session == nil {
		return NewLoopError(LoopErrSubsystemUnavailable, "session", "no active session")
	}
	oldPath := filepath.Join(a.loop.Config.SessionDir, a.loop.Session.ID+".json")
	a.loop.Session.ID = name
	// Save under new name.
	if err := SaveSession(a.loop.Config.SessionDir, a.loop.Session); err != nil {
		return err
	}
	// Remove old file (best-effort).
	_ = os.Remove(oldPath)
	return nil
}

// --- History / Export ---

// ShowHistory returns the last count messages formatted as strings.
func (a *LoopAdapter) ShowHistory(count int) ([]string, error) {
	if err := a.requireLoop(); err != nil {
		return nil, err
	}
	if a.loop.Session == nil {
		return nil, nil
	}
	msgs := a.loop.Session.Messages
	start := 0
	if len(msgs) > count {
		start = len(msgs) - count
	}
	var entries []string
	for i, m := range msgs[start:] {
		idx := start + i
		preview := ""
		if len(m.Content) > 0 && m.Content[0].Type == "text" {
			preview = m.Content[0].Text
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
		}
		entries = append(entries, fmt.Sprintf("[%d] %s: %s", idx, m.Role, preview))
	}
	return entries, nil
}

// ExportConversation exports the session to a JSON file.
func (a *LoopAdapter) ExportConversation(path string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if a.loop.Session == nil {
		return NewLoopError(LoopErrSubsystemUnavailable, "session", "no active session")
	}
	data, err := json.MarshalIndent(a.loop.Session, "", "  ")
	if err != nil {
		return fmt.Errorf("export: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// SummarizeConversation returns a brief summary of the conversation.
func (a *LoopAdapter) SummarizeConversation() (string, error) {
	if err := a.requireLoop(); err != nil {
		return "", err
	}
	if a.loop.Session == nil {
		return "No active session.", nil
	}
	msgs := a.loop.Session.Messages
	userCount, assistantCount, toolCount := 0, 0, 0
	for _, m := range msgs {
		switch m.Role {
		case "user":
			userCount++
		case "assistant":
			assistantCount++
		}
		for _, c := range m.Content {
			if c.Type == "tool_use" || c.Type == "tool_result" {
				toolCount++
			}
		}
	}
	return fmt.Sprintf("Session %s: %d messages (%d user, %d assistant), %d tool interactions, %d compactions",
		a.loop.Session.ID, len(msgs), userCount, assistantCount, toolCount,
		a.loop.Session.CompactionCount), nil
}

// --- Tags ---

// TagConversation adds a tag at the current conversation point.
func (a *LoopAdapter) TagConversation(label string) error {
	a.tags = append(a.tags, label)
	return nil
}

// --- Pins ---

// PinMessage pins a message index to persist across compaction.
func (a *LoopAdapter) PinMessage(index int) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if a.loop.Session == nil || index >= len(a.loop.Session.Messages) {
		return NewLoopError(LoopErrInvalidArgs, "session", "message index out of range")
	}
	// Avoid duplicates.
	for _, idx := range a.pinnedIndices {
		if idx == index {
			return nil
		}
	}
	a.pinnedIndices = append(a.pinnedIndices, index)
	return nil
}

// UnpinMessage unpins a previously pinned message.
func (a *LoopAdapter) UnpinMessage(index int) error {
	for i, idx := range a.pinnedIndices {
		if idx == index {
			a.pinnedIndices = append(a.pinnedIndices[:i], a.pinnedIndices[i+1:]...)
			return nil
		}
	}
	return NewLoopError(LoopErrInvalidArgs, "session", fmt.Sprintf("message %d is not pinned", index))
}

// --- Bookmarks ---

// ListBookmarks returns bookmark labels.
func (a *LoopAdapter) ListBookmarks() ([]string, error) {
	labels := make([]string, len(a.bookmarks))
	for i, b := range a.bookmarks {
		labels[i] = fmt.Sprintf("%s (message %d)", b.Label, b.MessageIndex)
	}
	return labels, nil
}

// AddBookmark adds a bookmark at the current position.
func (a *LoopAdapter) AddBookmark(label string) error {
	msgIdx := 0
	if a.loop != nil && a.loop.Session != nil {
		msgIdx = len(a.loop.Session.Messages) - 1
		if msgIdx < 0 {
			msgIdx = 0
		}
	}
	a.bookmarks = append(a.bookmarks, Bookmark{
		Label:        label,
		MessageIndex: msgIdx,
		CreatedAt:    time.Now(),
	})
	return nil
}

// RemoveBookmark removes a bookmark by label.
func (a *LoopAdapter) RemoveBookmark(label string) error {
	for i, b := range a.bookmarks {
		if b.Label == label {
			a.bookmarks = append(a.bookmarks[:i], a.bookmarks[i+1:]...)
			return nil
		}
	}
	return NewLoopError(LoopErrInvalidArgs, "session", fmt.Sprintf("bookmark %q not found", label))
}

// --- Focus ---

// AddFocus adds a path to the focus context.
func (a *LoopAdapter) AddFocus(path string) error {
	for _, p := range a.focusPaths {
		if p == path {
			return nil // already focused
		}
	}
	a.focusPaths = append(a.focusPaths, path)
	return nil
}

// RemoveFocus removes a path from the focus context. Empty path clears all.
func (a *LoopAdapter) RemoveFocus(path string) error {
	if path == "" {
		a.focusPaths = nil
		return nil
	}
	for i, p := range a.focusPaths {
		if p == path {
			a.focusPaths = append(a.focusPaths[:i], a.focusPaths[i+1:]...)
			return nil
		}
	}
	return nil
}

// --- Directory ---

// AddDirectory adds a directory to the context assembler.
func (a *LoopAdapter) AddDirectory(path string) error {
	// Store in focus paths for now; context assembler integration is future work.
	return a.AddFocus(path)
}

// --- ClearSession ---

// ClearSession resets the conversation history.
func (a *LoopAdapter) ClearSession() {
	if a.loop != nil {
		a.loop.ClearSession()
	}
}

// --- MessageCount ---

// MessageCount returns the number of messages in the active session.
func (a *LoopAdapter) MessageCount() int {
	if a.loop != nil {
		return a.loop.MessageCount()
	}
	return 0
}

// --- PluginManagerProvider ---

// SetPluginManager installs a marketplace plugin manager. Called by main
// at boot when a marketplace endpoint is configured. Slash commands
// (/store) check for this via the PluginManager() accessor.
func (a *LoopAdapter) SetPluginManager(m *plugins.Manager) {
	a.pluginManager = m
}

// PluginManager returns the registered marketplace plugin manager, or
// nil when none is configured. The /store slash command treats nil as
// "manager not available in this context".
func (a *LoopAdapter) PluginManager() *plugins.Manager {
	return a.pluginManager
}
