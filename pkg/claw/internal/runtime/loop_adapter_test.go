package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/plugins"
	"github.com/SocialGouv/iterion/pkg/claw/internal/usage"
)

// --- Compile-time interface checks ---
// These verify that LoopAdapter satisfies all command handler interfaces.

// SessionManager from session_cmds.go
var _ interface {
	ListSessions() ([]string, error)
	ForkSession(name string) (string, error)
	LoadSession(id string) error
	DeleteSession(id string) error
} = (*LoopAdapter)(nil)

// SessionDirProvider from session_timeline.go — read-only seam used by
// /timeline and /lineage to locate the session JSON store.
var _ interface {
	SessionDir() string
} = (*LoopAdapter)(nil)

// PluginManagerProvider from plugin_marketplace.go — exposes the
// marketplace manager to the /store slash command.
var _ interface {
	PluginManager() *plugins.Manager
} = (*LoopAdapter)(nil)

// sessionRenamer from session_cmds.go
var _ interface {
	RenameSession(name string) error
} = (*LoopAdapter)(nil)

// historyViewer from session_cmds.go
var _ interface {
	ShowHistory(count int) ([]string, error)
} = (*LoopAdapter)(nil)

// exporter from session_cmds.go
var _ interface {
	ExportConversation(path string) error
} = (*LoopAdapter)(nil)

// summarizer from session_cmds.go
var _ interface {
	SummarizeConversation() (string, error)
} = (*LoopAdapter)(nil)

// tagger from session_cmds.go
var _ interface {
	TagConversation(label string) error
} = (*LoopAdapter)(nil)

// pinner from session_cmds.go
var _ interface {
	PinMessage(index int) error
} = (*LoopAdapter)(nil)

// unpinner from session_cmds.go
var _ interface {
	UnpinMessage(index int) error
} = (*LoopAdapter)(nil)

// bookmarkManager from session_cmds.go
var _ interface {
	ListBookmarks() ([]string, error)
	AddBookmark(label string) error
	RemoveBookmark(label string) error
} = (*LoopAdapter)(nil)

// focusManager from session_cmds.go
var _ interface {
	AddFocus(path string) error
} = (*LoopAdapter)(nil)

// removeFocusManager from session_cmds.go
var _ interface {
	RemoveFocus(path string) error
} = (*LoopAdapter)(nil)

// dirAdder from session_cmds.go
var _ interface {
	AddDirectory(path string) error
} = (*LoopAdapter)(nil)

// sessionHolder from registry.go builtins
var _ interface {
	ClearSession()
} = (*LoopAdapter)(nil)

// UsageTracker from status_cmds.go
var _ interface {
	ModelName() string
	TurnCount() int
	InputTokens() int
	OutputTokens() int
	EstimatedCostUSD() float64
} = (*LoopAdapter)(nil)

// statsProvider from status_cmds.go
var _ interface {
	SessionDuration() string
	TurnCount() int
	InputTokens() int
	OutputTokens() int
	ToolCallCount() int
} = (*LoopAdapter)(nil)

// cacheTracker from status_cmds.go
var _ interface {
	CacheHits() int
	CacheMisses() int
	CachedTokens() int
} = (*LoopAdapter)(nil)

// providerLister from status_cmds.go
var _ interface {
	ListProviders() []string
	ActiveProvider() string
} = (*LoopAdapter)(nil)

// metricsProvider from status_cmds.go
var _ interface {
	AvgResponseTime() float64
	TotalRequests() int
	ErrorRate() float64
} = (*LoopAdapter)(nil)

// versionProvider from status_cmds.go
var _ interface {
	Version() string
	Commit() string
} = (*LoopAdapter)(nil)

// changelogProvider from status_cmds.go
var _ interface {
	RecentChanges(count int) []string
} = (*LoopAdapter)(nil)

// notificationManager from status_cmds.go
var _ interface {
	ListNotifications() []string
	ClearNotifications() error
} = (*LoopAdapter)(nil)

// ConfigSwitcher from config_cmds.go
var _ interface {
	CurrentModel() string
	SetModel(model string) error
	CurrentPermissionMode() string
	SetPermissionMode(mode string) error
} = (*LoopAdapter)(nil)

// planToggler from config_cmds.go
var _ interface {
	PlanMode() bool
	SetPlanMode(on bool)
} = (*LoopAdapter)(nil)

// tempController from config_cmds.go
var _ interface {
	GetTemperature() float64
	SetTemperature(t float64) error
} = (*LoopAdapter)(nil)

// tokenController from config_cmds.go
var _ interface {
	GetMaxTokens() int
	SetMaxTokens(n int) error
} = (*LoopAdapter)(nil)

// sysPromptManager from config_cmds.go
var _ interface {
	GetSystemPrompt() string
	SetSystemPrompt(prompt string) error
} = (*LoopAdapter)(nil)

// reasoningController from config_cmds.go
var _ interface {
	SetReasoningMode(mode string) error
	GetReasoningMode() string
} = (*LoopAdapter)(nil)

// budgetController from config_cmds.go
var _ interface {
	GetBudget() (string, error)
	SetBudget(limit string) error
} = (*LoopAdapter)(nil)

// rateLimitController from config_cmds.go
var _ interface {
	GetRateLimit() (string, error)
	SetRateLimit(rpm string) error
} = (*LoopAdapter)(nil)

// profileManager from config_cmds.go
var _ interface {
	CurrentProfile() string
	SwitchProfile(name string) error
	ListProfiles() []string
} = (*LoopAdapter)(nil)

// languageManager from config_cmds.go
var _ interface {
	GetLanguage() string
	SetLanguage(lang string) error
} = (*LoopAdapter)(nil)

// apiKeyManager from config_cmds.go
var _ interface {
	GetAPIKey() string
	SetAPIKey(key string) error
} = (*LoopAdapter)(nil)

// toolsManager from config_cmds.go
var _ interface {
	ListAllowedTools() ([]string, error)
	AddAllowedTool(name string) error
	RemoveAllowedTool(name string) error
} = (*LoopAdapter)(nil)

// toggleLoop from ux_cmds.go / config_cmds.go
var _ interface {
	GetToggle(name string) bool
	SetToggle(name string, value bool)
} = (*LoopAdapter)(nil)

// compactor from config_cmds.go
var _ interface {
	CompactSession() error
} = (*LoopAdapter)(nil)

// promptInjector from code_cmds.go
var _ interface {
	InjectPrompt(prompt string) error
} = (*LoopAdapter)(nil)

// effortLoop from ux_cmds.go
var _ interface {
	SetEffort(level string) error
	GetEffort() string
} = (*LoopAdapter)(nil)

// themeLoop from ux_cmds.go
var _ interface {
	SetTheme(name string) error
	CurrentTheme() string
	ListThemes() []string
} = (*LoopAdapter)(nil)

// contextLoop from context_cmds.go
var _ interface {
	ListContextFiles() []string
	ContextTokenBreakdown() map[string]int
	ClearContext()
} = (*LoopAdapter)(nil)

// searchLoop from context_cmds.go
var _ interface {
	SearchFiles(query string) ([]string, error)
} = (*LoopAdapter)(nil)

// rewindLoop from context_cmds.go
var _ interface {
	RewindSteps(n int) error
} = (*LoopAdapter)(nil)

// clipboardLoop from context_cmds.go
var _ interface {
	GetLastOutput() string
	GetFullConversation() string
} = (*LoopAdapter)(nil)

// symbolLister from context_cmds.go (LSP)
var _ interface {
	ListSymbols(path string) ([]string, error)
} = (*LoopAdapter)(nil)

// referenceFinder from context_cmds.go (LSP)
var _ interface {
	FindReferences(symbol string) ([]string, error)
} = (*LoopAdapter)(nil)

// definitionFinder from context_cmds.go (LSP)
var _ interface {
	FindDefinition(symbol string) (string, error)
} = (*LoopAdapter)(nil)

// hoverProvider from context_cmds.go (LSP)
var _ interface {
	GetHoverInfo(symbol string) (string, error)
} = (*LoopAdapter)(nil)

// diagnosticsProvider from context_cmds.go (LSP)
var _ interface {
	GetDiagnostics(path string) ([]string, error)
} = (*LoopAdapter)(nil)

// codeMapper from context_cmds.go
var _ interface {
	ShowCodeMap(depth int) (string, error)
} = (*LoopAdapter)(nil)

// toolDetailer from context_cmds.go
var _ interface {
	GetToolDetails(name string) (string, error)
} = (*LoopAdapter)(nil)

// --- Helper: create a test loop with temp session dir ---

func testLoopAdapter(t *testing.T) *LoopAdapter {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		Model:      "claude-test",
		MaxTokens:  4096,
		SessionDir: dir,
	}
	sess := NewSession()
	sess.Messages = []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hello"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "hi there"}}},
	}
	loop := &ConversationLoop{
		Config:  cfg,
		Session: sess,
		Usage:   usage.NewTracker("claude-test"),
	}
	return NewLoopAdapter(loop)
}

// --- Nil-safety suite ---

func TestLoopAdapter_NilLoop_NoPanic(t *testing.T) {
	a := NewLoopAdapter(nil)

	// Every method should return an error or a zero value, never panic.

	// Session
	_, err := a.ListSessions()
	assertIsLoopError(t, err, "ListSessions")
	err = a.LoadSession("x")
	assertIsLoopError(t, err, "LoadSession")
	_, err = a.ForkSession("x")
	assertIsLoopError(t, err, "ForkSession")
	err = a.DeleteSession("x")
	assertIsLoopError(t, err, "DeleteSession")
	err = a.RenameSession("x")
	assertIsLoopError(t, err, "RenameSession")
	_, err = a.ShowHistory(10)
	assertIsLoopError(t, err, "ShowHistory")
	err = a.ExportConversation("x.json")
	assertIsLoopError(t, err, "ExportConversation")
	_, err = a.SummarizeConversation()
	assertIsLoopError(t, err, "SummarizeConversation")

	// Usage - should return zero values, not panic
	if a.ModelName() != "unknown" {
		t.Error("nil loop ModelName should return 'unknown'")
	}
	if a.TurnCount() != 0 {
		t.Error("nil loop TurnCount should return 0")
	}
	if a.InputTokens() != 0 {
		t.Error("nil loop InputTokens should return 0")
	}

	// Config
	if a.CurrentModel() != "unknown" {
		t.Error("nil loop CurrentModel should return 'unknown'")
	}
	err = a.SetModel("x")
	assertIsLoopError(t, err, "SetModel")
	if a.GetMaxTokens() != DefaultMaxTokens {
		t.Errorf("nil loop GetMaxTokens should return default %d", DefaultMaxTokens)
	}
	err = a.SetMaxTokens(100)
	assertIsLoopError(t, err, "SetMaxTokens")
	err = a.SetSystemPrompt("x")
	assertIsLoopError(t, err, "SetSystemPrompt")
	err = a.SetAPIKey("x")
	assertIsLoopError(t, err, "SetAPIKey")

	// Prompt injection
	err = a.InjectPrompt("test")
	assertIsLoopError(t, err, "InjectPrompt")

	// Context
	if files := a.ListContextFiles(); files != nil {
		t.Error("nil loop ListContextFiles should return nil")
	}
	if bd := a.ContextTokenBreakdown(); bd != nil {
		t.Error("nil loop ContextTokenBreakdown should return nil")
	}
	_, err = a.SearchFiles("test")
	if err == nil {
		t.Error("nil loop SearchFiles should return error")
	}

	// Rewind
	err = a.RewindSteps(1)
	assertIsLoopError(t, err, "RewindSteps")

	// LSP methods — should return ErrNotConnected
	_, err = a.ListSymbols("x")
	assertIsLoopError(t, err, "ListSymbols")
	_, err = a.FindReferences("x")
	assertIsLoopError(t, err, "FindReferences")
	_, err = a.FindDefinition("x")
	assertIsLoopError(t, err, "FindDefinition")
	_, err = a.GetHoverInfo("x")
	assertIsLoopError(t, err, "GetHoverInfo")
	_, err = a.GetDiagnostics("x")
	assertIsLoopError(t, err, "GetDiagnostics")

	// Tags, pins, bookmarks — these don't need a loop
	if err := a.TagConversation("test"); err != nil {
		t.Errorf("TagConversation on nil loop should succeed: %v", err)
	}
	if err := a.AddBookmark("test"); err != nil {
		t.Errorf("AddBookmark on nil loop should succeed: %v", err)
	}

	// Clear and message count
	a.ClearSession() // should not panic
	if a.MessageCount() != 0 {
		t.Error("nil loop MessageCount should return 0")
	}

	// Toggles
	a.SetToggle("test", true)
	if !a.GetToggle("test") {
		t.Error("toggle should be true after SetToggle(true)")
	}
}

func assertIsLoopError(t *testing.T, err error, method string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s with nil loop should return error, got nil", method)
		return
	}
	var le *LoopError
	if !errors.As(err, &le) {
		t.Errorf("%s error should be *LoopError, got %T: %v", method, err, err)
	}
}

// --- Session management tests ---

func TestLoopAdapter_SessionDir(t *testing.T) {
	t.Run("nil loop returns empty", func(t *testing.T) {
		a := NewLoopAdapter(nil)
		if got := a.SessionDir(); got != "" {
			t.Errorf("expected empty string for nil loop, got %q", got)
		}
	})

	t.Run("loop with config returns SessionDir", func(t *testing.T) {
		dir := t.TempDir()
		loop := &ConversationLoop{Config: &Config{SessionDir: dir}}
		a := NewLoopAdapter(loop)
		if got := a.SessionDir(); got != dir {
			t.Errorf("expected %q, got %q", dir, got)
		}
	})

	t.Run("loop with nil config returns empty", func(t *testing.T) {
		loop := &ConversationLoop{}
		a := NewLoopAdapter(loop)
		if got := a.SessionDir(); got != "" {
			t.Errorf("expected empty string for nil config, got %q", got)
		}
	})
}

func TestLoopAdapter_PluginManagerSetterGetter(t *testing.T) {
	a := NewLoopAdapter(&ConversationLoop{Config: &Config{}})
	if a.PluginManager() != nil {
		t.Error("expected nil PluginManager before SetPluginManager")
	}
	mgr := &plugins.Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	a.SetPluginManager(mgr)
	if a.PluginManager() != mgr {
		t.Error("PluginManager() must return the manager set via SetPluginManager")
	}
	a.SetPluginManager(nil)
	if a.PluginManager() != nil {
		t.Error("expected nil after SetPluginManager(nil) — needed to clear out a stale manager")
	}
}

func TestLoopAdapter_SessionListForkDelete(t *testing.T) {
	a := testLoopAdapter(t)

	// Initially no saved sessions.
	sessions, err := a.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}

	// Fork the session.
	forkID, err := a.ForkSession("test-branch")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if forkID == "" {
		t.Fatal("ForkSession returned empty ID")
	}

	// Verify fork metadata.
	if a.loop.Session.Fork == nil {
		t.Fatal("forked session should have Fork metadata")
	}
	if a.loop.Session.Fork.BranchName != "test-branch" {
		t.Errorf("fork branch name = %q, want 'test-branch'", a.loop.Session.Fork.BranchName)
	}

	// Verify the fork was saved.
	sessions, err = a.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions after fork: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session after fork, got %d", len(sessions))
	}

	// Delete the fork.
	if err := a.DeleteSession(forkID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Verify deletion.
	sessions, err = a.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions after delete: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(sessions))
	}
}

func TestLoopAdapter_SessionDeleteNotFound(t *testing.T) {
	a := testLoopAdapter(t)
	err := a.DeleteSession("nonexistent-session")
	if err == nil {
		t.Fatal("DeleteSession for nonexistent session should return error")
	}
	kind, ok := LoopErrorKindOf(err)
	if !ok || kind != LoopErrSessionNotFound {
		t.Errorf("error kind should be LoopErrSessionNotFound, got %v", err)
	}
}

func TestLoopAdapter_SessionSwitch(t *testing.T) {
	a := testLoopAdapter(t)

	// Save current session so we can switch back to it.
	origID := a.loop.Session.ID
	dir := a.loop.Config.SessionDir
	if err := SaveSession(dir, a.loop.Session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// Fork to create a second session.
	_, err := a.ForkSession("other")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}

	// Switch back.
	if err := a.LoadSession(origID); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if a.loop.Session.ID != origID {
		t.Errorf("session ID after switch = %q, want %q", a.loop.Session.ID, origID)
	}
}

func TestLoopAdapter_RenameSession(t *testing.T) {
	a := testLoopAdapter(t)

	oldID := a.loop.Session.ID
	dir := a.loop.Config.SessionDir
	// Save first so there's a file to rename from.
	if err := SaveSession(dir, a.loop.Session); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	newName := "my-cool-session"
	if err := a.RenameSession(newName); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	if a.loop.Session.ID != newName {
		t.Errorf("session ID after rename = %q, want %q", a.loop.Session.ID, newName)
	}

	// Old file should be removed.
	if _, err := os.Stat(filepath.Join(dir, oldID+".json")); !os.IsNotExist(err) {
		t.Error("old session file should have been removed")
	}
	// New file should exist.
	if _, err := os.Stat(filepath.Join(dir, newName+".json")); err != nil {
		t.Error("new session file should exist")
	}
}

// --- Turn guard tests ---

func TestLoopAdapter_TurnGuardRejectsMutation(t *testing.T) {
	a := testLoopAdapter(t)

	a.MarkTurnStarted()
	defer a.MarkTurnFinished()

	// Session-mutating operations should be rejected.
	err := a.LoadSession("x")
	if !errors.Is(err, ErrTurnActive) {
		t.Errorf("LoadSession during turn: expected ErrTurnActive, got %v", err)
	}

	_, err = a.ForkSession("x")
	if !errors.Is(err, ErrTurnActive) {
		t.Errorf("ForkSession during turn: expected ErrTurnActive, got %v", err)
	}

	err = a.DeleteSession("x")
	if !errors.Is(err, ErrTurnActive) {
		t.Errorf("DeleteSession during turn: expected ErrTurnActive, got %v", err)
	}

	err = a.RenameSession("x")
	if !errors.Is(err, ErrTurnActive) {
		t.Errorf("RenameSession during turn: expected ErrTurnActive, got %v", err)
	}

	err = a.RewindSteps(1)
	if !errors.Is(err, ErrTurnActive) {
		t.Errorf("RewindSteps during turn: expected ErrTurnActive, got %v", err)
	}
}

// --- Usage tests ---

func TestLoopAdapter_UsageTracking(t *testing.T) {
	a := testLoopAdapter(t)

	if a.ModelName() != "claude-test" {
		t.Errorf("ModelName = %q, want 'claude-test'", a.ModelName())
	}

	// Add some usage.
	a.loop.Usage.Add(100, 50, 10, 5)

	if a.TurnCount() != 1 {
		t.Errorf("TurnCount = %d, want 1", a.TurnCount())
	}
	if a.InputTokens() != 100 {
		t.Errorf("InputTokens = %d, want 100", a.InputTokens())
	}
	if a.OutputTokens() != 50 {
		t.Errorf("OutputTokens = %d, want 50", a.OutputTokens())
	}
	if a.CacheHits() != 5 {
		t.Errorf("CacheHits = %d, want 5", a.CacheHits())
	}
	if a.CacheMisses() != 10 {
		t.Errorf("CacheMisses = %d, want 10", a.CacheMisses())
	}
}

// --- Config tests ---

func TestLoopAdapter_ConfigSwitching(t *testing.T) {
	a := testLoopAdapter(t)

	// Model
	if err := a.SetModel("claude-opus"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if a.CurrentModel() != "claude-opus" {
		t.Errorf("CurrentModel = %q, want 'claude-opus'", a.CurrentModel())
	}

	// Empty model should fail.
	if err := a.SetModel(""); err == nil {
		t.Error("SetModel with empty string should fail")
	}

	// Temperature
	if err := a.SetTemperature(0.5); err != nil {
		t.Fatalf("SetTemperature: %v", err)
	}
	if a.GetTemperature() != 0.5 {
		t.Errorf("GetTemperature = %f, want 0.5", a.GetTemperature())
	}
	if err := a.SetTemperature(3.0); err == nil {
		t.Error("SetTemperature(3.0) should fail (out of range)")
	}

	// Max tokens
	if err := a.SetMaxTokens(8192); err != nil {
		t.Fatalf("SetMaxTokens: %v", err)
	}
	if a.GetMaxTokens() != 8192 {
		t.Errorf("GetMaxTokens = %d, want 8192", a.GetMaxTokens())
	}
	if err := a.SetMaxTokens(-1); err == nil {
		t.Error("SetMaxTokens(-1) should fail")
	}

	// Reasoning
	if err := a.SetReasoningMode("on"); err != nil {
		t.Fatalf("SetReasoningMode: %v", err)
	}
	if a.GetReasoningMode() != "on" {
		t.Errorf("GetReasoningMode = %q, want 'on'", a.GetReasoningMode())
	}
	if err := a.SetReasoningMode("invalid"); err == nil {
		t.Error("SetReasoningMode('invalid') should fail")
	}

	// Plan mode
	a.SetPlanMode(true)
	if !a.PlanMode() {
		t.Error("PlanMode should be true")
	}
	a.SetPlanMode(false)
	if a.PlanMode() {
		t.Error("PlanMode should be false")
	}

	// Budget
	if err := a.SetBudget("10000"); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	info, err := a.GetBudget()
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if info == "" {
		t.Error("GetBudget should return non-empty string")
	}

	// Rate limit
	if err := a.SetRateLimit("60"); err != nil {
		t.Fatalf("SetRateLimit: %v", err)
	}
	rlInfo, err := a.GetRateLimit()
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if rlInfo == "" {
		t.Error("GetRateLimit should return non-empty string")
	}
}

// --- Prompt injection tests ---

func TestLoopAdapter_InjectPrompt(t *testing.T) {
	a := testLoopAdapter(t)

	initialCount := len(a.loop.Session.Messages)
	if err := a.InjectPrompt("Please explain this code"); err != nil {
		t.Fatalf("InjectPrompt: %v", err)
	}

	// Should have added a message.
	if len(a.loop.Session.Messages) != initialCount+1 {
		t.Errorf("message count = %d, want %d", len(a.loop.Session.Messages), initialCount+1)
	}

	// Injected message should be role "user".
	last := a.loop.Session.Messages[len(a.loop.Session.Messages)-1]
	if last.Role != "user" {
		t.Errorf("injected message role = %q, want 'user'", last.Role)
	}
	if last.Content[0].Text != "Please explain this code" {
		t.Errorf("injected message text mismatch")
	}

	// Should be tracked in prompt history with [injected] prefix.
	if len(a.loop.Session.PromptHistory) == 0 {
		t.Fatal("prompt history should have entry")
	}
	lastEntry := a.loop.Session.PromptHistory[len(a.loop.Session.PromptHistory)-1]
	if lastEntry.Text == "" {
		t.Error("prompt history entry text should not be empty")
	}
}

// --- Tags, pins, bookmarks tests ---

func TestLoopAdapter_TagsPinsBookmarks(t *testing.T) {
	a := testLoopAdapter(t)

	// Tags
	if err := a.TagConversation("checkpoint-1"); err != nil {
		t.Fatalf("TagConversation: %v", err)
	}
	if len(a.tags) != 1 || a.tags[0] != "checkpoint-1" {
		t.Errorf("Tags = %v, want [checkpoint-1]", a.tags)
	}

	// Pins
	if err := a.PinMessage(0); err != nil {
		t.Fatalf("PinMessage: %v", err)
	}
	if len(a.pinnedIndices) != 1 || a.pinnedIndices[0] != 0 {
		t.Errorf("PinnedIndices = %v, want [0]", a.pinnedIndices)
	}

	// Duplicate pin should be a no-op.
	if err := a.PinMessage(0); err != nil {
		t.Fatalf("PinMessage duplicate: %v", err)
	}
	if len(a.pinnedIndices) != 1 {
		t.Errorf("duplicate pin should not create second entry, got %d", len(a.pinnedIndices))
	}

	// Unpin
	if err := a.UnpinMessage(0); err != nil {
		t.Fatalf("UnpinMessage: %v", err)
	}
	if len(a.pinnedIndices) != 0 {
		t.Error("PinnedIndices should be empty after unpin")
	}

	// Unpin non-existent should error.
	if err := a.UnpinMessage(99); err == nil {
		t.Error("UnpinMessage for non-pinned index should error")
	}

	// Bookmarks
	if err := a.AddBookmark("test-bm"); err != nil {
		t.Fatalf("AddBookmark: %v", err)
	}
	labels, err := a.ListBookmarks()
	if err != nil {
		t.Fatalf("ListBookmarks: %v", err)
	}
	if len(labels) != 1 {
		t.Errorf("expected 1 bookmark, got %d", len(labels))
	}

	// Remove bookmark.
	if err := a.RemoveBookmark("test-bm"); err != nil {
		t.Fatalf("RemoveBookmark: %v", err)
	}
	labels, _ = a.ListBookmarks()
	if len(labels) != 0 {
		t.Error("bookmarks should be empty after removal")
	}

	// Remove non-existent bookmark should error.
	if err := a.RemoveBookmark("nonexistent"); err == nil {
		t.Error("RemoveBookmark for non-existent should error")
	}
}

// --- Focus tests ---

func TestLoopAdapter_Focus(t *testing.T) {
	a := testLoopAdapter(t)

	if err := a.AddFocus("/src/main.go"); err != nil {
		t.Fatalf("AddFocus: %v", err)
	}
	if len(a.focusPaths) != 1 {
		t.Errorf("expected 1 focus path, got %d", len(a.focusPaths))
	}

	// Duplicate should be no-op.
	if err := a.AddFocus("/src/main.go"); err != nil {
		t.Fatalf("AddFocus duplicate: %v", err)
	}
	if len(a.focusPaths) != 1 {
		t.Error("duplicate focus should not create second entry")
	}

	// Remove specific.
	if err := a.RemoveFocus("/src/main.go"); err != nil {
		t.Fatalf("RemoveFocus: %v", err)
	}
	if len(a.focusPaths) != 0 {
		t.Error("focus should be empty after removal")
	}

	// Add multiple, then clear all.
	_ = a.AddFocus("/a")
	_ = a.AddFocus("/b")
	if err := a.RemoveFocus(""); err != nil {
		t.Fatalf("RemoveFocus all: %v", err)
	}
	if len(a.focusPaths) != 0 {
		t.Error("focus should be empty after clear-all")
	}
}

// --- History and export tests ---

func TestLoopAdapter_ShowHistory(t *testing.T) {
	a := testLoopAdapter(t)

	entries, err := a.ShowHistory(10)
	if err != nil {
		t.Fatalf("ShowHistory: %v", err)
	}
	if len(entries) != 2 { // the test fixture has 2 messages
		t.Errorf("expected 2 history entries, got %d", len(entries))
	}
	if entries[0] == "" {
		t.Error("history entry should not be empty")
	}
}

func TestLoopAdapter_ExportConversation(t *testing.T) {
	a := testLoopAdapter(t)

	path := filepath.Join(t.TempDir(), "export.json")
	if err := a.ExportConversation(path); err != nil {
		t.Fatalf("ExportConversation: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	if len(data) == 0 {
		t.Error("exported file should not be empty")
	}
}

// --- Context tests ---

func TestLoopAdapter_Clipboard(t *testing.T) {
	a := testLoopAdapter(t)

	output := a.GetLastOutput()
	if output != "hi there" {
		t.Errorf("GetLastOutput = %q, want 'hi there'", output)
	}

	full := a.GetFullConversation()
	if full == "" {
		t.Error("GetFullConversation should return non-empty string")
	}
}

func TestLoopAdapter_Rewind(t *testing.T) {
	a := testLoopAdapter(t)

	// Start with 2 messages, rewind 1 step (removes 2 messages).
	if err := a.RewindSteps(1); err != nil {
		t.Fatalf("RewindSteps: %v", err)
	}
	if len(a.loop.Session.Messages) != 0 {
		t.Errorf("after rewind 1 step, expected 0 messages, got %d", len(a.loop.Session.Messages))
	}
}

// --- LSP graceful degradation tests ---

func TestLoopAdapter_LSP_NotConnected(t *testing.T) {
	a := testLoopAdapter(t)

	tests := []struct {
		name string
		fn   func() error
	}{
		{"ListSymbols", func() error { _, err := a.ListSymbols("x"); return err }},
		{"FindReferences", func() error { _, err := a.FindReferences("x"); return err }},
		{"FindDefinition", func() error { _, err := a.FindDefinition("x"); return err }},
		{"GetHoverInfo", func() error { _, err := a.GetHoverInfo("x"); return err }},
		{"GetDiagnostics", func() error { _, err := a.GetDiagnostics("x"); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if err == nil {
				t.Fatal("expected error")
			}
			kind, ok := LoopErrorKindOf(err)
			if !ok {
				t.Fatalf("error should be *LoopError, got %T: %v", err, err)
			}
			if kind != LoopErrNotConnected {
				t.Errorf("error kind = %s, want not_connected", kind)
			}
		})
	}
}

// --- Error type tests ---

func TestLoopError_ErrorsIs(t *testing.T) {
	err := &LoopError{
		Kind:      LoopErrSessionNotFound,
		Subsystem: "session",
		Message:   "not found",
	}

	// errors.As should work.
	var le *LoopError
	if !errors.As(err, &le) {
		t.Error("errors.As should match *LoopError")
	}
	if le.Kind != LoopErrSessionNotFound {
		t.Errorf("Kind = %s, want session_not_found", le.Kind)
	}
}

func TestLoopError_Unwrap(t *testing.T) {
	cause := errors.New("underlying cause")
	err := WrapLoopError(LoopErrSessionNotFound, "session", "wrap test", cause)
	if !errors.Is(err, cause) {
		t.Error("errors.Is should find the wrapped cause")
	}
}

// --- Backward compatibility: load old Session JSON without new fields ---

func TestSession_BackwardCompatJSON(t *testing.T) {
	// Simulate an old session JSON without Tags, PinnedIndices, etc.
	oldJSON := `{
		"id": "old-session",
		"messages": [],
		"created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-01T00:00:00Z"
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "old-session.json")
	if err := os.WriteFile(path, []byte(oldJSON), 0o644); err != nil {
		t.Fatalf("write old session: %v", err)
	}
	sess, err := LoadSession(dir, "old-session")
	if err != nil {
		t.Fatalf("LoadSession for old format: %v", err)
	}
	if sess.ID != "old-session" {
		t.Errorf("ID = %q, want 'old-session'", sess.ID)
	}
	// New fields should be zero-valued.
	if sess.TotalInputTokens != 0 {
		t.Error("old session should have zero TotalInputTokens")
	}
	if sess.Fork != nil {
		t.Error("old session should have nil Fork")
	}
}

// --- HandleSlashCommand integration ---

func TestConversationLoop_HandleSlashCommand(t *testing.T) {
	cfg := &Config{
		Model:      "claude-test",
		MaxTokens:  4096,
		SessionDir: t.TempDir(),
	}
	loop := &ConversationLoop{
		Config:  cfg,
		Session: NewSession(),
		Usage:   usage.NewTracker("claude-test"),
	}

	// Without a command registry, should return (false, nil).
	handled, err := loop.HandleSlashCommand("/help")
	if handled || err != nil {
		t.Errorf("without registry: handled=%v, err=%v", handled, err)
	}

	// Non-slash input should return (false, nil).
	handled, err = loop.HandleSlashCommand("just text")
	if handled || err != nil {
		t.Errorf("non-slash: handled=%v, err=%v", handled, err)
	}
}
